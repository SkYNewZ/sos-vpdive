package web

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/push"
)

const pushoverUserKey = "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"

// withPush turns Web Push and Pushover on, and gives alice a Pushover key.
func withPush(t *testing.T) func(*Deps) {
	t.Helper()
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), bytes.Repeat([]byte{3}, 32))
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	return func(d *Deps) {
		d.Config.VAPID = &config.VAPID{PrivateKey: key, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Subject: "mailto:club@example.org"}
		d.Config.PushAllowedHosts = []string{"fcm.googleapis.com", ".notify.windows.com"}
		d.Config.PushoverToken = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	}
}

// browserKeys is what PushManager gives: a P-256 public key and an auth secret.
func browserKeys(t *testing.T) url.Values {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(nil)
	require.NoError(t, err)
	return url.Values{
		"endpoint": {"https://fcm.googleapis.com/fcm/send/abc123"},
		"p256dh":   {base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())},
		"auth":     {base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))},
	}
}

func (e *testEnv) pushPost(t *testing.T, path string, cookie *http.Cookie, v url.Values, mutators ...func(*http.Request)) int {
	t.Helper()
	return e.do(t, http.MethodPost, adminHost, path, formBody(v), append([]func(*http.Request){formType, withCookie(cookie)}, mutators...)...).Code
}

func TestNotificationsPageStates(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	body := e.do(t, http.MethodGet, adminHost, "/notifications", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, body, "Aucune notification")
	assert.NotContains(t, body, "data-push")
	assert.Contains(t, e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String(), `href="/notifications"`, "in the nav")
	assert.Equal(t, http.StatusNotFound, e.pushPost(t, "/push/abonnement", cookie, browserKeys(t)), "no Web Push without VAPID")
	assert.Equal(t, http.StatusNotFound, e.pushPost(t, "/push/test", cookie, url.Values{}), "no test without VAPID")

	e = newTestEnv(t, withPush(t))
	cookie = e.login(t)
	body = e.do(t, http.MethodGet, adminHost, "/notifications", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, body, `data-vapid-key="`+e.deps.Config.VAPID.PublicKey+`"`)
	assert.Contains(t, body, `data-subscribed="false"`)
	assert.Contains(t, body, "dans le fichier des comptes", "alice has no Pushover key yet")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/notifications", nil).Code, "committee host only")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, publicHost, "/push/abonnement", formBody(browserKeys(t)), formType).Code)

	require.NoError(t, e.deps.Admins.SetPushoverKey(context.Background(), "alice", pushoverUserKey))
	body = e.do(t, http.MethodGet, adminHost, "/notifications", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, body, "Actif pour ton compte")
	assert.NotContains(t, body, pushoverUserKey, "the key never reaches a page")
}

// « M'envoyer une notification de test » pushes to this session's device
// and tells how it went, the push service's reason included.
func TestPushTestRoute(t *testing.T) {
	var (
		sessions [][]byte
		result   error
	)
	e := newTestEnv(t, withPush(t), func(d *Deps) {
		d.PushTest = func(_ context.Context, session []byte) error {
			sessions = append(sessions, session)
			return result
		}
	})
	cookie := e.login(t)
	valid := url.Values{"csrf": {e.csrf(t, cookie, "/notifications")}}
	assert.Equal(t, http.StatusForbidden, e.pushPost(t, "/push/test", cookie, url.Values{}), "no CSRF token")
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodPost, adminHost, "/push/test", formBody(valid), formType).Code, "no session")
	assert.Empty(t, sessions)

	send := func() string {
		t.Helper()
		r := e.do(t, http.MethodPost, adminHost, "/push/test", formBody(valid), formType, withCookie(cookie))
		require.Equal(t, http.StatusOK, r.Code)
		return r.Body.String()
	}
	assert.Contains(t, send(), "Notification envoyée")
	require.Len(t, sessions, 1)

	result = push.ErrNoSubscription
	assert.Contains(t, send(), "ne sont pas activées sur cet appareil")

	result = errors.New("push service refused the message: status 403 (BadJwtToken)")
	assert.Contains(t, send(), "status 403 (BadJwtToken)")
	assert.Contains(t, e.logs.String(), "test push not delivered")
}

func TestPushSubscriptionRoutes(t *testing.T) {
	e := newTestEnv(t, withPush(t))
	ctx := context.Background()
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/notifications")
	valid := browserKeys(t)
	valid.Set("csrf", csrf)

	assert.Equal(t, http.StatusForbidden, e.pushPost(t, "/push/abonnement", cookie, browserKeys(t)), "no CSRF token")
	assert.Equal(t, http.StatusForbidden, e.pushPost(t, "/push/abonnement", cookie, valid,
		func(r *http.Request) { r.Header.Set("Origin", "https://"+publicHost) }), "wrong origin")
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodPost, adminHost, "/push/abonnement", formBody(valid), formType).Code, "no session")
	for name, change := range map[string][2]string{
		"http endpoint":  {"endpoint", "http://fcm.googleapis.com/fcm/send/abc123"},
		"foreign host":   {"endpoint", "https://push.example.org/abc123"},
		"bare domain":    {"endpoint", "https://notify.windows.com/w/abc"},
		"long endpoint":  {"endpoint", "https://fcm.googleapis.com/" + strings.Repeat("a", 2048)},
		"not a point":    {"p256dh", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 65))},
		"short auth":     {"auth", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 15))},
		"not base64":     {"auth", "%%%"},
		"empty endpoint": {"endpoint", ""},
	} {
		v := maps.Clone(valid)
		v.Set(change[0], change[1])
		assert.Equal(t, http.StatusBadRequest, e.pushPost(t, "/push/abonnement", cookie, v), name)
	}
	assert.Zero(t, e.count(t, "push_subscriptions"))

	require.Equal(t, http.StatusNoContent, e.pushPost(t, "/push/abonnement", cookie, valid))
	assert.Contains(t, e.do(t, http.MethodGet, adminHost, "/notifications", nil, withCookie(cookie)).Body.String(), `data-subscribed="true"`)
	require.Equal(t, http.StatusNoContent, e.pushPost(t, "/push/desabonnement", cookie, url.Values{"csrf": {csrf}}))
	assert.Zero(t, e.count(t, "push_subscriptions"), "« Désactiver »")

	require.Equal(t, http.StatusNoContent, e.pushPost(t, "/push/abonnement", cookie, valid))
	require.Equal(t, http.StatusSeeOther, e.pushPost(t, "/deconnexion", cookie, url.Values{"csrf": {csrf}}))
	assert.Zero(t, e.count(t, "push_subscriptions"), "logging out ends this device's subscription")

	again := e.login(t)
	valid.Set("csrf", e.csrf(t, again, "/notifications"))
	require.Equal(t, http.StatusNoContent, e.pushPost(t, "/push/abonnement", again, valid))
	third := e.login(t)
	valid.Set("csrf", e.csrf(t, third, "/notifications"))
	require.Equal(t, http.StatusNoContent, e.pushPost(t, "/push/abonnement", third, valid))
	assert.Equal(t, 1, e.count(t, "push_subscriptions"), "the same browser after a new login: one row")

	require.NoError(t, e.deps.Admins.Delete(ctx, "alice"))
	assert.Zero(t, e.count(t, "push_subscriptions"), "removing an account deletes its subscriptions (spec §13)")
}
