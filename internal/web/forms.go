package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/SkYNewZ/sos-vpdive/internal/images"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Member forms with screenshots are read in memory: the root file system is
// read-only, and ParseMultipartForm would spill large parts to /tmp.
const (
	captureField = "captures"
	maxFormParts = 64
	maxTextBytes = 16 << 10 // 4 000 runes of UTF-8 always fit
	maxFormBody  = tickets.MaxCapturesPerPost*images.MaxBytes + 64<<10
)

var errFormTooLarge = errors.New("form too large")

// memberForm is a multipart form read in memory.
type memberForm struct {
	values url.Values
	files  [][]byte
}

// readMultipart reads the text fields of a form and the non-empty files of
// fileField. A file longer than images.MaxBytes is cut one byte past the
// limit, so that images.Sanitize refuses it as too big.
func readMultipart(w http.ResponseWriter, r *http.Request, fileField string) (memberForm, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	mr, err := r.MultipartReader()
	if err != nil {
		return memberForm{}, fmt.Errorf("multipart form: %w", err)
	}
	f := memberForm{values: url.Values{}}
	for parts := 0; ; parts++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return f, nil
		}
		if err != nil {
			return memberForm{}, bodyError(err)
		}
		if parts == maxFormParts {
			return memberForm{}, errFormTooLarge
		}
		if part.FormName() == fileField && part.FileName() != "" {
			data, err := io.ReadAll(io.LimitReader(part, images.MaxBytes+1))
			if err != nil {
				return memberForm{}, bodyError(err)
			}
			if len(data) > 0 {
				f.files = append(f.files, data)
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxTextBytes+1))
		if err != nil {
			return memberForm{}, bodyError(err)
		}
		if len(data) > maxTextBytes {
			return memberForm{}, errFormTooLarge
		}
		f.values.Add(part.FormName(), string(data))
	}
}

// bodyError tells an oversized body apart from a broken one.
func bodyError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return errFormTooLarge
	}
	return fmt.Errorf("read form: %w", err)
}

// sanitizeCaptures checks and re-encodes the screenshots of one post (spec
// §11.4). The message, shown beside the captures field, is empty when every
// file passed.
func sanitizeCaptures(files [][]byte) ([]tickets.Upload, string) {
	if len(files) > tickets.MaxCapturesPerPost {
		return nil, "3 captures au plus par envoi."
	}
	out := make([]tickets.Upload, 0, len(files))
	for i, data := range files {
		n := strconv.Itoa(i + 1)
		clean, mime, err := images.Sanitize(data)
		switch {
		case errors.Is(err, images.ErrTooBig):
			return nil, "La capture n° " + n + " dépasse 5 Mo."
		case errors.Is(err, images.ErrTooManyPixels):
			return nil, "La capture n° " + n + " est trop grande : 40 millions de pixels au plus."
		case err != nil:
			return nil, "Le fichier n° " + n + " n'est pas une image PNG, JPEG ou WebP. Joins une capture d'écran."
		}
		out = append(out, tickets.Upload{Data: clean, MIME: mime})
	}
	return out, ""
}

// pathID reads a positive numeric path value.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// formInt reads a numeric form value; anything unreadable is zero.
func formInt(v url.Values, key string) int64 {
	n, err := strconv.ParseInt(v.Get(key), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
