// Package images checks and re-encodes the screenshots members attach
// (spec §11.4): type read from the content, dimensions checked before any
// decoding, and a fresh encoding that leaves every metadata behind.
package images

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"sync"

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

	decodeMu.Lock()
	defer decodeMu.Unlock()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrNotImage
	}
	var buf bytes.Buffer
	if format == "png" {
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("encode png: %w", err)
		}
		return buf.Bytes(), mimePNG, nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), mimeJPEG, nil
}
