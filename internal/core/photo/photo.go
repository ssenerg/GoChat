// Package photo turns uploaded pictures into small square images that are
// safe to serve: decoded within limits, cropped, resized, turned upright and
// re-encoded, which drops metadata and anything else hidden in the original.
package photo

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif" // decoders for uploads
	"image/jpeg"
	"image/png"
	"io"
	"regexp"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decoder for uploads
)

const (
	// Size is the side of every stored photo, in pixels.
	Size = 256
	// MaxUpload bounds an uploaded picture, and MaxStored a processed one.
	MaxUpload = 10 << 20
	MaxStored = 512 << 10
	// maxPixels bounds the decoded picture, about 96MB of RGBA.
	maxPixels = 24_000_000
)

var (
	ErrUnsupported = errors.New("use a JPEG, PNG, GIF or WebP picture")
	ErrTooLarge    = errors.New("the picture is too large, use one under 10 MB and 24 megapixels")
)

// Process reads an uploaded picture and returns the photo to store.
func Process(r io.Reader) ([]byte, string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxUpload+1))
	if err != nil {
		return nil, "", err
	}
	img, format, err := decode(raw)
	if err != nil {
		return nil, "", err
	}
	out := image.NewRGBA(image.Rect(0, 0, Size, Size))
	draw.CatmullRom.Scale(out, out.Bounds(), img, centerSquare(img.Bounds()), draw.Src, nil)
	if format == "jpeg" {
		// The center square turns with the picture, so orienting the small
		// result is enough.
		out = orient(out, jpegOrientation(raw))
	}
	return encode(out)
}

// Fit turns a picture into one that fits within width by height, scaled
// down when larger, upright and encoded again, as Process does. It returns
// the result's content type and size.
func Fit(raw []byte, width, height int) ([]byte, string, image.Point, error) {
	img, format, err := decode(raw)
	if err != nil {
		return nil, "", image.Point{}, err
	}
	o := 1
	if format == "jpeg" {
		o = jpegOrientation(raw)
	}
	b := img.Bounds()
	// The size upright, scaled to fit, then as stored before turning.
	w, h := b.Dx(), b.Dy()
	if o >= 5 {
		w, h = h, w
	}
	scale := min(1, float64(width)/float64(w), float64(height)/float64(h))
	w, h = max(1, int(float64(w)*scale+0.5)), max(1, int(float64(h)*scale+0.5))
	if o >= 5 {
		w, h = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(out, out.Bounds(), img, b, draw.Src, nil)
	out = orient(out, o)
	data, ctype, err := encode(out)
	return data, ctype, out.Bounds().Size(), err
}

// decode reads a picture of an allowed format within the limits.
func decode(raw []byte) (image.Image, string, error) {
	if len(raw) > MaxUpload {
		return nil, "", ErrTooLarge
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrUnsupported
	}
	switch format {
	case "jpeg", "png", "gif", "webp":
	default:
		return nil, "", ErrUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, "", ErrTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrUnsupported
	}
	return img, format, nil
}

func centerSquare(b image.Rectangle) image.Rectangle {
	side := min(b.Dx(), b.Dy())
	x, y := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2
	return image.Rect(x, y, x+side, y+side)
}

func encode(img *image.RGBA) ([]byte, string, error) {
	var buf bytes.Buffer
	if img.Opaque() {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}

// Check accepts only a processed photo: a JPEG or PNG of Size by Size. It
// returns its content type.
func Check(data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxStored {
		return "", ErrTooLarge
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") || cfg.Width != Size || cfg.Height != Size {
		return "", ErrUnsupported
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return "", ErrUnsupported
	}
	return "image/" + format, nil
}

// Version names a photo by its content.
func Version(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

var versionRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// jpegOrientation reads the EXIF orientation of a JPEG, 1 when it has none.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xff {
			return 1
		}
		marker := b[i+1]
		if marker == 0xda || marker == 0xd9 {
			return 1
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+n]
		if marker == 0xe1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 1
	}
	count := int(bo.Uint16(t[ifd:]))
	for e := range count {
		p := ifd + 2 + e*12
		if p+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[p:]) == 0x0112 {
			if v := int(bo.Uint16(t[p+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient turns an image upright for an EXIF orientation; 5 to 8 swap its
// sides.
func orient(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			copy(dst.Pix[dst.PixOffset(x, y):dst.PixOffset(x, y)+4], src.Pix[src.PixOffset(sx, sy):src.PixOffset(sx, sy)+4])
		}
	}
	return dst
}
