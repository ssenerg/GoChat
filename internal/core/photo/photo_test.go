package photo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func picture(w, h int, fill func(x, y int) color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, fill(x, y))
		}
	}
	return img
}

func encoded(t *testing.T, img image.Image, asPNG bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var err error
	if asPNG {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcess(t *testing.T) {
	opaque := picture(640, 400, func(x, _ int) color.Color { return color.RGBA{uint8(x), 120, 200, 255} })
	data, ctype, err := Process(bytes.NewReader(encoded(t, opaque, true)))
	if err != nil || ctype != "image/jpeg" {
		t.Fatalf("opaque: %q %v", ctype, err)
	}
	if got, err := Check(data); err != nil || got != "image/jpeg" {
		t.Fatalf("check: %q %v", got, err)
	}
	seeThrough := picture(300, 300, func(int, int) color.Color { return color.NRGBA{10, 20, 30, 100} })
	if _, ctype, err := Process(bytes.NewReader(encoded(t, seeThrough, true))); err != nil || ctype != "image/png" {
		t.Fatalf("transparent: %q %v", ctype, err)
	}
}

func TestProcessRefuses(t *testing.T) {
	if _, _, err := Process(bytes.NewReader([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"))); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("svg: %v", err)
	}
	// A PNG header claiming a huge image is refused before decoding.
	huge := encoded(t, picture(1, 1, func(int, int) color.Color { return color.White }), true)
	binary.BigEndian.PutUint32(huge[16:], 20000)
	binary.BigEndian.PutUint32(huge[20:], 20000)
	binary.BigEndian.PutUint32(huge[29:], crc32.ChecksumIEEE(huge[12:29]))
	if _, _, err := Process(bytes.NewReader(huge)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("huge: %v", err)
	}
	if _, _, err := Process(bytes.NewReader(make([]byte, MaxUpload+1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("long: %v", err)
	}
	big := encoded(t, picture(300, 300, func(int, int) color.Color { return color.White }), true)
	if _, err := Check(big); err == nil {
		t.Fatal("check accepted an unprocessed picture")
	}
}

// withOrientation inserts an EXIF segment with orientation o after the SOI.
func withOrientation(jpg []byte, o uint16) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(o), 0, 0, 0, 0, 0, 0, 0}
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(len(seg)+2))
	out := append([]byte{0xff, 0xd8}, app1...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

func TestOrientation(t *testing.T) {
	// Red on the left: turned clockwise, red is on top.
	src := picture(200, 200, func(x, _ int) color.Color {
		if x < 100 {
			return color.RGBA{220, 30, 30, 255}
		}
		return color.RGBA{30, 30, 220, 255}
	})
	data, _, err := Process(bytes.NewReader(withOrientation(encoded(t, src, false), 6)))
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	top, _, _, _ := img.At(Size/2, Size/8).RGBA()
	bottom, _, _, _ := img.At(Size/2, Size*7/8).RGBA()
	if top>>8 < 150 || bottom>>8 > 100 {
		t.Fatalf("not turned upright: top red %d, bottom red %d", top>>8, bottom>>8)
	}
}

func TestVersion(t *testing.T) {
	v := Version([]byte("x"))
	if !ValidVersion(v) || ValidVersion("../etc") || ValidVersion(v+"0") {
		t.Fatal("version shape")
	}
}

func TestFit(t *testing.T) {
	wide := picture(800, 400, func(x, _ int) color.Color { return color.RGBA{uint8(x), 90, 160, 255} })
	data, ctype, size, err := Fit(encoded(t, wide, true), 400, 400)
	if err != nil || ctype != "image/jpeg" || size != image.Pt(400, 200) {
		t.Fatalf("wide: %q %v %v", ctype, size, err)
	}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width != 400 || cfg.Height != 200 {
		t.Fatalf("stored %+v %v", cfg, err)
	}
	small := picture(100, 50, func(int, int) color.Color { return color.NRGBA{10, 20, 30, 100} })
	if _, ctype, size, err := Fit(encoded(t, small, true), 400, 400); err != nil || ctype != "image/png" || size != image.Pt(100, 50) {
		t.Fatalf("small: %q %v %v", ctype, size, err)
	}
	// Red on the left of a wide picture turned clockwise: a tall one, red on top.
	src := picture(300, 100, func(x, _ int) color.Color {
		if x < 100 {
			return color.RGBA{220, 30, 30, 255}
		}
		return color.RGBA{30, 30, 220, 255}
	})
	data, _, size, err = Fit(withOrientation(encoded(t, src, false), 6), 640, 640)
	if err != nil || size != image.Pt(100, 300) {
		t.Fatalf("turned: %v %v", size, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	top, _, _, _ := img.At(50, 30).RGBA()
	bottom, _, _, _ := img.At(50, 270).RGBA()
	if top>>8 < 150 || bottom>>8 > 100 {
		t.Fatalf("not turned upright: top red %d, bottom red %d", top>>8, bottom>>8)
	}
	if _, _, _, err := Fit([]byte("<svg/>"), 100, 100); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("svg: %v", err)
	}
}
