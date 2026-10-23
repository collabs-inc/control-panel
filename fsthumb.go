package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	fsThumbSem  = make(chan struct{}, map[bool]int{true: 8, false: 4}[fsConvert != ""]) // eight ImageMagicks at once; four Go decodes (~25 MB each)
	fsThumbDone = time.AfterFunc(time.Hour, debug.FreeOSMemory)                         // and the heap the Go decoder grew goes back after a burst
	// ImageMagick, when the box has it, scales in its own process: the panel's memory stays put, and a JPEG is decoded
	// straight at 1/2, 1/4 or 1/8 of its size (libjpeg), so a 12 MP photo's tile takes ~0.1 s, not ~0.2. Else the
	// standard library does JPEG, PNG and GIF here.
	fsConvert, _ = exec.LookPath("convert")
)

// fsMagick: formats only ImageMagick reads. HEIC is how iPhones save photos, and no browser but Safari shows it: this
// is how it's seen at all.
var fsMagick = map[string]string{".webp": "webp", ".avif": "avif", ".heic": "heic", ".heif": "heic"}

// fsThumb serves a JPEG, PNG or GIF (or the fsMagick formats) scaled down to a w×h box (fit=cover: to fill it), so a
// folder of photos isn't megabytes a tile and a phone isn't sent a camera's full resolution to show on a small screen.
// What already fits, or won't decode, goes out as it is (fsRaw).
func (s *server) fsThumb(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := s.fsPath(q.Get("path"), true)
	if err != nil {
		fsFail(w, err)
		return
	}
	bw, _ := strconv.Atoi(q.Get("w"))
	bh, _ := strconv.Atoi(q.Get("h"))
	bw, bh = min(max(bw, 16), 4096), min(max(bh, 16), 4096)
	cover := q.Get("fit") == "cover"
	fsThumbSem <- struct{}{}
	defer func() { <-fsThumbSem }()
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0) // a fifo can't block the open
	if err != nil {
		fsFail(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64<<20 {
		s.fsRaw(w, r)
		return
	}
	head := make([]byte, min(fi.Size(), 128<<10)) // the size and the EXIF orientation are in the first bytes
	if _, err := io.ReadFull(f, head); err != nil {
		s.fsRaw(w, r)
		return
	}
	coder := fsMagick[strings.ToLower(filepath.Ext(p))]
	cfg, format, err := image.DecodeConfig(bytes.NewReader(head))
	if err != nil && coder == "" && int64(len(head)) < fi.Size() { // its header further in (a large ICC profile, say)
		cfg, format, err = image.DecodeConfig(io.MultiReader(bytes.NewReader(head), f))
		f.Seek(int64(len(head)), io.SeekStart)
	}
	known := err == nil && coder == ""
	if known {
		coder = map[string]string{"jpeg": "jpg", "png": "png", "gif": "gif"}[format]
	}
	if coder == "" || known && cfg.Width*cfg.Height > 80e6 {
		s.fsRaw(w, r)
		return
	}
	o, dw, dh, tw, th := 1, 0, 0, 0, 0
	if known { // its size known: scaled exactly, or sent as it is if it already fits
		if format == "jpeg" {
			o = fsOrientation(head)
		}
		dw, dh = cfg.Width, cfg.Height // as shown: turned by the EXIF orientation, as browsers turn the original
		if o >= 5 {
			dw, dh = dh, dw
		}
		k := min(float64(bw)/float64(dw), float64(bh)/float64(dh))
		if cover {
			k = max(float64(bw)/float64(dw), float64(bh)/float64(dh))
		}
		if k >= 1 {
			s.fsRaw(w, r)
			return
		}
		tw, th = max(1, int(float64(dw)*k+0.5)), max(1, int(float64(dh)*k+0.5))
		w.Header().Set("X-Image-Size", fmt.Sprintf("%d × %d", dw, dh))
	}
	if fsConvert != "" {
		// made before, for this version of the file and this box: from disk (a HEIC takes ~1 s to decode; a phone and
		// a desktop opening the same folder, or a browser that dropped its cache, needn't wait twice)
		key := fsThumbKey(p, fi, bw, bh, cover)
		if b, ct, ok := fsThumbCached(key); ok {
			fsThumbHeaders(w, q.Get("v"), ct)
			w.Write(b)
			return
		}
		var args []string
		geo := fmt.Sprintf("%dx%d>", bw, bh) // only ever smaller
		if cover {
			geo = fmt.Sprintf("%dx%d^>", bw, bh)
		}
		if tw > 0 {
			geo = fmt.Sprintf("%dx%d!", tw, th)
			if format == "jpeg" { // decoded at the smallest 1/n still at least this big (as stored: before turning)
				sw, sh := tw, th
				if o >= 5 {
					sw, sh = th, tw
				}
				args = append(args, "-define", fmt.Sprintf("jpeg:size=%dx%d", sw, sh))
			}
		}
		in, out, ct := coder+":-", "webp:-", "image/webp" // WebP: small, and see-through stays so
		if coder == "gif" {
			in += "[0]" // its first frame
		}
		if format == "jpeg" {
			out, ct = "jpg:-", "image/jpeg"
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		scale := []string{"-filter", "Triangle", "-thumbnail", geo} // a JPEG comes decoded at about this size: a fine filter is cheap
		if format != "jpeg" {
			scale = []string{"-scale", geo, "-strip"} // decoded whole (12 MP): plain averaging, ~0.1 s where a filter took ~0.6
		}
		// ?pre=WxH (a HEIC's tile): the screen-sized copy its viewer will ask for, made from the same decode into the
		// cache, so opening it from its folder skips a second ~0.8 s decode
		var also []string
		pre := ""
		if w, h, ok := strings.Cut(q.Get("pre"), "x"); ok && (coder == "heic" || coder == "avif") {
			pw, _ := strconv.Atoi(w)
			ph, _ := strconv.Atoi(h)
			pw, ph = min(max(pw, 16), 4096), min(max(ph, 16), 4096) // as the viewer's own ask is held
			if pkey := fsThumbKey(p, fi, pw, ph, false); os.MkdirAll(fsThumbDir, 0o700) == nil {
				if _, err := os.Stat(filepath.Join(fsThumbDir, pkey+".webp")); err != nil {
					pre = filepath.Join(fsThumbDir, pkey)
					also = []string{"(", "+clone", "-scale", fmt.Sprintf("%dx%d>", pw, ph), "-strip", "-quality", "82", "-write", "webp:" + pre + ".tmp", "+delete", ")"}
				}
			}
		}
		cmd := exec.CommandContext(ctx, fsConvert, slices.Concat(args, []string{in, "-auto-orient"}, also, scale, []string{"-quality", "82", out})...)
		cmd.Stdin = io.MultiReader(bytes.NewReader(head), f) // never its name: ImageMagick reads meaning into names (globs, frames, coders)
		threads := "4"                                       // a HEIC or PNG is decoded and scaled whole: worth a few threads
		if format == "jpeg" {
			threads = "1" // decoded small already: one each, side by side
		}
		cmd.Env = append(os.Environ(), "MAGICK_THREAD_LIMIT="+threads)
		if b, err := cmd.Output(); err == nil && len(b) > 0 {
			fsThumbHeaders(w, q.Get("v"), ct)
			w.Write(b)
			fsThumbKeep(key, ct, b)
			if pre != "" {
				os.Rename(pre+".tmp", pre+".webp")
			}
			return
		}
		if !known { // nothing else here reads it
			s.fsRaw(w, r)
			return
		}
		f.Seek(int64(len(head)), io.SeekStart)
	}
	if !known {
		s.fsRaw(w, r)
		return
	}
	defer func() { runtime.GC(); fsThumbDone.Reset(3 * time.Second) }() // the next decode reuses this one's ~25 MB
	rest, err := io.ReadAll(f)
	if err != nil {
		s.fsRaw(w, r)
		return
	}
	img, _, err := image.Decode(io.MultiReader(bytes.NewReader(head), bytes.NewReader(rest)))
	if err != nil {
		s.fsRaw(w, r)
		return
	}
	dst := fsShrink(img, o, tw, th)
	var out bytes.Buffer
	ct := "image/jpeg"
	if format != "jpeg" && !dst.Opaque() { // see-through stays so
		ct = "image/png"
		(&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&out, dst)
	} else {
		jpeg.Encode(&out, dst, &jpeg.Options{Quality: 82})
	}
	fsThumbHeaders(w, q.Get("v"), ct)
	w.Write(out.Bytes())
}

// fsThumbDir keeps ImageMagick's results: ~15–150 KB each; what isn't used for 30 days goes.
var fsThumbDir = func() string { d, _ := os.UserCacheDir(); return filepath.Join(d, "control-panel", "thumbs") }()

func fsThumbKey(p string, fi os.FileInfo, w, h int, cover bool) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%dx%d\x00%v", p, fi.Size(), fi.ModTime().UnixNano(), w, h, cover))
	return hex.EncodeToString(sum[:16])
}

func fsThumbCached(key string) ([]byte, string, bool) {
	for ext, ct := range map[string]string{".jpg": "image/jpeg", ".webp": "image/webp"} {
		f := filepath.Join(fsThumbDir, key+ext)
		if b, err := os.ReadFile(f); err == nil {
			now := time.Now()
			os.Chtimes(f, now, now) // still in use
			return b, ct, true
		}
	}
	return nil, "", false
}

func fsThumbKeep(key, ct string, b []byte) {
	ext := map[string]string{"image/jpeg": ".jpg", "image/webp": ".webp"}[ct]
	if os.MkdirAll(fsThumbDir, 0o700) != nil {
		return
	}
	tmp := filepath.Join(fsThumbDir, key+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(fsThumbDir, key+ext))
	}
	if rand.IntN(200) == 0 { // now and then, what hasn't been used for 30 days goes
		ds, _ := os.ReadDir(fsThumbDir)
		for _, d := range ds {
			if i, err := d.Info(); err == nil && time.Since(i.ModTime()) > 30*24*time.Hour {
				os.Remove(filepath.Join(fsThumbDir, d.Name()))
			}
		}
	}
}

