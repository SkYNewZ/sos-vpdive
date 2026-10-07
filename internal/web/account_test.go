package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

const newPassword = "a brand new long password"

func (e *testEnv) postAccount(t *testing.T, cookie *http.Cookie, v url.Values) (int, string) {
	t.Helper()
	rec := e.postAs(t, cookie, "/compte", v)
	return rec.Code, html.UnescapeString(rec.Body.String())
}

// changeOK posts a password change that succeeds and returns the new
// session's cookie, after checking that the old one ended.
func (e *testEnv) changeOK(t *testing.T, old *http.Cookie, v url.Values, location string) *http.Cookie {
	t.Helper()
	rec := e.postAs(t, old, "/compte", v)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	assert.Equal(t, location, rec.Header().Get("Location"))
	cookie := sessionCookie(t, rec)
	assert.NotEqual(t, old.Value, cookie.Value, "a new session")
	gone := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(old))
	assert.Equal(t, http.StatusSeeOther, gone.Code, "the old session ended")
	assert.Equal(t, "/connexion", gone.Header().Get("Location"))
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Code, "this device stays signed in")
	return cookie
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

	// A stream that opens would run until the context ends: bound it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	committee := func(r *http.Request) { *r = *r.WithContext(ctx); r.Header.Set("Origin", "https://"+adminHost) }
	stream := e.do(t, http.MethodGet, adminHost, "/evenements", nil, withCookie(cookie), committee)
	assert.Equal(t, http.StatusForbidden, stream.Code, "no live board before the password is chosen")
	assert.Contains(t, stream.Body.String(), "Session expirée")

	csrf := e.csrf(t, cookie, "/compte")
	assert.Equal(t, http.StatusForbidden,
		e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(url.Values{"csrf": {csrf}}), formType, withCookie(cookie)).Code)
	code, body := e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {"short"}, "confirmation": {"short"}})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "Il faut au moins 12 caractères.")
	_, body = e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {temporary}, "confirmation": {temporary}})
	assert.Contains(t, body, "C'est déjà ton mot de passe.")
	_, body = e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {newPassword}, "confirmation": {newPassword + "!"}})
	assert.Contains(t, body, "Les deux mots de passe ne sont pas pareils.")

	e.changeOK(t, cookie, url.Values{"csrf": {csrf}, "nouveau": {newPassword}, "confirmation": {newPassword}}, "/")
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
	code, body := e.postAccount(t, this, url.Values{"csrf": {csrf}, "actuel": {"wrong password"}, "nouveau": {newPassword}, "confirmation": {newPassword}})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "Ce n'est pas ton mot de passe actuel.")

	renewed := e.changeOK(t, this, url.Values{"csrf": {csrf}, "actuel": {testPassword}, "nouveau": {newPassword}, "confirmation": {newPassword}}, "/compte?change=1")
	done := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/compte?change=1", nil, withCookie(renewed)).Body.String())
	assert.Contains(t, done, "Mot de passe changé. Tes autres appareils sont déconnectés.")
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(other)).Code)
	var follows int
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM push_subscriptions WHERE session_token_hash = ?`, secure.TokenHash(renewed.Value)).Scan(&follows))
	assert.Equal(t, 1, e.count(t, "push_subscriptions"), "this device keeps its notifications")
	assert.Equal(t, 1, follows, "they follow the new session")
}

func TestCurrentPasswordIsRateLimited(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/compte")
	post := func(current string) (int, string) {
		return e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "actuel": {current}, "nouveau": {newPassword}, "confirmation": {newPassword}})
	}
	for range userFailureLimit {
		code, _ := post("wrong password")
		require.Equal(t, http.StatusUnprocessableEntity, code)
	}
	code, body := post("wrong password")
	assert.Equal(t, http.StatusTooManyRequests, code)
	assert.Contains(t, body, "Trop d'essais")
	code, _ = post(testPassword)
	assert.Equal(t, http.StatusTooManyRequests, code, "even the right password waits")
}

// The owner resets the password while the form is open: the change does not
// overwrite the reset.
func TestChangePasswordAfterAReset(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/compte")
	_, err := e.db.ExecContext(context.Background(), `UPDATE accounts SET password_hash = 'reset' WHERE username = 'alice'`)
	require.NoError(t, err)
	code, body := e.postAccount(t, cookie, url.Values{"csrf": {csrf}, "actuel": {testPassword}, "nouveau": {newPassword}, "confirmation": {newPassword}})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "Ton mot de passe vient d'être changé. Reconnecte-toi.")
}

func TestAccountLinks(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	board := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, board, `href="/compte"`, "the sidebar's account block")
	plus := e.do(t, http.MethodGet, adminHost, "/plus", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, plus, `href="/compte"`)
}
