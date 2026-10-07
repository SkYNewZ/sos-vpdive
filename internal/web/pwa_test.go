package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testManifest struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ShortName  string `json:"short_name"`
	StartURL   string `json:"start_url"`
	Scope      string `json:"scope"`
	Display    string `json:"display"`
	ThemeColor string `json:"theme_color"`
	Icons      []struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	} `json:"icons"`
}

func TestManifestPerHost(t *testing.T) {
	e := newTestEnv(t)
	for host, name := range map[string]string{publicHost: "SOS CPP", adminHost: "SOS CPP Comité"} {
		t.Run(host, func(t *testing.T) {
			rec := e.do(t, http.MethodGet, host, "/manifest.webmanifest", nil)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "application/manifest+json", rec.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), "a new deploy is seen at once")
			var m testManifest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
			assert.Equal(t, name, m.Name)
			assert.Equal(t, name, m.ShortName)
			assert.Equal(t, "/", m.ID)
			assert.Equal(t, "/", m.StartURL)
			assert.Equal(t, "/", m.Scope)
			assert.Equal(t, "standalone", m.Display)
			assert.Equal(t, "#0b2e4a", m.ThemeColor)
			purposes := map[string]string{}
			for _, icon := range m.Icons {
				purposes[icon.Sizes+" "+icon.Purpose] = icon.Src
				assert.Equal(t, "image/png", icon.Type)
				assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, host, icon.Src, nil).Code, icon.Src)
			}
			assert.Len(t, purposes, 3)
			assert.Contains(t, purposes, "192x192 any")
			assert.Contains(t, purposes, "512x512 any")
			assert.Contains(t, purposes, "512x512 maskable", "a separate maskable entry, as web.dev advises")
		})
	}
	members := e.do(t, http.MethodGet, publicHost, "/manifest.webmanifest", nil).Body.String()
	committee := e.do(t, http.MethodGet, adminHost, "/manifest.webmanifest", nil).Body.String()
	assert.Contains(t, members, "/icons/membres/")
	assert.Contains(t, committee, "/icons/comite/", "each app has its own icons")
}

var precachePattern = regexp.MustCompile(`const PRECACHE = (\[[^\n]*\]);`)

func TestServiceWorkerPerHost(t *testing.T) {
	e := newTestEnv(t)
	workers := map[string]string{}
	for _, host := range []string{publicHost, adminHost} {
		rec := e.do(t, http.MethodGet, host, "/sw.js", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		workers[host] = rec.Body.String()

		m := precachePattern.FindStringSubmatch(rec.Body.String())
		require.NotNil(t, m, "the precache list is one JSON line")
		var urls []string
		require.NoError(t, json.Unmarshal([]byte(m[1]), &urls))
		assert.Contains(t, urls, "/hors-ligne")
		assert.Contains(t, urls, e.srv.assets.URL("app.js"))
		assert.Contains(t, urls, "/static/fonts/atkinson-hyperlegible-next-latin.woff2", "the font under the path the CSS uses")
		own, other := "/icons/membres/", "/icons/comite/"
		if host == adminHost {
			own, other = other, own
		}
		assert.Contains(t, urls, e.srv.assets.URL(strings.TrimPrefix(own, "/")+"icon-192.png"))
		assert.NotContains(t, m[1], other, "each app precaches its own icons only")
		for _, u := range urls {
			assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, host, u, nil).Code, u)
			assert.Regexp(t, `^/(static/|hors-ligne$)`, u, "a closed list: static files and the offline page, no page with data")
		}
	}
	assert.Regexp(t, `const CACHE = "sos-[0-9a-f]{12}";`, workers[publicHost])
	assert.NotContains(t, workers[publicHost], `addEventListener("push"`, "members get no push")
	assert.Contains(t, workers[adminHost], `addEventListener("push"`)
	assert.Contains(t, workers[adminHost], `addEventListener("notificationclick"`)
	assert.NotContains(t, workers[adminHost], ".navigate(", "the worker never steers a window away: the page decides, a reply may be in progress")
	assert.Contains(t, workers[adminHost], "client.url === url", "it focuses a window already on the request")
	assert.Contains(t, workers[adminHost], "postMessage({ open: url })", "an open window is asked to go there: iOS gives an installed app one window")
	assert.Contains(t, workers[adminHost], "clients.openWindow(url)")
	for _, sw := range workers {
		assert.Equal(t, 1, strings.Count(sw, "skipWaiting()"), "never at install: only when the page asks")
		assert.Contains(t, sw, `if (event.data === "skip-waiting") self.skipWaiting();`)
	}
}

