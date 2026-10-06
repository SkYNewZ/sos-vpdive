package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// b64 decodes the base64url values of RFC 8291, whitespace included.
func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	require.NoError(t, err)
	return b
}

func TestEncryptMatchesRFC8291AppendixA(t *testing.T) {
	asPrivate, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	require.NoError(t, err)
	got, err := encrypt(
		b64(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24"),
		b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV- JvLexhqUzORcx aOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		b64(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		asPrivate,
		b64(t, "DGv6ra1nlYgDCS1FRnbzlw"),
	)
	require.NoError(t, err)
	want := append(
		b64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z 9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml mlMoZIIgDll6e3vCYLocInmYWAmS6Tlz AC8wEqKK6PBru3jl7A8"),
		b64(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrd irN6GcG7sFz1y1sqLgVi1VhjVkHsUoEs bI_0LpXMuGvnzQ")...)
	assert.Equal(t, want, got, "header (salt, record size, key id) then ciphertext, byte for byte")
}

func TestEncryptRefusesBadInput(t *testing.T) {
	key, err := ecdh.P256().GenerateKey(nil)
	require.NoError(t, err)
	ua, err := ecdh.P256().GenerateKey(nil)
	require.NoError(t, err)
	salt := make([]byte, 16)
	_, err = encrypt([]byte("x"), []byte("not a point"), make([]byte, 16), key, salt)
	require.Error(t, err)
	_, err = encrypt([]byte("x"), ua.PublicKey().Bytes(), make([]byte, 15), key, salt)
	require.Error(t, err, "the auth secret is 16 bytes")
	_, err = encrypt(make([]byte, maxPayload+1), ua.PublicKey().Bytes(), make([]byte, 16), key, salt)
	require.Error(t, err, "one record only")
	_, err = encrypt(make([]byte, maxPayload), ua.PublicKey().Bytes(), make([]byte, 16), key, salt)
	require.NoError(t, err)
}

func TestHostAllowed(t *testing.T) {
	hosts := []string{"fcm.googleapis.com", ".notify.windows.com"}
	for host, want := range map[string]bool{
		"fcm.googleapis.com":             true,
		"FCM.googleapis.com":             true,
		"wns2-par02p.notify.windows.com": true,
		"notify.windows.com":             false, // a leading dot means subdomains only
		"evil-fcm.googleapis.com":        false,
		"fcm.googleapis.com.evil.org":    false,
		"evilnotify.windows.com":         false,
		"":                               false,
	} {
		assert.Equal(t, want, HostAllowed(hosts, host), host)
	}
}

type testVAPID struct {
	cfg   *config.VAPID
	clock *testClock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestVAPID(t *testing.T) testVAPID {
	t.Helper()
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), bytes.Repeat([]byte{3}, 32))
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	return testVAPID{
		cfg:   &config.VAPID{PrivateKey: key, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Subject: "mailto:club@example.org"},
		clock: &testClock{t: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)},
	}
}

// checkJWT verifies a VAPID authorization header and returns its claims.
func checkJWT(t *testing.T, v testVAPID, header string) map[string]any {
	t.Helper()
	token, key, ok := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
	require.True(t, ok, header)
	require.Equal(t, v.cfg.PublicKey, key)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	var head map[string]string
	require.NoError(t, json.Unmarshal(b64(t, parts[0]), &head))
	assert.Equal(t, map[string]string{"typ": "JWT", "alg": "ES256"}, head)
	sig := b64(t, parts[2])
	require.Len(t, sig, 64, "ES256 signatures are r||s, not ASN.1")
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	assert.True(t, ecdsa.Verify(&v.cfg.PrivateKey.PublicKey, sum[:],
		new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])), "signature")
	var claims map[string]any
	require.NoError(t, json.Unmarshal(b64(t, parts[1]), &claims))
	return claims
}

func TestVAPIDTokenIsReusedTwelveHours(t *testing.T) {
	v := newTestVAPID(t)
	signer := newVAPID(v.cfg, v.clock.now)
	first, err := signer.authorization("https://web.push.apple.com")
	require.NoError(t, err)
	claims := checkJWT(t, v, first)
	assert.Equal(t, "https://web.push.apple.com", claims["aud"])
	assert.Equal(t, "mailto:club@example.org", claims["sub"])
	assert.InDelta(t, float64(v.clock.now().Add(12*time.Hour).Unix()), claims["exp"], 0)

	v.clock.advance(10 * time.Hour)
	again, err := signer.authorization("https://web.push.apple.com")
	require.NoError(t, err)
	assert.Equal(t, first, again, "Apple: never refresh more than once an hour")
	other, err := signer.authorization("https://fcm.googleapis.com")
	require.NoError(t, err)
	assert.NotEqual(t, first, other, "one token per push service")

	v.clock.advance(time.Hour + time.Second)
	renewed, err := signer.authorization("https://web.push.apple.com")
	require.NoError(t, err)
	assert.NotEqual(t, first, renewed, "renewed an hour before it expires")
}

