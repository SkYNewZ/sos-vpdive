package web

import (
	"html"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A toast is a fragment fetched under the session (spec §4.2 as amended):
// the event stream carries no name, the toast route does.
func TestToastFragment(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	path := "/demandes/" + strconv.FormatInt(tk.ID, 10) + "/toast?type="
	get := func(query string) (int, string) {
		t.Helper()
		rec := e.do(t, http.MethodGet, adminHost, path+query, nil, withCookie(cookie))
		return rec.Code, html.UnescapeString(rec.Body.String())
	}

	code, body := get("created")
	require.Equal(t, http.StatusOK, code)
	for _, want := range []string{"Nouvelle demande · " + tk.Ref, "Léa Martin · Carnet", `href="/demandes/` + strconv.FormatInt(tk.ID, 10) + `"`, "data-toast-close"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "<html", "a fragment, not a page")

	_, body = get("replied")
	assert.Contains(t, body, "Réponse de l'adhérent · "+tk.Ref)

	_, body = get("changed")
	assert.Contains(t, body, ">À traiter</p>", "no resolver, no name after the status")

	page := e.openTicket(t, cookie, tk.ID)
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, tk.ID, page, url.Values{"action": {"take"}}).Code)
	_, body = get("changed")
	assert.Contains(t, body, "Demande mise à jour · "+tk.Ref)
	assert.Contains(t, body, "En cours · Alice")

	code, _ = get("deleted")
	assert.Equal(t, http.StatusNotFound, code, "a deleted request has nothing to show")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, "/demandes/999/toast?type=created", nil, withCookie(cookie)).Code)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, path+"created", nil).Code, "committee host only")
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, path+"created", nil).Code, "session required")
}