func TestOfflinePageCarriesNoData(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	for _, host := range []string{publicHost, adminHost} {
		rec := e.do(t, http.MethodGet, host, "/hors-ligne", nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		body := rec.Body.String()
		assert.Contains(t, body, "data-reload")
		assert.NotContains(t, body, `name="csrf"`, "the cached page never holds a session")
		assert.NotContains(t, body, "Alice")
	}
}

func TestLayoutDeclaresTheInstallableApp(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	pages := map[string]string{
		"members":   e.do(t, http.MethodGet, publicHost, "/", nil).Body.String(),
		"committee": e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String(),
	}
	for name, body := range pages {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, body, `<link rel="manifest" href="/manifest.webmanifest">`)
			assert.Contains(t, body, `<meta name="theme-color" content="#0b2e4a">`)
			assert.Contains(t, body, `<meta name="viewport" content="width=device-width, initial-scale=1">`,
				"no viewport-fit=cover: iOS keeps the status bar and home indicator areas")
			assert.NotContains(t, body, "apple-mobile-web-app-status-bar-style")
			assert.Contains(t, body, `<div data-update hidden`, "the update banner waits for app.js")
			assert.Contains(t, body, `<div data-old-browser hidden`)
			assert.Contains(t, body, e.srv.assets.URL("compat.js"))
		})
	}
	app := map[string]string{"members": "membres", "committee": "comite"}
	for name, body := range pages {
		assert.Contains(t, body, `<link rel="icon" type="image/png" sizes="32x32" href="`+e.srv.assets.URL("icons/"+app[name]+"/favicon-32.png")+`">`)
		assert.Contains(t, body, `<link rel="apple-touch-icon" href="`+e.srv.assets.URL("icons/"+app[name]+"/apple-touch-icon.png")+`">`)
	}
}

func TestEveryMemberPageLinksBackToTheForm(t *testing.T) {
	e, _ := modelEnv(t, 200)
	page, _ := e.screen2(t, validRequest(e.formKey(t)))
	assert.Contains(t, page, `href="/"`, "screen 2: an installed app has no back button")
	for _, path := range []string{"/retrouver", "/demandes/abandonnee"} {
		assert.Contains(t, e.do(t, http.MethodGet, publicHost, path, nil).Body.String(), `href="/"`, path)
	}
	tt := e.submitTicket(t, "lea.martin@example.org")
	_, body := e.tracking(t, tt.Token)
	assert.Contains(t, body, `href="/"`, "the tracking page, opened from a mail, leads to the form")
}

func TestPagesMarkTheLocalDraftSteps(t *testing.T) {
	e, _ := modelEnv(t, 200)
	form := e.do(t, http.MethodGet, publicHost, "/", nil).Body.String()
	assert.Contains(t, form, `enctype="multipart/form-data" class="mt-6 flex flex-col gap-5" data-draft>`, "app.js keeps this form's text")
	assert.Contains(t, form, `data-draft-note hidden`, "the note shows only when storage works")
	assert.Contains(t, form, `data-draft-clear`)

	page, _ := e.screen2(t, validRequest(e.formKey(t)))
	assert.Contains(t, page, "data-draft-key-used", "screen 2: the key reached the server")
	for _, path := range []string{"/demandes/envoyee?ref=CPP-0001", "/demandes/abandonnee"} {
		assert.Contains(t, e.do(t, http.MethodGet, publicHost, path, nil).Body.String(), "data-draft-done", path)
	}
	assert.NotContains(t, form, "data-draft-done")
}
