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

// manifestScreenshot feeds Chrome's richer install dialog: one without
// form_factor for phones, one "wide" for desktops.
type manifestScreenshot struct {
	Src        string `json:"src"`
	Sizes      string `json:"sizes"`
	Type       string `json:"type"`
	FormFactor string `json:"form_factor,omitempty"`
	Label      string `json:"label"`
}

type manifest struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	ShortName       string               `json:"short_name"`
	Description     string               `json:"description"`
	Lang            string               `json:"lang"`
	StartURL        string               `json:"start_url"`
	Scope           string               `json:"scope"`
	Display         string               `json:"display"`
	BackgroundColor string               `json:"background_color"`
	ThemeColor      string               `json:"theme_color"`
	Icons           []manifestIcon       `json:"icons"`
	Screenshots     []manifestScreenshot `json:"screenshots"`
}

// apps tell the two installable apps apart (spec §9.6, §14.1); the layout
// picks the same icon folders.
// The screenshots are made with synthetic data from testdata/fixtures
// (README, "Adapt it to your club"), never with real requests.
var apps = []struct {
	admin       bool
	name        string
	icons       string
	description string
	screenshots string // folder of etroite.png (824x1830) and large.png (1280x800)
	label       string // what the screenshots show
}{
	{
		admin: false, name: "SOS CPP", icons: "icons/membres/", screenshots: "screenshots/membres/",
		description: "Pose ta question au comité sur VPDive et suis sa réponse.", label: "Le formulaire de demande",
	},
	{
		admin: true, name: "SOS CPP Comité", icons: "icons/comite/", screenshots: "screenshots/comite/",
		description: "Traite les demandes des adhérents au sujet de VPDive.", label: "Le tableau des demandes",
	},
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
	built := map[bool]installable{}
	for _, app := range apps {
		precache := []string{offlinePath}
		for name := range a.versions {
			switch {
			case strings.HasPrefix(name, "icons/") && !strings.HasPrefix(name, app.icons):
				// the other app's icons
			case strings.HasPrefix(name, "screenshots/"):
				// for the install dialog only
			case strings.HasPrefix(name, "fonts/"):
				precache = append(precache, "/static/"+name) // the stylesheet asks for the plain path
			default:
				precache = append(precache, a.URL(name))
			}
		}
		slices.Sort(precache)
		list, err := json.Marshal(precache)
		if err != nil {
			return nil, err
		}
		icon := func(file, sizes, purpose string) manifestIcon {
			return manifestIcon{Src: a.URL(app.icons + file), Sizes: sizes, Type: "image/png", Purpose: purpose}
		}
		shot := func(file, sizes, formFactor string) manifestScreenshot {
			return manifestScreenshot{Src: a.URL(app.screenshots + file), Sizes: sizes, Type: "image/png", FormFactor: formFactor, Label: app.label}
		}
		m, err := json.Marshal(manifest{
			ID: "/", Name: app.name, ShortName: app.name, Description: app.description, Lang: "fr",
			StartURL: "/", Scope: "/", Display: "standalone", BackgroundColor: "#ffffff", ThemeColor: themeColor,
			Icons: []manifestIcon{
				icon("icon-192.png", "192x192", "any"),
				icon("icon-512.png", "512x512", "any"),
				icon("maskable-512.png", "512x512", "maskable"),
			},
			Screenshots: []manifestScreenshot{shot("etroite.png", "824x1830", ""), shot("large.png", "1280x800", "wide")},
		})
		if err != nil {
			return nil, err
		}
		var sw bytes.Buffer
		if err := worker.Execute(&sw, map[string]any{
			"Version": version, "Precache": string(list), "Offline": offlinePath,
			"Admin": app.admin, "Icon": a.URL(app.icons + "icon-192.png"),
		}); err != nil {
			return nil, fmt.Errorf("render service worker: %w", err)
		}
		built[app.admin] = installable{manifest: m, worker: sw.Bytes()}
	}
	return built, nil
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
		if _, err := fmt.Fprintf(h, "%s %d\n", p, len(data)); err != nil {
			return err
		}
		_, err = h.Write(data)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("hash embedded files: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func (s *Server) manifestFile(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.write(w, r, http.StatusOK, "application/manifest+json", s.apps[admin].manifest)
	}
}

func (s *Server) serviceWorker(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.write(w, r, http.StatusOK, "text/javascript; charset=utf-8", s.apps[admin].worker)
	}
}

// offlinePage is cached by the service worker at install: it is fixed and
// never carries a session (spec §9.6).
func (s *Server) offlinePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "offline", s.newPage(r, "Hors ligne"))
}
