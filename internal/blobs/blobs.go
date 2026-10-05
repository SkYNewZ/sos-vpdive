// Package blobs stores the encrypted screenshots (spec §9.8): in a private
// S3-compatible bucket in production (Cloudflare R2), in a local directory
// in development. Callers seal the data before Put; keys are random.
package blobs

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// ErrNotFound reports a key without object.
var ErrNotFound = errors.New("object not found")

// Object is one stored object, as listed for the orphan sweep.
type Object struct {
	Key      string
	Modified time.Time
}

// Store holds encrypted screenshots. Delete of a missing key returns nil.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error) // ErrNotFound
	Delete(ctx context.Context, key string) error
	List(ctx context.Context) ([]Object, error)
}

// keyPattern matches the keys NewKey draws: base64url only, so a key can
// never climb out of the directory store.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

var errInvalidKey = errors.New("invalid object key")

// New returns the S3 store when cfg.S3 is set, else a Dir under
// DataDir/captures (development only: config requires S3 in production).
func New(cfg *config.Config) (Store, error) {
	if cfg.S3 != nil {
		s, err := NewS3(*cfg.S3)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	d, err := NewDir(filepath.Join(cfg.DataDir, "captures"))
	if err != nil {
		return nil, err
	}
	return d, nil
}

// NewKey returns a random object key, unrelated to any ticket or file name.
func NewKey() (string, error) {
	return secure.NewToken()
}
