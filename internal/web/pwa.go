package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"text/template"
)

// themeColor is the club navy (spec §12.2), in the manifests and the layout.
const themeColor = "#0b2e4a"

// installable is one of the two apps (spec §9.6): its manifest and its
// service worker, built once at startup.
type installable struct {
	manifest []byte
	worker   []byte
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

type manifest struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	ShortName       string         `json:"short_name"`
	Lang            string         `json:"lang"`
	StartURL        string         `json:"start_url"`
	Scope           string         `json:"scope"`
	Display         string         `json:"display"`
	BackgroundColor string         `json:"background_color"`
	ThemeColor      string         `json:"theme_color"`
	Icons           []manifestIcon `json:"icons"`
}

// appName and iconDir tell the two apps apart (spec §9.6, §14.1).
func appName(admin bool) string {
	if admin {
		return "SOS CPP Comité"
	}
	return "SOS CPP"
}

func iconDir(admin bool) string {
	if admin {
		return "icons/comite/"
	}
	return "icons/membres/"
}

// newInstallables builds the manifest and the service worker of each site.
// The worker's cache name carries a hash of every embedded file, so any
// deploy that changes the site installs a new worker and shows the banner.
func newInstallables(a *assets) (map[bool]installable, error) {
	version, err := siteVersion()
	if err != nil {
		return nil, err
	}
	worker, err := template.ParseFS(embedded, "templates/sw.js")
	if err != nil {
		return nil, fmt.Errorf("parse service worker: %w", err)
	}
	precache := []string{offlinePath}
	for name := range a.versions {
		if strings.HasPrefix(name, "fonts/") {
			precache = append(precache, "/static/"+name) // the stylesheet asks for the plain path
		} else {
			precache = append(precache, a.URL(name))
		}
	}
	slices.Sort(precache)
	list, err := json.Marshal(precache)
	if err != nil {
		return nil, err
	}
	apps := map[bool]installable{}
	for _, admin := range []bool{false, true} {
		dir := iconDir(admin)
		icon := func(file, sizes, purpose string) manifestIcon {
			return manifestIcon{Src: a.URL(dir + file), Sizes: sizes, Type: "image/png", Purpose: purpose}
		}
		m, err := json.Marshal(manifest{
			ID: "/", Name: appName(admin), ShortName: appName(admin), Lang: "fr",
			StartURL: "/", Scope: "/", Display: "standalone", BackgroundColor: "#ffffff", ThemeColor: themeColor,
			Icons: []manifestIcon{
				icon("icon-192.png", "192x192", "any"),
				icon("icon-512.png", "512x512", "any"),
				icon("maskable-512.png", "512x512", "maskable"),
			},
		})
		if err != nil {
			return nil, err
		}
		var sw bytes.Buffer
		if err := worker.Execute(&sw, map[string]any{
			"Version": version, "Precache": string(list), "Admin": admin, "Icon": a.URL(dir + "icon-192.png"),
		}); err != nil {
			return nil, fmt.Errorf("render service worker: %w", err)
		}
		apps[admin] = installable{manifest: m, worker: sw.Bytes()}
	}
	return apps, nil
}

// siteVersion hashes every embedded static file and template.
func siteVersion() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(embedded, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(embedded, p)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s %d\n", p, len(data))
		_, _ = h.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash embedded files: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func (s *Server) manifestFile(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeBytes(w, r, "application/manifest+json", s.apps[admin].manifest)
	}
}

func (s *Server) serviceWorker(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeBytes(w, r, "text/javascript; charset=utf-8", s.apps[admin].worker)
	}
}

// offlinePage is cached by the service worker at install: it is fixed and
// never carries a session (spec §9.6).
func (s *Server) offlinePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "offline", s.newPage(r, "Hors ligne"))
}

func (s *Server) writeBytes(w http.ResponseWriter, r *http.Request, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	if _, err := w.Write(data); err != nil {
		s.logger.DebugContext(r.Context(), "write response", "error", err)
	}
}
