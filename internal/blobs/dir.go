package blobs

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// Dir stores each object as a file of one directory, for development
// without S3. os.Root confines every access to that directory.
type Dir struct {
	root *os.Root
}

// NewDir creates dir if needed and opens it.
func NewDir(dir string) (*Dir, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, wrap("captures directory", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, wrap("captures directory", err)
	}
	return &Dir{root: root}, nil
}

// Put writes a temporary file then renames it, so a reader never sees half
// an object.
func (d *Dir) Put(_ context.Context, key string, data []byte) error {
	if !keyPattern.MatchString(key) {
		return errInvalidKey
	}
	tmp := ".put-" + key
	if err := d.root.WriteFile(tmp, data, 0o600); err != nil {
		return wrap("write object", err)
	}
	if err := d.root.Rename(tmp, key); err != nil {
		return errors.Join(wrap("store object", err), scrub(d.root.Remove(tmp)))
	}
	return nil
}

// Get reads an object.
func (d *Dir) Get(_ context.Context, key string) ([]byte, error) {
	if !keyPattern.MatchString(key) {
		return nil, errInvalidKey
	}
	data, err := d.root.ReadFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, wrap("read object", err)
	}
	return data, nil
}

// Delete removes an object; a missing one is not an error.
func (d *Dir) Delete(_ context.Context, key string) error {
	if !keyPattern.MatchString(key) {
		return errInvalidKey
	}
	if err := d.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return wrap("delete object", err)
	}
	return nil
}

// List returns every object with its modification time. Temporary files
// never match a key and are skipped.
func (d *Dir) List(_ context.Context) ([]Object, error) {
	entries, err := fs.ReadDir(d.root.FS(), ".")
	if err != nil {
		return nil, wrap("list objects", err)
	}
	var out []Object
	for _, e := range entries {
		if !e.Type().IsRegular() || !keyPattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, wrap("list objects", err)
		}
		out = append(out, Object{Key: e.Name(), Modified: info.ModTime().UTC()})
	}
	return out, nil
}