// testSubscription is a browser key pair, so the test can decrypt.
func testSubscription(t *testing.T, endpoint string) (Subscription, *ecdh.PrivateKey) {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(nil)
	require.NoError(t, err)
	return Subscription{ID: 7, Endpoint: endpoint, P256DH: ua.PublicKey().Bytes(), Auth: bytes.Repeat([]byte{1}, 16)}, ua
}

// decrypt opens an aes128gcm body the way a browser does (RFC 8291).
func decrypt(t *testing.T, body []byte, ua *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	require.Greater(t, len(body), 86)
	salt, keyID, ciphertext := body[:16], body[21:86], body[86:]
	as, err := ecdh.P256().NewPublicKey(keyID)
	require.NoError(t, err)
	secret, err := ua.ECDH(as)
	require.NoError(t, err)
	gcm, nonce := contentCipher(t, secret, auth, ua.PublicKey().Bytes(), keyID, salt)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	require.NoError(t, err)
	require.Equal(t, byte(2), plain[len(plain)-1], "last-record delimiter")
	return plain[:len(plain)-1]
}

type pushService struct {
	*httptest.Server

	mu       sync.Mutex
	status   int
	requests []*http.Request
	bodies   [][]byte
}

func newPushService(t *testing.T, status int) *pushService {
	t.Helper()
	ps := &pushService{status: status}
	ps.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ps.mu.Lock()
		ps.requests, ps.bodies = append(ps.requests, r), append(ps.bodies, body)
		status := ps.status
		ps.mu.Unlock()
		if status == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", status)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *pushService) count() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.requests)
}

// newTestClient trusts the push service double and allows its host.
func newTestClient(v testVAPID, ps *pushService) *Client {
	c := NewClient(v.cfg, []string{"127.0.0.1"}, v.clock.now)
	c.http.Transport = ps.Client().Transport
	return c
}

func TestClientSendsAnEncryptedSignedMessage(t *testing.T) {
	v := newTestVAPID(t)
	ps := newPushService(t, http.StatusCreated)
	sub, ua := testSubscription(t, ps.URL+"/wpush/abc")
	payload := []byte(`{"title":"Nouvelle demande","body":"CPP-0042 · Autre","url":"/demandes/42"}`)

	require.NoError(t, newTestClient(v, ps).Send(context.Background(), sub, payload))
	require.Equal(t, 1, ps.count())
	r := ps.requests[0]
	assert.Equal(t, http.MethodPost, r.Method)
	assert.Equal(t, "/wpush/abc", r.URL.Path)
	assert.Equal(t, "aes128gcm", r.Header.Get("Content-Encoding"))
	assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
	assert.Equal(t, "86400", r.Header.Get("TTL"))
	assert.Empty(t, r.Header.Get("Traceparent"), "no trace context leaves")
	claims := checkJWT(t, v, r.Header.Get("Authorization"))
	assert.Equal(t, ps.URL, claims["aud"], "the audience is the push service origin")
	assert.Equal(t, payload, decrypt(t, ps.bodies[0], ua, sub.Auth))
}

func TestClientOutcomes(t *testing.T) {
	v := newTestVAPID(t)
	for status, want := range map[int]error{
		http.StatusOK:                    nil,
		http.StatusAccepted:              nil,
		http.StatusNotFound:              ErrGone,
		http.StatusGone:                  ErrGone,
		http.StatusForbidden:             errRejected,
		http.StatusRequestEntityTooLarge: errRejected,
		http.StatusInternalServerError:   errRejected,
		http.StatusFound:                 errRejected, // never followed
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ps := newPushService(t, status)
			sub, _ := testSubscription(t, ps.URL+"/wpush/abc")
			err := newTestClient(v, ps).Send(context.Background(), sub, []byte("{}"))
			if want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, want)
			}
			assert.Equal(t, 1, ps.count(), "one request, no redirect followed, no retry")
		})
	}
}

func TestClientRefusesEndpointsOutsideTheList(t *testing.T) {
	v := newTestVAPID(t)
	ps := newPushService(t, http.StatusCreated)
	c := newTestClient(v, ps)
	for _, endpoint := range []string{
		strings.Replace(ps.URL, "https://", "http://", 1) + "/wpush/abc", // not HTTPS
		strings.Replace(ps.URL, "127.0.0.1", "localhost", 1) + "/wpush/abc",
		"https://user@" + strings.TrimPrefix(ps.URL, "https://") + "/wpush/abc",
		"not a url\x7f",
	} {
		sub, _ := testSubscription(t, endpoint)
		require.ErrorIs(t, c.Send(context.Background(), sub, []byte("{}")), ErrEndpointRefused, endpoint)
	}
	assert.Zero(t, ps.count())
}

func TestClientGivesUpOnASlowService(t *testing.T) {
	v := newTestVAPID(t)
	release := make(chan struct{})
	slow := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); slow.Close() })
	c := NewClient(v.cfg, []string{"127.0.0.1"}, v.clock.now)
	c.http.Transport = slow.Client().Transport
	c.http.Timeout = 50 * time.Millisecond
	sub, _ := testSubscription(t, slow.URL+"/wpush/abc")
	err := c.Send(context.Background(), sub, []byte("{}"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrGone)
	assert.Equal(t, 10*time.Second, NewClient(v.cfg, nil, v.clock.now).http.Timeout, "the deployed timeout")
}
