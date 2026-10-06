package web

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/push"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

func TestLoginPageInDevelopmentHasNoTurnstile(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, adminHost, "/connexion", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Connexion du comité")
	assert.NotContains(t, rec.Body.String(), "cf-turnstile")
	assert.Contains(t, rec.Body.String(), `autocomplete="current-password"`)
}

func TestCommitteePagesRequireSession(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, adminHost, "/", nil)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/connexion", rec.Header().Get("Location"))
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodPost, adminHost, "/deconnexion", nil, formType).Code)
}

func TestLoginIgnoresUsernameCase(t *testing.T) {
	e := newTestEnv(t)
	rec := e.postLogin(t, " Alice ", testPassword)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
}

func TestLoginSetsHostOnlyCookieAndShowsAccount(t *testing.T) {
	e := newTestEnv(t)
	rec := e.postLogin(t, "alice", testPassword)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/", rec.Header().Get("Location"))

	c := sessionCookie(t, rec)
	assert.True(t, c.Secure)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, "/", c.Path)
	assert.Empty(t, c.Domain)
	assert.Equal(t, 30*24*3600, c.MaxAge)
	assert.Len(t, c.Value, 43)

	home := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(c))
	require.Equal(t, http.StatusOK, home.Code)
	body := home.Body.String()
	assert.Contains(t, body, "Alice")
	assert.Contains(t, body, "Présidente")
	assert.Contains(t, html.UnescapeString(body), `src="data:image/svg+xml`)
	assert.Contains(t, body, "SOS CPP Comité")

	var stored int
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sessions WHERE token_hash = ?`, []byte(c.Value)).Scan(&stored))
	assert.Zero(t, stored, "only the token hash is stored")
}

func TestLoginFailuresShowOneMessage(t *testing.T) {
	e := newTestEnv(t)
	for _, creds := range [][2]string{{"alice", "wrong"}, {"nobody", testPassword}} {
		rec := e.postLogin(t, creds[0], creds[1])
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "Identifiant ou mot de passe incorrect.")
	}
}

func TestFiveConsecutiveFailuresDelayLogin(t *testing.T) {
	e := newTestEnv(t)
	for range 5 {
		require.Equal(t, http.StatusUnauthorized, e.postLogin(t, "alice", "wrong").Code)
	}
	blocked := e.postLogin(t, "alice", testPassword)
	assert.Equal(t, http.StatusTooManyRequests, blocked.Code)
	assert.Contains(t, html.UnescapeString(blocked.Body.String()), "Trop d'essais")

	e.clock.advance(61 * time.Second)
	assert.Equal(t, http.StatusSeeOther, e.postLogin(t, "alice", testPassword).Code)
}

func TestDelayGrowsAndIsCapped(t *testing.T) {
	assert.Equal(t, time.Minute, userDelay(5))
	assert.Equal(t, 2*time.Minute, userDelay(6))
	assert.Equal(t, 32*time.Minute, userDelay(10))
	assert.Equal(t, time.Hour, userDelay(11))
	assert.Equal(t, time.Hour, userDelay(50))
}

func TestTenFailuresPerHourBlockTheAddress(t *testing.T) {
	e := newTestEnv(t)
	for i := range 10 {
		require.Equal(t, http.StatusUnauthorized, e.postLogin(t, "user"+string(rune('a'+i)), "wrong").Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, e.postLogin(t, "alice", testPassword).Code)

	other := func(r *http.Request) { r.RemoteAddr = "198.51.100.20:4000" }
	assert.Equal(t, http.StatusSeeOther, e.postLogin(t, "alice", testPassword, other).Code)

	e.clock.advance(time.Hour + time.Second)
	assert.Equal(t, http.StatusSeeOther, e.postLogin(t, "alice", testPassword).Code)
}

func TestTurnstileOnLogin(t *testing.T) {
	var reply string
	var status int
	var gotForm url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		gotForm = r.PostForm
		w.WriteHeader(status)
		_, err := w.Write([]byte(reply))
		assert.NoError(t, err)
	}))
	defer fake.Close()
	e := newTestEnv(t, func(d *Deps) {
		d.Turnstile = NewTurnstile("1x00000000000000000000AA", "1x0000000000000000000000000000000AA", fake.URL)
	})
	page := e.do(t, http.MethodGet, adminHost, "/connexion", nil)
	assert.Contains(t, page.Body.String(), `class="cf-turnstile" data-sitekey="1x00000000000000000000AA" data-action="connexion"`)
	assert.Contains(t, page.Body.String(), "https://challenges.cloudflare.com/turnstile/v0/api.js")

	post := func() *httptest.ResponseRecorder {
		body := formBody(url.Values{"username": {"alice"}, "password": {testPassword}, "cf-turnstile-response": {"token-xyz"}})
		return e.do(t, http.MethodPost, adminHost, "/connexion", body, formType)
	}

	status, reply = http.StatusOK, `{"success":true,"hostname":"comite.example.org","action":"connexion"}`
	assert.Equal(t, http.StatusSeeOther, post().Code)
	assert.Equal(t, "token-xyz", gotForm.Get("response"))
	assert.Equal(t, "1x0000000000000000000000000000000AA", gotForm.Get("secret"))
	assert.Equal(t, "192.0.2.10", gotForm.Get("remoteip"))

	for _, bad := range []string{
		`{"success":false,"error-codes":["invalid-input-response"]}`,
		`{"success":true,"hostname":"evil.example","action":"connexion"}`,
		`{"success":true,"hostname":"comite.example.org","action":"other"}`,
	} {
		reply = bad
		rec := post()
		assert.Equal(t, http.StatusForbidden, rec.Code, bad)
		assert.Contains(t, rec.Body.String(), "Le contrôle anti-robot a échoué")
	}

	reply = `{"success":false,"error-codes":["invalid-input-secret"]}`
	status = http.StatusOK
	rec := post()
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "club@example.org")

	status = http.StatusBadGateway
	rec = post()
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "club@example.org")
	assert.Contains(t, e.logs.String(), `"level":"ERROR","msg":"turnstile unavailable"`, "a 503 reaches Sentry")
}

func countSessions(t *testing.T, e *testEnv) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n))
	return n
}

func TestRevokeSessionsAfterPasswordChange(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	require.Equal(t, http.StatusOK, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Code)

	changed := alice()
	var err error
	changed.PasswordHash, err = admins.HashPassword("a brand new long password")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(e.adminsPath, []byte(accountsFile(changed)), 0o600))
	users, err := e.deps.Admins.Reload()
	require.NoError(t, err)
	assert.Equal(t, []string{"alice"}, users)

	assert.Equal(t, 1, countSessions(t, e))
	require.NoError(t, e.srv.RevokeSessions(context.Background(), users))
	assert.Zero(t, countSessions(t, e))
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Code)
}

func TestSessionEndsWhenAccountRemoved(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	bob := admins.Account{Username: "bob", Name: "Bob", Role: "Trésorier", PasswordHash: testHash()}
	require.NoError(t, os.WriteFile(e.adminsPath, []byte(accountsFile(bob)), 0o600))
	users, err := e.deps.Admins.Reload()
	require.NoError(t, err)
	assert.Equal(t, []string{"alice"}, users)

	rec := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/connexion", rec.Header().Get("Location"))
}

func TestConcurrentFailuresStopAtTheLimit(t *testing.T) {
	e := newTestEnv(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			e.postLogin(t, "alice", "wrong")
		})
	}
	wg.Wait()

	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT count FROM counters WHERE key = ?`, e.srv.limiter.userKey("alice")).Scan(&n))
	// Serialised: the 6th to 8th attempts hit the delay and are not counted.
	assert.Equal(t, userFailureLimit, n)
	assert.Equal(t, http.StatusTooManyRequests, e.postLogin(t, "alice", testPassword).Code)
}

