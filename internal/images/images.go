// Package images checks and re-encodes the screenshots members attach
// (spec §11.4) and the committee's account photos: type read from the
// content, dimensions checked before any decoding, and a fresh encoding that
// leaves every metadata behind.
package images

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers WebP for image.Decode
)

// Limits of one screenshot (spec §3.1).
const (
	MaxBytes  = 5 << 20
	MaxPixels = 40_000_000
)

const (
	mimePNG     = "image/png"
	mimeJPEG    = "image/jpeg"
	jpegQuality = 85
)

// Refusals, each worth its own message to the member.
var (
	ErrNotImage      = errors.New("not a PNG, JPEG or WebP image")
	ErrTooBig        = errors.New("image file larger than 5 MB")
	ErrTooManyPixels = errors.New("image larger than 40 million pixels")
)

// ponytail: one decode at a time bounds memory to a single 40 Mpx image
// (about 320 MB: a 16-bit PNG decodes to 8 bytes per pixel); a per-request memory budget if uploads become frequent.
var decodeMu sync.Mutex

// Sanitize checks and re-encodes a screenshot: PNG stays PNG, JPEG and WebP
// become JPEG; metadata is dropped. mime is "image/png" or "image/jpeg".
//
// ponytail: EXIF orientation is not applied; screenshots carry none. Rotate
// from the EXIF tag before encoding if members start sending photos.
func Sanitize(data []byte) (out []byte, mime string, err error) {
	decodeMu.Lock()
	defer decodeMu.Unlock()
	img, format, err := decode(data)
	if err != nil {
		return nil, "", err
	}
	if format == "png" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("encode png: %w", err)
		}
		return buf.Bytes(), mimePNG, nil
	}
	out, err = encodeJPEG(img)
	return out, mimeJPEG, err
}

// AvatarSize is the side of an account photo in pixels: three times its
// largest display (40 px), for phone screens.
const AvatarSize = 128

// Avatar checks a photo as Sanitize does, turns it upright from its EXIF
// orientation, keeps the centred square and scales it to AvatarSize. The
// result is a JPEG without metadata; transparency becomes white.
//
// ponytail: orientation is read from JPEG only, where phones write it; read
// the WebP EXIF chunk if photos come out sideways.
func Avatar(data []byte) ([]byte, error) {
	decodeMu.Lock()
	defer decodeMu.Unlock()
	img, _, err := decode(data)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	side := min(b.Dx(), b.Dy())
	x0, y0 := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2
	square := image.Rect(x0, y0, x0+side, y0+side)
	small := image.NewRGBA(image.Rect(0, 0, AvatarSize, AvatarSize))
	draw.Draw(small, small.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(small, small.Bounds(), img, square, draw.Over, nil)
	return encodeJPEG(upright(small, orientation(data)))
}

// decode checks data and decodes it. format is "png", "jpeg" or "webp".
// The caller holds decodeMu.
func decode(data []byte) (img image.Image, format string, err error) {
	if len(data) > MaxBytes {
		return nil, "", ErrTooBig
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "webp") {
		return nil, "", ErrNotImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, "", ErrTooManyPixels
	}
	if img, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return nil, "", ErrNotImage
	}
	return img, format, nil
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// orientation reads the EXIF orientation of a JPEG (1 to 8); 1, upright,
// when data is no JPEG or carries none.
func orientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	// The segments before the image data: marker, length, payload.
	for i := 2; i+4 <= len(data) && data[i] == 0xFF; {
		marker, n := data[i+1], int(binary.BigEndian.Uint16(data[i+2:]))
		if marker == 0xDA || n < 2 || i+2+n > len(data) { // start of scan, or broken
			return 1
		}
		if seg := data[i+4 : i+2+n]; marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

// tiffOrientation reads tag 0x0112 of the first IFD of an EXIF TIFF block.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(t[:4]) {
	case "II\x2a\x00":
		order = binary.LittleEndian
	case "MM\x00\x2a":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(t[4:]))
	if ifd+2 > len(t) {
		return 1
	}
	for i := range int(order.Uint16(t[ifd:])) {
		e := ifd + 2 + 12*i
		if e+12 > len(t) {
			break
		}
		if order.Uint16(t[e:]) == 0x0112 {
			return int(order.Uint16(t[e+8:])) // upright ignores values out of 1..8
		}
	}
	return 1
}

// upright applies EXIF orientation o to a square image.
func upright(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	// Mirror, flip, then swap the axes: the stored pixel shown at (x, y).
	mirror, flip, swap := o == 2 || o == 3 || o == 6 || o == 7, o == 3 || o == 4 || o == 7 || o == 8, o >= 5
	dst := image.NewRGBA(src.Bounds())
	last := src.Bounds().Dx() - 1
	for y := range last + 1 {
		for x := range last + 1 {
			sx, sy := x, y
			if mirror {
				sx = last - x
			}
			if flip {
				sy = last - y
			}
			if swap {
				sx, sy = sy, sx
			}
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
}
