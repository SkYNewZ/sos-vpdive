package images

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tinyWebP is a 4×3 lossless WebP made with `cwebp -lossless`.
const tinyWebP = "UklGRjgAAABXRUJQVlA4TCsAAAAvA4AAAF9AkG0zxCLc3+MOwzPI89+DDLBcAIIAEqUUUhhuhRVGjeh/UJcDAA=="

func testImage(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// jpegWithExif encodes img as a JPEG and inserts an APP1 "Exif" segment of
// that payload, as phones do.
func jpegWithExif(t *testing.T, img image.Image, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	payload = append([]byte("Exif\x00\x00"), payload...)
	segment := binary.BigEndian.AppendUint16([]byte{0xFF, 0xE1}, uint16(len(payload)+2))
	segment = append(segment, payload...)
	raw := buf.Bytes()
	return append(append(append([]byte{}, raw[:2]...), segment...), raw[2:]...)
}

// exifOrientation is a TIFF block of one IFD holding the orientation tag.
func exifOrientation(order binary.AppendByteOrder, o uint16) []byte {
	b := []byte("MM\x00\x2a")
	if order == binary.LittleEndian {
		b = []byte("II\x2a\x00")
	}
	b = order.AppendUint32(b, 8)      // IFD0 offset
	b = order.AppendUint16(b, 1)      // one entry
	b = order.AppendUint16(b, 0x0112) // Orientation
	b = order.AppendUint16(b, 3)      // SHORT
	b = order.AppendUint32(b, 1)      // count
	b = order.AppendUint16(b, o)
	b = append(b, 0, 0)             // value padding
	return order.AppendUint32(b, 0) // no next IFD
}

// halves is a w×h image, red on its left half and blue on its right.
func halves(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			c := color.NRGBA{R: 220, B: 20, A: 255}
			if x >= w/2 {
				c = color.NRGBA{R: 20, B: 220, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// pngHeader is a PNG signature and IHDR chunk announcing w×h, without any
// pixel data: only the header can be read.
func pngHeader(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)))
	chunk = append(chunk, "IHDR"...)
	chunk = append(chunk, ihdr...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	return append([]byte("\x89PNG\r\n\x1a\n"), chunk...)
}

func TestSanitizeKeepsPNG(t *testing.T) {
	out, mime, err := Sanitize(encodePNG(t, testImage(30, 20)))
	require.NoError(t, err)
	assert.Equal(t, "image/png", mime)
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, [2]int{30, 20}, [2]int{cfg.Width, cfg.Height})
}

func TestSanitizeDropsJPEGMetadata(t *testing.T) {
	in := jpegWithExif(t, testImage(16, 8), []byte("GPS 43.08N 6.02E serial 12345"))
	require.True(t, bytes.Contains(in, []byte("Exif")))

	out, mime, err := Sanitize(in)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", mime)
	assert.False(t, bytes.Contains(out, []byte("Exif")))
	assert.False(t, bytes.Contains(out, []byte("GPS")))
}

func TestSanitizeTurnsWebPIntoJPEG(t *testing.T) {
	in, err := base64.StdEncoding.DecodeString(tinyWebP)
	require.NoError(t, err)

	out, mime, err := Sanitize(in)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", mime)
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, [2]int{4, 3}, [2]int{cfg.Width, cfg.Height})
}

func TestSanitizeRefusals(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"pdf", []byte("%PDF-1.7\n1 0 obj << >> endobj"), ErrNotImage},
		{"text named .png", []byte("hello, this is not an image"), ErrNotImage},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), ErrNotImage},
		{"truncated png", pngHeader(10, 10), ErrNotImage},
		{"over 40 Mpx, header only", pngHeader(8000, 5001), ErrTooManyPixels},
		{"zero width", pngHeader(0, 10), ErrNotImage},
		{"over 5 MB", append(pngHeader(10, 10), make([]byte, MaxBytes)...), ErrTooBig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Sanitize(tt.data)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestAvatarCropsAndScales(t *testing.T) {
	out, err := Avatar(encodePNG(t, testImage(300, 200)))
	require.NoError(t, err)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	assert.Equal(t, [2]int{AvatarSize, AvatarSize}, [2]int{cfg.Width, cfg.Height})
}

func TestAvatarTurnsPhotoUpright(t *testing.T) {
	// The stored pixels are red on the left; the orientation tag says how
	// they are shown, so where red lands tells what was applied.
	tests := []struct {
		name    string
		payload []byte
		red     image.Point // a point that must be red once upright
	}{
		{"no exif", nil, image.Pt(10, AvatarSize/2)},
		{"upright", exifOrientation(binary.BigEndian, 1), image.Pt(10, AvatarSize/2)},
		{"rotate 90 clockwise", exifOrientation(binary.BigEndian, 6), image.Pt(AvatarSize/2, 10)},
		{"rotate 90 counter-clockwise, little endian", exifOrientation(binary.LittleEndian, 8), image.Pt(AvatarSize/2, AvatarSize-10)},
		{"rotate 180", exifOrientation(binary.BigEndian, 3), image.Pt(AvatarSize-10, AvatarSize/2)},
		{"mirrored", exifOrientation(binary.LittleEndian, 2), image.Pt(AvatarSize-10, AvatarSize/2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := halves(400, 300)
			var data []byte
			if tt.payload == nil {
				data = encodePNG(t, in)
			} else {
				data = jpegWithExif(t, in, tt.payload)
			}
			out, err := Avatar(data)
			require.NoError(t, err)
			assert.False(t, bytes.Contains(out, []byte("Exif")), "metadata dropped")
			img, err := jpeg.Decode(bytes.NewReader(out))
			require.NoError(t, err)
			r, _, b, _ := img.At(tt.red.X, tt.red.Y).RGBA()
			assert.Greater(t, r, b, "red at %v", tt.red)
		})
	}
}

func TestAvatarRefusesNonImage(t *testing.T) {
	_, err := Avatar([]byte("%PDF-1.7"))
	require.ErrorIs(t, err, ErrNotImage)
}
