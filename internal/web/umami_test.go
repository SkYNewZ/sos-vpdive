package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// lot1CSP is the policy of spec §11.5, unchanged while Umami is off.
const lot1CSP = "default-src 'self'; script-src 'self' https://challenges.cloudflare.com; " +
	"frame-src https://challenges.cloudflare.com; connect-src 'self'; img-src 'self' data: blob:; " +
	"style-src 'self'; worker-src 'self'; manifest-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'self'; form-action 'self'"

const (
	umamiScript    = "https://analytics.example.org/script.js"
	membersSiteID  = "11111111-2222-4333-8444-555555555555"
	committeeSite  = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	umamiCSPOrigin = "https://analytics.example.org"
)

func withUmami(t *testing.T, adminID string) func(*Deps) {
	t.Helper()
	script := mustURL(t, umamiScript)
	return func(d *Deps) {
		d.Config.Umami = &config.Umami{ScriptURL: script, WebsiteID: membersSiteID, AdminWebsiteID: adminID}
	}
}

var (
	umamiAttr = regexp.MustCompile(`data-umami-([a-z]+)="([^"]*)"`)
	scriptSrc = regexp.MustCompile(`<script[^>]* src="([^"]+)"`)
)

// umamiAttrs returns the data-umami-* attributes of a page, by name.
func umamiAttrs(body string) map[string]string {
	attrs := map[string]string{}
	for _, m := range umamiAttr.FindAllStringSubmatch(body, -1) {
		attrs[m[1]] = m[2]
	}
	return attrs
}

func TestNoThirdPartyScriptWithoutUmami(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	lea := e.submitTicket(t, "lea.martin@example.org")
	for _, rec := range []*httptest.ResponseRecorder{
		e.do(t, http.MethodGet, publicHost, "/", nil),
		e.do(t, http.MethodGet, publicHost, "/suivi/"+lea.Token, nil),
		e.do(t, http.MethodGet, adminHost, "/connexion", nil),
	} {
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, lot1CSP, rec.Header().Get("Content-Security-Policy"), "byte for byte")
		body := rec.Body.String()
		assert.Empty(t, umamiAttrs(body))
		for _, m := range scriptSrc.FindAllStringSubmatch(body, -1) {
			assert.True(t, strings.HasPrefix(m[1], "/static/"), "script %s", m[1])
		}
	}
}

func TestUmamiReportsRouteTemplatesOnly(t *testing.T) {
	e := newTestEnv(t, withUmami(t, committeeSite))
	e.importMembers(t, "members_valid.xlsx")
	lea := e.submitTicket(t, "lea.martin@example.org")
	cookie := e.login(t)

	tracking := e.do(t, http.MethodGet, publicHost, "/suivi/"+lea.Token, nil)
	require.Equal(t, http.StatusOK, tracking.Code)
	assert.Equal(t, map[string]string{"src": umamiScript, "website": membersSiteID, "path": "/suivi/[masqué]"},
		umamiAttrs(tracking.Body.String()))
	assert.Equal(t, "default-src 'self'; script-src 'self' https://challenges.cloudflare.com "+umamiCSPOrigin+"; "+
		"frame-src https://challenges.cloudflare.com; connect-src 'self' "+umamiCSPOrigin+"; img-src 'self' data: blob:; "+
		"style-src 'self'; worker-src 'self'; manifest-src 'self'; frame-ancestors 'none'; "+
		"base-uri 'self'; form-action 'self'", tracking.Header().Get("Content-Security-Policy"))
	for _, m := range scriptSrc.FindAllStringSubmatch(tracking.Body.String(), -1) {
		assert.True(t, strings.HasPrefix(m[1], "/static/"), "no third-party script tag: app.js loads Umami")
	}

	cases := []struct {
		host, target string
		website      string
		path         string
	}{
		{publicHost, "/", membersSiteID, "/"},
		{publicHost, "/retrouver", membersSiteID, "/retrouver"},
		{publicHost, "/demandes/envoyee?ref=" + lea.Ref, membersSiteID, "/demandes/envoyee"},
		{adminHost, "/fiches", committeeSite, "/fiches"},
		{adminHost, "/", committeeSite, "/"},
		{adminHost, "/demandes/" + strconv.FormatInt(lea.ID, 10), committeeSite, "/demandes/[id]"},
	}
	for _, tc := range cases {
		rec := e.do(t, http.MethodGet, tc.host, tc.target, nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code, tc.target)
		attrs := umamiAttrs(rec.Body.String())
		assert.Equal(t, tc.website, attrs["website"], tc.target)
		assert.Equal(t, tc.path, attrs["path"], tc.target)
		for _, v := range attrs {
			assert.NotContains(t, v, lea.Ref, "never a reference")
			assert.NotContains(t, v, lea.Token, "never a token")
		}
	}

	login := umamiAttrs(e.do(t, http.MethodGet, adminHost, "/connexion", nil).Body.String())
	assert.Equal(t, "/connexion", login["path"], "signed out too")

	notFound := e.do(t, http.MethodGet, publicHost, "/suivi", nil)
	assert.Equal(t, http.StatusNotFound, notFound.Code)
	assert.Empty(t, umamiAttrs(notFound.Body.String()), "the catch-all is not measured")
}

func TestUmamiLeavesASiteWithoutIDUnmeasured(t *testing.T) {
	e := newTestEnv(t, withUmami(t, ""))
	assert.Equal(t, membersSiteID, umamiAttrs(e.do(t, http.MethodGet, publicHost, "/", nil).Body.String())["website"])
	assert.Empty(t, umamiAttrs(e.do(t, http.MethodGet, adminHost, "/connexion", nil).Body.String()))
}

func TestUmamiPath(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /{$}":                          "/",
		"GET /suivi/{jeton}":                "/suivi/[masqué]",
		"POST /suivi/{jeton}/reponse":       "/suivi/[masqué]/reponse",
		"GET /demandes/{id}":                "/demandes/[id]",
		"GET /demandes/{id}/captures/{cid}": "/demandes/[id]/captures/[cid]",
		"POST /demandes":                    "/demandes",
		"/":                                 "", // the catch-all matches any path
		"":                                  "",
	} {
		assert.Equal(t, want, umamiPath(pattern), pattern)
	}
}
