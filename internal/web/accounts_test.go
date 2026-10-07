package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

var temporaryPattern = regexp.MustCompile(`[a-z2-9]{4}(?:-[a-z2-9]{4}){3}`)

func TestOwnerCreatesResetsAndDeletesAccounts(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t) // alice is OWNER_USERNAME
	list := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/comptes", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, list, "Ajouter un compte")
	assert.Contains(t, list, `href="/comptes" aria-current="page"`)
	assert.NotContains(t, list, `href="/comptes/alice"`, "the owner's own row has no page")
	csrf := e.csrf(t, cookie, "/comptes")

	rec := e.postAs(t, cookie, "/comptes", url.Values{"csrf": {csrf}, "identifiant": {"bob smith"}, "nom": {" "}, "fonction": {""}})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	for _, want := range []string{"Lettres minuscules sans accent", "Indique le nom affiché.", "Indique la fonction.", `value="bob smith"`} {
		assert.Contains(t, body, want)
	}

	rec = e.postAs(t, cookie, "/comptes", url.Values{"csrf": {csrf}, "identifiant": {".."}, "nom": {"Bob"}, "fonction": {"Trésorier"}})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a path segment browsers normalise away")
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Lettres minuscules sans accent")

	rec = e.postAs(t, cookie, "/comptes", url.Values{"csrf": {csrf}, "identifiant": {" Bob "}, "nom": {"Bob"}, "fonction": {"Trésorier"}})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body = html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, "Compte créé pour Bob")
	assert.Contains(t, body, "Les tirets font partie du mot de passe.")
	first := temporaryPattern.FindString(body)
	require.NotEmpty(t, first)
	_, ok := e.deps.Admins.Get("bob")
	require.True(t, ok, "the username was trimmed and lowercased")

	rec = e.postAs(t, cookie, "/comptes", url.Values{"csrf": {csrf}, "identifiant": {"bob"}, "nom": {"Bob"}, "fonction": {"Trésorier"}})
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Cet identifiant est déjà pris.")

	login := e.postLogin(t, "bob", first)
	require.Equal(t, http.StatusSeeOther, login.Code)
	bobCookie := sessionCookie(t, login)
	assert.Contains(t, html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/comptes", nil, withCookie(cookie)).Body.String()), "Mot de passe temporaire")

	rec = e.postAs(t, cookie, "/comptes/bob/mot-de-passe", url.Values{"csrf": {csrf}})
	require.Equal(t, http.StatusOK, rec.Code)
	body = html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, "Nouveau mot de passe temporaire pour Bob")
	assert.NotEqual(t, first, temporaryPattern.FindString(body))
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/compte", nil, withCookie(bobCookie)).Code, "bob's sessions ended")

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, "/comptes/alice", nil, withCookie(cookie)).Code)
	assert.Equal(t, http.StatusNotFound, e.postAs(t, cookie, "/comptes/alice/mot-de-passe", url.Values{"csrf": {csrf}}).Code)
	assert.Equal(t, http.StatusNotFound, e.postAs(t, cookie, "/comptes/alice/suppression", url.Values{"csrf": {csrf}, "confirmer": {"oui"}}).Code)

	detail := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/comptes/bob", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, detail, "Je supprime le compte de Bob")
	assert.Equal(t, http.StatusBadRequest, e.postAs(t, cookie, "/comptes/bob/suppression", url.Values{"csrf": {csrf}}).Code)
	rec = e.postAs(t, cookie, "/comptes/bob/suppression", url.Values{"csrf": {csrf}, "confirmer": {"oui"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/comptes?supprime=1", rec.Header().Get("Location"))
	_, ok = e.deps.Admins.Get("bob")
	assert.False(t, ok)
	assert.Contains(t, e.do(t, http.MethodGet, adminHost, "/comptes?supprime=1", nil, withCookie(cookie)).Body.String(), "Compte supprimé.")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, "/comptes/bob", nil, withCookie(cookie)).Code)
}

func TestAccountsAreTheOwnersOnly(t *testing.T) {
	e := newTestEnv(t)
	e.addBob(t)
	rec := e.postLogin(t, "bob", testPassword)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	cookie := sessionCookie(t, rec)

	plus := e.do(t, http.MethodGet, adminHost, "/plus", nil, withCookie(cookie)).Body.String()
	assert.NotContains(t, plus, `href="/comptes"`)
	for _, path := range []string{"/comptes", "/comptes/alice"} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie)).Code, path)
	}
	csrf := e.csrf(t, cookie, "/plus")
	assert.Equal(t, http.StatusNotFound, e.postAs(t, cookie, "/comptes", url.Values{"csrf": {csrf}, "identifiant": {"eve"}, "nom": {"Eve"}, "fonction": {"X"}}).Code)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/comptes", nil).Code)
}

func TestDeletingAnAccountReturnsItsRequests(t *testing.T) {
	e := newTestEnv(t)
	e.addBob(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	require.NoError(t, e.deps.Tickets.Apply(context.Background(),
		tickets.Command{Action: tickets.ActionTake, TicketID: tk.ID, Version: e.detail(t, tk.ID).Version, Actor: "bob"}))

	rec := e.postAs(t, cookie, "/comptes/bob/suppression", url.Values{"csrf": {e.csrf(t, cookie, "/comptes")}, "confirmer": {"oui"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	d := e.detail(t, tk.ID)
	assert.Equal(t, tickets.StatusTodo, d.Status)
	assert.Empty(t, d.Assignee)
}
