package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"time"
)

// Patches: a page that changes a little now and then (a caret blinking, a hover, a keystroke, a counter) shouldn't
// cost a whole frame each time, nor blur: the stream moves at 1x and sharpens at rest (browser.go), so every such
// change would flicker from sharp to soft and back. When a frame comes on its own (brSparse since the last one), it's
// compared with the whole frame before it (what the page shows, at whatever density); if what changed is small, only
// that rectangle is captured, sharp, and sent as a patch (kind 4) that the page draws over its canvas. A caret's
// blink is then a few KB instead of ~400. The rectangle is cut from a whole capture (see patch).

const (
	brSparse    = 60 * time.Millisecond // frames further apart than this are changes, not motion (scrolling is ~30 fps)
	brPatchMax  = 0.2                   // a patch covers at most this part of the page; past it, a whole frame
	brPatchPNG  = 60000                 // CSS px² under which a patch goes as PNG (lossless, small for small areas)
	brBlockSize = 16                    // the comparison's blocks (a JPEG's 4:2:0 MCU)
)

type bpend struct {
	img         []byte
	dw, dh, top float64
}

// frame takes a screencast frame: a patch when it can be, else the whole frame.
func (b *browser) frame(t *btab, img []byte, num int, dw, dh, top float64) {
	// a patch's clipped capture makes headless Chromium send a frame of the clip's size: not the page, dropped
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(img)); err == nil && cfg.Height > 0 {
		b.mu.Lock()
		cw, ch := t.cssW, t.cssH
		b.mu.Unlock()
		if cw > 0 && ch > 0 && math.Abs(float64(cfg.Width)/float64(cfg.Height)-cw/ch) > 0.03*cw/ch {
			t.src.cdp.fire(t.sess, "Page.screencastFrameAck", map[string]any{"sessionId": num})
			return
		}
	}
	b.mu.Lock()
	now := time.Now()
	if now.Sub(t.lastMotion) < brSparse { // a few quick frames in a row is motion; one quick pair isn't (a caret's
		t.rapid++ // blink comes as two frames ~30 ms apart)
	} else {
		t.rapid = 0
	}
	sparse := t.rapid < 3
	t.lastMotion = now
	if t.patching { // one is being made: this frame goes as it is when it's done
		t.pend = &bpend{img, dw, dh, top}
		b.mu.Unlock()
		t.src.cdp.fire(t.sess, "Page.screencastFrameAck", map[string]any{"sessionId": num})
		return
	}
	// the owner's tabs only: a clipped capture briefly resizes the page's view, and an agent's page is under test
	if sparse && t.src.mine() && t.casting == "full" && t.seq > 0 && t.prevJPEG != nil && t.dialog == nil {
		t.patching = true
		prev, prevDec := t.prevJPEG, t.prevDec
		b.mu.Unlock()
		t.src.cdp.fire(t.sess, "Page.screencastFrameAck", map[string]any{"sessionId": num})
		go b.patch(t, prev, prevDec, img, dw, dh, top)
		return
	}
	b.mu.Unlock()
	b.deliver(t, img, num, dw, dh, top)
}

// deliver hands a whole frame to the viewers showing the tab (and the cards showing it small), acknowledges it to
// Chromium when one of them has room (num 0: already acknowledged), and arms the sharp frame for when it settles.
func (b *browser) deliver(t *btab, img []byte, num int, dw, dh, top float64) {
	b.mu.Lock()
	t.seq++
	t.wholeSeq = t.seq
	t.prevJPEG, t.prevDec = img, nil
	if t.cssW > 0 {
		dw, dh = t.cssW, t.cssH
	}
	meta, _ := json.Marshal(map[string]any{"tab": t.id, "seq": t.seq, "dw": dw, "dh": dh, "top": top})
	f := &bframe{seq: t.seq, img: img, meta: meta}
	t.frame, t.stale = f, false
	room := false
	for v := range b.viewers {
		if v.tab == t.id {
			v.frame, v.patches = f, nil
			if v.inflight < brFramesInFlight {
				room = true
			}
			v.kick()
		} else if v.thumbs[t.id] {
			if time.Since(v.thumbAt[t.id]) > 400*time.Millisecond {
				v.thumbQ[t.id] = f
				v.kick()
			}
		}
	}
	room = room || t.casting == "thumb" || t.casting == "record"
	if num != 0 && !room {
		t.ackHeld = append(t.ackHeld, num)
	}
	var rec *btrail // an agent's tab it works in: its trail keeps the motion
	if !t.src.mine() && t.src.cur == t {
		rec = t.src.agent.trail
	}
	if t.casting == "full" {
		whole := t.wholeSeq
		if t.sharpT != nil {
			t.sharpT.Stop()
		}
		t.sharpT = time.AfterFunc(brSharpAfter, func() { b.sharpen(t, whole) })
	}
	b.mu.Unlock()
	if room && num != 0 {
		t.src.cdp.fire(t.sess, "Page.screencastFrameAck", map[string]any{"sessionId": num})
	}
	rec.frame(img)
}

