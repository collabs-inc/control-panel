package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// a photo as a phone takes it: stored on its side (red left half, blue right), EXIF saying to turn it 90° clockwise
func sidewaysJPEG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := range 200 {
		for x := range 400 {
			c := color.RGBA{220, 20, 20, 255}
			if x >= 200 {
				c = color.RGBA{20, 20, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0}
	app1 := append([]byte{0xFF, 0xE1, 0, byte(2 + 6 + len(tiff))}, append([]byte("Exif\x00\x00"), tiff...)...)
	return append(append([]byte{0xFF, 0xD8}, app1...), b.Bytes()[2:]...)
}

// what a thumbnail is: its format and size (WebP, which Go can't read, through identify)
func thumbOf(t *testing.T, b []byte) (string, int, int) {
	if img, format, err := image.Decode(bytes.NewReader(b)); err == nil {
		return format, img.Bounds().Dx(), img.Bounds().Dy()
	}
	id := exec.Command("identify", "-format", "%m %w %h", "-")
	id.Stdin = bytes.NewReader(b)
	out, err := id.Output()
	var format string
	var w, h int
	if _, err2 := fmt.Sscan(string(out), &format, &w, &h); err != nil || err2 != nil {
		t.Fatalf("not a picture: %v %q", err, b[:min(len(b), 16)])
	}
	return strings.ToLower(format), w, h
}

func TestFsThumbScalesTurnsAndPassesSmallOnes(t *testing.T) {
	defer func(d string) { fsThumbDir = d }(fsThumbDir)
	fsThumbDir = t.TempDir()
	for _, convert := range []string{fsConvert, ""} { // ImageMagick (if here) and the standard library
		if convert == "" && fsConvert == "" {
			continue
		}
		t.Run(map[bool]string{true: "imagemagick", false: "stdlib"}[convert != ""], func(t *testing.T) {
			defer func(c string) { fsConvert = c }(fsConvert)
			fsConvert = convert
			f := newFsEnv(t)
			photo := sidewaysJPEG(t)
			if o := fsOrientation(photo); o != 6 {
				t.Fatalf("orientation %d, want 6", o)
			}
			os.WriteFile(filepath.Join(f.home, "side.jpg"), photo, 0o644)
			r := f.do(t, "GET", "/api/fs/thumb?w=50&h=50&path="+q(filepath.Join(f.home, "side.jpg")), "", f.authed(f.tok))
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("X-Image-Size") != "200 × 400" {
				t.Fatalf("size header %q", r.Header.Get("X-Image-Size"))
			}
			img, format, err := image.Decode(bytes.NewReader(b))
			if err != nil || format != "jpeg" {
				t.Fatalf("%v %s", err, format)
			}
			if b := img.Bounds(); b.Dx() != 25 || b.Dy() != 50 { // 200×400 as shown, into 50×50
				t.Fatalf("thumbnail %v", b)
			}
			if r, _, b, _ := img.At(12, 5).RGBA(); r < b { // turned: the left half is now the top
				t.Fatal("top isn't red: not turned by its orientation")
			}
			if r, _, b, _ := img.At(12, 45).RGBA(); b < r {
				t.Fatal("bottom isn't blue")
			}

			// see-through stays so; a picture that already fits goes out as it is
			clear := image.NewNRGBA(image.Rect(0, 0, 300, 100))
			var pb bytes.Buffer
			png.Encode(&pb, clear)
			os.WriteFile(filepath.Join(f.home, "clear.png"), pb.Bytes(), 0o644)
			r = f.do(t, "GET", "/api/fs/thumb?w=30&h=30&path="+q(filepath.Join(f.home, "clear.png")), "", f.authed(f.tok))
			b, _ = io.ReadAll(r.Body)
			if format, w, h := thumbOf(t, b); format == "jpeg" || w != 30 || h != 10 {
				t.Fatalf("%s %dx%d: want a see-through 30x10", format, w, h)
			}
			r = f.do(t, "GET", "/api/fs/thumb?w=500&h=500&path="+q(filepath.Join(f.home, "clear.png")), "", f.authed(f.tok))
			if got, _ := io.ReadAll(r.Body); !bytes.Equal(got, pb.Bytes()) {
				t.Fatal("a picture that fits should be sent as it is")
			}
		})
	}
}

func TestFsThumbScalesWhatGoCantDecodeWithImageMagick(t *testing.T) {
	if _, err := exec.LookPath("convert"); err != nil {
		t.Skip("no ImageMagick here")
	}
	defer func(d string) { fsThumbDir = d }(fsThumbDir)
	fsThumbDir = t.TempDir()
	f := newFsEnv(t)
	big := filepath.Join(f.home, "big.webp")
	if out, err := exec.Command("convert", "-size", "1200x800", "gradient:red-blue", big).CombinedOutput(); err != nil {
		t.Skipf("this ImageMagick can't write WebP: %s", out)
	}
	r := f.do(t, "GET", "/api/fs/thumb?w=120&h=120&path="+q(big), "", f.authed(f.tok))
	b, _ := io.ReadAll(r.Body)
	if r.Header.Get("Content-Type") != "image/webp" || len(b) < 12 || string(b[8:12]) != "WEBP" {
		t.Fatalf("%s %q", r.Header.Get("Content-Type"), b[:min(len(b), 16)])
	}
	id := exec.Command("identify", "-format", "%wx%h", "webp:-")
	id.Stdin = bytes.NewReader(b)
	if out, err := id.Output(); err != nil || string(out) != "120x80" {
		t.Fatalf("scaled to %s (%v), want 120x80", out, err)
	}
	// made once: the next asks get it from disk
	if ds, _ := os.ReadDir(fsThumbDir); len(ds) != 1 || !strings.HasSuffix(ds[0].Name(), ".webp") {
		t.Fatalf("cache: %v", ds)
	}
	r = f.do(t, "GET", "/api/fs/thumb?w=120&h=120&path="+q(big), "", f.authed(f.tok))
	if b2, _ := io.ReadAll(r.Body); !bytes.Equal(b, b2) || r.Header.Get("Content-Type") != "image/webp" {
		t.Fatal("the second ask should be the same picture, from disk")
	}
	// a HEIC's tile makes its opened size in the same decode: opening it then comes from disk
	heic := filepath.Join(f.home, "IMG_0001.heic")
	if out, err := exec.Command("convert", "-size", "1200x800", "gradient:red-blue", heic).CombinedOutput(); err != nil {
		t.Skipf("this ImageMagick can't write HEIC: %s", out)
	}
	r = f.do(t, "GET", "/api/fs/thumb?w=60&h=60&fit=cover&pre=300x300&path="+q(heic), "", f.authed(f.tok))
	if _, w, h := thumbOf(t, mustRead(r.Body)); w != 90 || h != 60 {
		t.Fatalf("tile %dx%d, want 90x60", w, h)
	}
	ds, _ := os.ReadDir(fsThumbDir)
	if len(ds) != 3 {
		t.Fatalf("cache after the tile: %v (want the webp, the tile and the opened size)", ds)
	}
	r = f.do(t, "GET", "/api/fs/thumb?w=300&h=300&path="+q(heic), "", f.authed(f.tok))
	if _, w, h := thumbOf(t, mustRead(r.Body)); w != 300 || h != 200 {
		t.Fatalf("opened %dx%d, want 300x200", w, h)
	}
	if ds2, _ := os.ReadDir(fsThumbDir); len(ds2) != 3 {
		t.Fatalf("opening it made another: %v", ds2)
	}
}

func mustRead(r io.Reader) []byte { b, _ := io.ReadAll(r); return b }
