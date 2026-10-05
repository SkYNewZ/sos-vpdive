package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/images"
)

func readTestForm(t *testing.T, values url.Values, files ...[]byte) (memberForm, error) {
	t.Helper()
	body, contentType := multipartBody(t, values, files...)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", body)
	contentType(req)
	return readMultipart(httptest.NewRecorder(), req, captureField)
}

func TestReadMultipartKeepsFieldsAndFiles(t *testing.T) {
	f, err := readTestForm(t, url.Values{"prenom": {"Léa"}}, pngBytes(t), pngBytes(t))
	require.NoError(t, err)
	assert.Equal(t, "Léa", f.values.Get("prenom"))
	assert.Len(t, f.files, 2)
}

func TestReadMultipartLimits(t *testing.T) {
	many := url.Values{}
	for i := range maxFormParts + 1 {
		many.Set("champ"+strconv.Itoa(i), "x")
	}
	_, err := readTestForm(t, many)
	require.ErrorIs(t, err, errFormTooLarge, "too many parts")

	_, err = readTestForm(t, url.Values{"description": {strings.Repeat("a", maxTextBytes+1)}})
	require.ErrorIs(t, err, errFormTooLarge, "oversized text field")

	full := bytes.Repeat([]byte{1}, images.MaxBytes)
	_, err = readTestForm(t, nil, full, full, full, full)
	require.ErrorIs(t, err, errFormTooLarge, "body above three screenshots")

	f, err := readTestForm(t, nil, bytes.Repeat([]byte{1}, images.MaxBytes+10))
	require.NoError(t, err)
	assert.Len(t, f.files[0], images.MaxBytes+1, "cut one byte past the limit so that Sanitize refuses it")
}

func TestSanitizeCaptures(t *testing.T) {
	uploads, msg := sanitizeCaptures([][]byte{pngBytes(t)})
	assert.Empty(t, msg)
	require.Len(t, uploads, 1)
	assert.Equal(t, "image/png", uploads[0].MIME)

	_, msg = sanitizeCaptures([][]byte{pngBytes(t), pngBytes(t), pngBytes(t), pngBytes(t)})
	assert.Equal(t, "3 captures au plus par envoi.", msg)

	_, msg = sanitizeCaptures([][]byte{pngBytes(t), []byte("bonjour, ceci est un texte")})
	assert.Contains(t, msg, "Le fichier n° 2 n'est pas une image PNG, JPEG ou WebP")

	_, msg = sanitizeCaptures([][]byte{bytes.Repeat([]byte{1}, images.MaxBytes+1)})
	assert.Contains(t, msg, "dépasse 5 Mo")
}

func TestPathIDAndFormInt(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.SetPathValue("id", "42")
	id, ok := pathID(req, "id")
	assert.True(t, ok)
	assert.Equal(t, int64(42), id)
	for _, bad := range []string{"", "0", "-3", "abc", "99999999999999999999"} {
		req.SetPathValue("id", bad)
		_, ok = pathID(req, "id")
		assert.False(t, ok, bad)
	}
	assert.Equal(t, int64(5), formInt(url.Values{"version": {"5"}}, "version"))
	assert.Zero(t, formInt(url.Values{"version": {"x"}}, "version"))
}