func (b *browser) patch(t *btab, prev []byte, prevDec *image.YCbCr, img []byte, dw, dh, top float64) {
	var cur *image.YCbCr
	whole := true
	defer func() {
		b.mu.Lock()
		p := t.pend
		t.pend = nil
		if !whole {
			t.prevJPEG, t.prevDec = img, cur
		}
		t.patching = p != nil && !whole
		b.mu.Unlock()
		if whole {
			b.deliver(t, img, 0, dw, dh, top)
			if p != nil { // and the frame that came meanwhile, as it is: the page is moving
				b.deliver(t, p.img, 0, p.dw, p.dh, p.top)
			}
		} else if p != nil { // a frame that came meanwhile: compared with this one, in turn
			go b.patch(t, img, cur, p.img, p.dw, p.dh, p.top)
		}
	}()
	a := prevDec
	if a == nil {
		a = decodeYCbCr(prev)
	}
	cur = decodeYCbCr(img)
	if a == nil || cur == nil {
		return
	}
	r, ok := dirtyRect(a, cur)
	if !ok {
		return
	}
	if r.Empty() { // the same picture again
		whole = false
		return
	}
	b.mu.Lock()
	cw, ch := t.cssW, t.cssH
	b.mu.Unlock()
	if cw <= 0 {
		cw, ch = float64(cur.Rect.Dx()), float64(cur.Rect.Dy())
	}
	// the frame's pixels are CSS px at the stream's 1x (maxWidth = the viewport): scale in case it was capped
	kx, ky := cw/float64(cur.Rect.Dx()), ch/float64(cur.Rect.Dy())
	x0, y0 := math.Max(0, math.Floor(float64(r.Min.X)*kx)-2), math.Max(0, math.Floor(float64(r.Min.Y)*ky)-2)
	x1, y1 := math.Min(cw, math.Ceil(float64(r.Max.X)*kx)+2), math.Min(ch, math.Ceil(float64(r.Max.Y)*ky)+2)
	// the whole viewport, cropped here: a clipped capture would briefly shrink the page's view to the clip, and a
	// click arriving then would miss
	var out struct {
		Data []byte `json:"data"`
	}
	if os.Getenv("BR_NOCAP") != "" {
		return
	}
	if t.src.cdp.callT(3*time.Second, t.sess, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 92, "optimizeForSpeed": true}, &out) != nil || len(out.Data) == 0 {
		return
	}
	full, err := jpeg.Decode(bytes.NewReader(out.Data))
	if err != nil {
		return
	}
	k := float64(full.Bounds().Dx()) / cw // the capture's pixels per CSS px: the page's emulated density
	crop := image.Rect(int(x0*k), int(y0*k), int(math.Ceil(x1*k)), int(math.Ceil(y1*k))).Intersect(full.Bounds())
	sub, ok := full.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok || crop.Empty() {
		return
	}
	var enc bytes.Buffer
	format := "png"
	if (x1-x0)*(y1-y0) > brPatchPNG {
		format = "jpeg"
		err = jpeg.Encode(&enc, sub.SubImage(crop), &jpeg.Options{Quality: 90})
	} else {
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&enc, sub.SubImage(crop))
	}
	if err != nil {
		return
	}
	out.Data = enc.Bytes()
	b.mu.Lock()
	t.seq++
	t.stale = true // t.frame is behind what's on screen now: a viewer that comes next gets a fresh capture
	meta, _ := json.Marshal(map[string]any{"tab": t.id, "seq": t.seq, "x": x0, "y": y0, "w": x1 - x0, "h": y1 - y0, "dw": cw, "dh": ch, "fmt": format})
	f := &bframe{seq: t.seq, img: out.Data, meta: meta}
	resync := false
	for v := range b.viewers {
		if v.tab == t.id {
			if len(v.patches) >= 8 { // a viewer that can't keep up: its patches go, a whole sharp frame replaces them
				v.patches, resync = nil, true
				continue
			}
			v.patches = append(v.patches, f)
			v.kick()
		}
	}
	whole0 := t.wholeSeq
	b.mu.Unlock()
	whole = false
	if resync {
		go b.sharpen(t, whole0, true)
	}
}

func decodeYCbCr(b []byte) *image.YCbCr {
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	y, _ := img.(*image.YCbCr)
	return y
}

// dirtyRect: the box around every block that differs between two frames, in pixels; not ok when the frames
// aren't comparable or the box is too big to be worth a patch.
func dirtyRect(a, c *image.YCbCr) (image.Rectangle, bool) {
	if a.Rect != c.Rect || a.SubsampleRatio != c.SubsampleRatio || a.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		return image.Rectangle{}, false
	}
	w, h := c.Rect.Dx(), c.Rect.Dy()
	var box image.Rectangle
	for by := 0; by < h; by += brBlockSize {
		for bx := 0; bx < w; bx += brBlockSize {
			if blockDiffers(a, c, bx, by, min(bx+brBlockSize, w), min(by+brBlockSize, h)) {
				box = box.Union(image.Rect(bx, by, min(bx+brBlockSize, w), min(by+brBlockSize, h)))
			}
		}
	}
	if float64(box.Dx()*box.Dy()) > brPatchMax*float64(w*h) {
		return box, false
	}
	return box, true
}

// blockDiffers: a block's luma or chroma moved by more than a JPEG's re-encoding noise
func blockDiffers(a, c *image.YCbCr, x0, y0, x1, y1 int) bool {
	for y := y0; y < y1; y++ {
		ra, rc := a.Y[y*a.YStride+x0:y*a.YStride+x1], c.Y[y*c.YStride+x0:y*c.YStride+x1]
		for i := range ra {
			if d := int(ra[i]) - int(rc[i]); d > 6 || d < -6 {
				return true
			}
		}
	}
	for y := y0 / 2; y < (y1+1)/2; y++ {
		for x := x0 / 2; x < (x1+1)/2; x++ {
			i, j := y*a.CStride+x, y*c.CStride+x
			if i >= len(a.Cb) || j >= len(c.Cb) {
				continue
			}
			if d := int(a.Cb[i]) - int(c.Cb[j]); d > 6 || d < -6 {
				return true
			}
			if d := int(a.Cr[i]) - int(c.Cr[j]); d > 6 || d < -6 {
				return true
			}
		}
	}
	return false
}
