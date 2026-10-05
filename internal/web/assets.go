package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static templates
var embedded embed.FS

// assets serves the embedded static files with versioned URLs: a URL that
// carries the current content hash is cacheable forever.
type assets struct {
	files    fs.FS
	versions map[string]string
}

func newAssets() (*assets, error) {
	files, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, fmt.Errorf("static files: %w", err)
	}
	versions := map[string]string{}
	err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		versions[p] = hex.EncodeToString(sum[:6])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("hash static files: %w", err)
	}
	return &assets{files: files, versions: versions}, nil
}

// URL returns the versioned URL of a static file. A file missing from the
// build (app.css before `make css`) gets a plain URL.
func (a *assets) URL(name string) string {
	if v, ok := a.versions[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

func (a *assets) handler() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(a.files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		v, ok := a.versions[name]
		if !ok {
			http.NotFound(w, r) // also refuses directory listings
			return
		}
		w.Header().Set("ETag", `"`+v+`"`)
		if r.URL.Query().Get("v") == v {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
