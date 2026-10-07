package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const newPassword = "a brand new long password"

func (e *testEnv) postAccount(t *testing.T, cookie *http.Cookie, v url.Values) (int, string, string) {
	t.Helper()
	rec := e.do(t, http.MethodPost, adminHost, "/compte", formBody(v), formType, withCookie(cookie))
	return rec.Code, html.UnescapeString(rec.Body.String()), rec.Header().Get("Location")
}

func TestTemporaryPasswordLeadsToTheAccountPage(t *testing.T) {
	e := newTestEnv(t)
	temporary, err := e.deps.Admins.ResetPassword(context.Background(), "alice")
	require.NoError(t, err)
	rec := e.postLogin(t, "alice", temporary)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	cookie := sessionCookie(t, rec)

	for _, path := range []string{"/", "/notifications", "/plus"} {
		rec := e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie))
		assert.Equal(t, http.StatusSeeOther, rec.Code, path)
		assert.Equal(t, "/compte", rec.Header().Get("Location"), path)
	}
	page := e.do(t, http.MethodGet, adminHost, "/compte", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, page.Code)
	body := html.UnescapeString(page.Body.String())
	assert.Contains(t, body, "Choisis ton mot de passe")
	assert.NotContains(t, body, `class="dock`, "no navigation that leads nowhere")
	assert.NotContains(t, body, `name="actuel"`, "the temporary password was just typed")
	assert.Contains(t, body, `action="/deconnexion"`)

	csrf := e.csrf(t, cookie, "/compte")
	assert.Equal(t, http.StatusForbidden,
		e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(url.Values{"csrf": {csrf}}), formType, withCookie(cookie)).Code)
	code, body, _ := e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {"short"}, "confirmation": {"short"}})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "Il faut au moins 12 caractères.")
	_, body, _ = e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {temporary}, "confirmation": {temporary}})
	assert.Contains(t, body, "C'est déjà ton mot de passe.")
	_, body, _ = e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {newPassword}, "confirmation": {newPassword + "!"}})
	assert.Contains(t, body, "Les deux mots de passe ne sont pas pareils.")

	code, _, location := e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {newPassword}, "confirmation": {newPassword}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Equal(t, "/", location)
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Code, "this session stays")
	assert.Equal(t, http.StatusSeeOther, e.postLogin(t, "alice", newPassword).Code)
}

func TestChangePasswordKeepsThisDeviceOnly(t *testing.T) {
	e := newTestEnv(t, withPush(t))
	this, other := e.login(t), e.login(t)
	keys := browserKeys(t)
	keys.Set("csrf", e.csrf(t, this, "/notifications"))
	require.Equal(t, http.StatusNoContent, e.pushPost(t, "/push/abonnement", this, keys))

	page := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/compte", nil, withCookie(this)).Body.String())
	for _, want := range []string{"Mon compte", "Alice", "Présidente", "Identifiant : alice", `name="actuel"`, `autocomplete="new-password"`} {
		assert.Contains(t, page, want)
	}
	csrf := e.csrf(t, this, "/compte")
	code, body, _ := e.postAccount(t, this, url.Values{"csrf": {csrf}, "actuel": {"wrong password"}, "nouveau": {newPassword}, "confirmation": {newPassword}})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "Ce n'est pas ton mot de passe actuel.")

	code, _, location := e.postAccount(t, this, url.Values{"csrf": {csrf}, "actuel": {testPassword}, "nouveau": {newPassword}, "confirmation": {newPassword}})
	require.Equal(t, http.StatusSeeOther, code)
	assert.Equal(t, "/compte?change=1", location)
	done := html.UnescapeString(e.do(t, http.MethodGet, adminHost, location, nil, withCookie(this)).Body.String())
	assert.Contains(t, done, "Mot de passe changé. Tes autres appareils sont déconnectés.")
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(other)).Code)
	assert.Equal(t, 1, e.count(t, "push_subscriptions"), "this device keeps its notifications")
}

func TestAccountLinks(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	board := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, board, `href="/compte"`, "the sidebar's account block")
	plus := e.do(t, http.MethodGet, adminHost, "/plus", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, plus, `href="/compte"`)
}