func fsThumbHeaders(w http.ResponseWriter, v, ct string) {
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "private, max-age=86400") // the page puts the file's mtime in the URL (v)
	if v == "" {
		h.Set("Cache-Control", "private, no-cache")
	}
	h.Set("Content-Security-Policy", "frame-ancestors 'self'")
}

// fsShrink scales img, turned by EXIF orientation o, to tw×th: each pixel the average of the box of source pixels
// under it (up to 6×6 samples of it: a photo's planes are read directly, anything else through At).
func fsShrink(img image.Image, o, tw, th int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if o >= 5 {
		dw, dh = sh, sw
	}
	var px func(x, y int) (r, g, bl, a uint32) // premultiplied, 0–255, at a source point
	switch m := img.(type) {
	case *image.YCbCr:
		px = func(x, y int) (uint32, uint32, uint32, uint32) {
			yi, ci := m.YOffset(x, y), m.COffset(x, y)
			r, g, bl := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
			return uint32(r), uint32(g), uint32(bl), 255
		}
	case *image.NRGBA:
		px = func(x, y int) (uint32, uint32, uint32, uint32) {
			p := m.Pix[m.PixOffset(x, y):]
			a := uint32(p[3])
			return uint32(p[0]) * a / 255, uint32(p[1]) * a / 255, uint32(p[2]) * a / 255, a
		}
	case *image.RGBA:
		px = func(x, y int) (uint32, uint32, uint32, uint32) {
			p := m.Pix[m.PixOffset(x, y):]
			return uint32(p[0]), uint32(p[1]), uint32(p[2]), uint32(p[3])
		}
	default:
		px = func(x, y int) (uint32, uint32, uint32, uint32) {
			r, g, bl, a := m.At(x, y).RGBA()
			return r >> 8, g >> 8, bl >> 8, a >> 8
		}
	}
	at := func(x, y int) (int, int) { // a shown point → where it is in the source
		switch o {
		case 2:
			return sw - 1 - x, y
		case 3:
			return sw - 1 - x, sh - 1 - y
		case 4:
			return x, sh - 1 - y
		case 5:
			return y, x
		case 6:
			return y, sh - 1 - x
		case 7:
			return sw - 1 - y, sh - 1 - x
		case 8:
			return sw - 1 - y, x
		}
		return x, y
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := range th {
		y0, y1 := y*dh/th, max((y+1)*dh/th, y*dh/th+1)
		sy := max(1, (y1-y0+5)/6)
		for x := range tw {
			x0, x1 := x*dw/tw, max((x+1)*dw/tw, x*dw/tw+1)
			sx := max(1, (x1-x0+5)/6)
			var r, g, bl, a, n uint32
			for yy := y0; yy < y1; yy += sy {
				for xx := x0; xx < x1; xx += sx {
					ax, ay := at(xx, yy)
					pr, pg, pb, pa := px(b.Min.X+ax, b.Min.Y+ay)
					r, g, bl, a, n = r+pr, g+pg, bl+pb, a+pa, n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// fsOrientation finds a JPEG's EXIF orientation (1–8; 1 when it has none).
func fsOrientation(b []byte) int {
	for i := 2; i+4 <= len(b) && b[i] == 0xFF; {
		m, n := b[i+1], int(binary.BigEndian.Uint16(b[i+2:]))
		if m == 0xDA || n < 2 || i+2+n > len(b) { // the image data: no EXIF after it
			break
		}
		if m == 0xE1 && n >= 16 && string(b[i+4:i+10]) == "Exif\x00\x00" {
			t := b[i+10 : i+2+n] // TIFF: byte order, then the first directory's offset
			var bo binary.ByteOrder = binary.BigEndian
			if string(t[:2]) == "II" {
				bo = binary.LittleEndian
			}
			d := int(bo.Uint32(t[4:]))
			if d < 0 || d+2 > len(t) {
				break
			}
			for e, c := d+2, int(bo.Uint16(t[d:])); c > 0 && e+12 <= len(t); e, c = e+12, c-1 {
				if bo.Uint16(t[e:]) == 0x0112 {
					if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
						return v
					}
				}
			}
			break
		}
		i += 2 + n
	}
	return 1
}
