package web

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNavSection(t *testing.T) {
	for path, want := range map[string][2]string{
		"/":                  {"demandes", "demandes"},
		"/demandes/12":       {"demandes", "demandes"},
		"/anomalies/masquer": {"anomalies", "anomalies"},
		"/fiches":            {"fiches", "fiches"},
		"/imports/confirmer": {"imports", "plus"},
		"/push/test":         {"notifications", "plus"},
		"/envois":            {"envois", "plus"},
		"/plus":              {"plus", "plus"},
	} {
		section, tab := navSection(path)
		assert.Equal(t, want, [2]string{section, tab}, path)
	}
}

// The committee shell: a sidebar and a phone tab bar with icons, the page's
// entry marked in both; « Plus » holds the pages without a tab. The members
// site has none of it.
func TestCommitteeShell(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	body := e.do(t, http.MethodGet, adminHost, "/annulations", nil, withCookie(cookie)).Body.String()
	assert.Equal(t, 2, strings.Count(body, `href="/annulations" aria-current="page"`), "sidebar and tab bar")
	assert.Equal(t, 2, strings.Count(body, `aria-current="page"`))
	assert.Contains(t, body, `<a class="inline-flex size-11 flex-none items-center justify-center" href="/plus">`,
		"the avatar link is a 44 px touch target (spec §12.1)")
	assert.Contains(t, body, `<p class="truncate text-xl font-bold" aria-hidden="true">Sorties annulées</p>`,
		"the phone title bar repeats the page's h1: screen readers read it once")
	for _, want := range []string{`<symbol id="i-calendar-x"`, `<use href="#i-calendar-x">`, `class="dock`, `data-toasts`, `data-open-banner hidden`} {
		assert.Contains(t, body, want)
	}

	rec := e.do(t, http.MethodGet, adminHost, "/plus", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	plus := html.UnescapeString(rec.Body.String())
	for _, want := range []string{`href="/imports"`, `href="/effacement"`, `href="/notifications"`, "Alice", "Présidente", `action="/deconnexion"`} {
		assert.Contains(t, plus, want)
	}
	assert.Equal(t, 1, strings.Count(plus, `aria-current="page"`), "the Plus tab only")
	assert.Contains(t, plus, `href="/plus" aria-current="page"`)
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/plus", nil).Code, "session required")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/plus", nil).Code)

	members := e.do(t, http.MethodGet, publicHost, "/", nil).Body.String()
	for _, absent := range []string{"<symbol", "dock", "data-toasts", "data-open-banner"} {
		assert.NotContains(t, members, absent)
	}
}