func TestSessionExpiresAndIsPurged(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	e.clock.advance(31 * 24 * time.Hour)
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Code)

	e.login(t)
	e.clock.advance(32 * 24 * time.Hour)
	require.NoError(t, e.srv.Purge(context.Background()))
	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n))
	assert.Zero(t, n)
}

func TestLogoutNeedsCSRFAndEndsSession(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	token := e.csrf(t, cookie, "/")

	bad := e.do(t, http.MethodPost, adminHost, "/deconnexion", formBody(url.Values{"csrf": {"forged"}}), formType, withCookie(cookie))
	assert.Equal(t, http.StatusForbidden, bad.Code)

	rec := e.do(t, http.MethodPost, adminHost, "/deconnexion", formBody(url.Values{"csrf": {token}}), formType, withCookie(cookie))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/connexion", rec.Header().Get("Location"))
	assert.Negative(t, sessionCookie(t, rec).MaxAge)
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Code)
}

func TestCommitteeBanners(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	home := func() string {
		rec := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}

	assert.Contains(t, home(), "Le formulaire est fermé")

	e.importMembers(t, "members_valid.xlsx")
	assert.NotContains(t, home(), "Le formulaire est fermé")
	assert.NotContains(t, home(), "Pense à refaire l")

	e.clock.advance(15 * 24 * time.Hour)
	assert.Contains(t, home(), "Pense à refaire l")

	require.NoError(t, os.WriteFile(e.adminsPath, []byte("admins: [\n"), 0o600))
	_, err := e.deps.Admins.Reload()
	require.Error(t, err)
	body := home()
	assert.Contains(t, body, "Le fichier des comptes est invalide")
	assert.Contains(t, body, "Erreur : ")
	assert.Contains(t, body, "invalid YAML")
}

func TestRevokeStaleSessionsAtStart(t *testing.T) {
	e := newTestEnv(t, withPush(t))
	ctx := context.Background()
	kept := e.login(t)
	insert := func(token, username string, credential []byte) []byte {
		hash := secure.TokenHash(token)
		_, err := e.db.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (?, ?, ?, 1, 9999999999)`,
			hash, username, credential)
		require.NoError(t, err)
		return hash
	}
	removed := insert("removed-account", "bob", alice().CredentialHash())
	changed := insert("changed-password", "alice", []byte("an older password hash"))
	keys := browserKeys(t)
	sub, ok := push.ParseSubscription(e.deps.Config.PushAllowedHosts, keys.Get("endpoint"), keys.Get("p256dh"), keys.Get("auth"))
	require.True(t, ok)
	require.NoError(t, e.deps.Push.Save(ctx, removed, "bob", sub))

	require.NoError(t, e.srv.RevokeStale(ctx))
	assert.Equal(t, 1, e.count(t, "sessions"), "the account left the file or changed its password while the service was down")
	assert.Zero(t, e.count(t, "push_subscriptions"), "their subscriptions go with them")
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(kept)).Code, "a valid session stays")
	for _, hash := range [][]byte{removed, changed} {
		var n int
		require.NoError(t, e.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE token_hash = ?`, hash).Scan(&n))
		assert.Zero(t, n)
	}
}
