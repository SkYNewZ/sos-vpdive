package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/blobs"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/push"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

const (
	publicHost   = "sos.example.org"
	adminHost    = "comite.example.org"
	clubEmail    = "club@example.org"
	testPassword = "correct horse battery staple"
)

// testHash is computed once: argon2id with 64 MiB is slow on purpose.
var testHash = sync.OnceValue(func() string {
	h, err := admins.HashPassword(testPassword)
	if err != nil {
		return "unreachable"
	}
	return h
})

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

// fakeSender stands for the SMTP relay: it keeps every mail it accepts, or
// fails with err.
type fakeSender struct {
	mu   sync.Mutex
	sent []mail.Message
	err  error
}

func (f *fakeSender) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

type testEnv struct {
	srv        *Server
	deps       Deps
	db         *sql.DB
	logs       *syncBuffer
	clock      *testClock
	adminsPath string
	sender     *fakeSender
	blobs      blobs.Store
}

// syncBuffer collects logs written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func accountsFile(accounts ...admins.Account) string {
	var b strings.Builder
	b.WriteString("admins:\n")
	for _, a := range accounts {
		b.WriteString("  - username: " + a.Username + "\n    name: " + a.Name + "\n    role: " + a.Role +
			"\n    password_hash: \"" + a.PasswordHash + "\"\n")
	}
	return b.String()
}

func alice() admins.Account {
	return admins.Account{Username: "alice", Name: "Alice", Role: "Présidente", PasswordHash: testHash()}
}

func newTestEnv(t *testing.T, opts ...func(*Deps)) *testEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	require.NoError(t, store.CheckKey(ctx, db, keys))

	adminsPath := filepath.Join(dir, "admins.yaml")
	require.NoError(t, os.WriteFile(adminsPath, []byte(accountsFile(alice())), 0o600))
	logs := &syncBuffer{}
	logger := telemetry.NewLogger(logs, slog.LevelDebug)
	registry, err := admins.Load(adminsPath, logger)
	require.NoError(t, err)

	clock := &testClock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	cfg := &config.Config{
		Env:            config.EnvDevelopment,
		BaseURL:        mustURL(t, "https://"+publicHost),
		AdminBaseURL:   mustURL(t, "https://"+adminHost),
		VPDiveBaseURL:  mustURL(t, "https://vpdive.example.org"),
		NotifyEmail:    &netmail.Address{Address: clubEmail},
		MembersMaxAge:  336 * time.Hour,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		AgeWarnAfter:   48 * time.Hour,
		AgeAlertAfter:  168 * time.Hour,
		RetentionDays:  365,
		FormRateLimit:  20,
	}
	catalog, err := tickets.LoadCatalog(sosvpdive.Content)
	require.NoError(t, err)
	base, err := LoadKB(sosvpdive.Content, catalog)
	require.NoError(t, err)
	blobStore, err := blobs.NewDir(filepath.Join(dir, "captures"))
	require.NoError(t, err)
	memberStore := members.NewStore(db, keys, clock.now)
	outbox := mail.NewOutbox(db, keys, clock.now)
	broker := NewBroker()
	ticketStore := tickets.NewStore(tickets.Deps{
		DB: db, Keys: keys, Catalog: catalog, Members: memberStore, Outbox: outbox, Blobs: blobStore,
		Account: registry.Get, BaseURL: cfg.BaseURL, AdminBaseURL: cfg.AdminBaseURL, ClubEmail: clubEmail,
		RetentionDays: cfg.RetentionDays, Now: clock.now, Logger: logger, OnChange: broker.Publish,
	})
	deps := Deps{
		Config: cfg, DB: db, Keys: keys, Members: memberStore, Admins: registry,
		Tickets: ticketStore, Outbox: outbox, Push: push.NewStore(db, keys, clock.now), Broker: broker, KB: base,
		Content: sosvpdive.Content, Logger: logger, Now: clock.now,
	}
	for _, opt := range opts {
		opt(&deps)
	}
	srv, err := New(deps)
	require.NoError(t, err)
	return &testEnv{
		srv: srv, deps: deps, db: db, logs: logs, clock: clock, adminsPath: adminsPath,
		sender: &fakeSender{}, blobs: blobStore,
	}
}

// do sends a request to host. Mutations carry the host's Origin unless a
// mutator changes it.
func (e *testEnv) do(t *testing.T, method, host, target string, body io.Reader, mutators ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, body)
	req.Host = host
	req.RemoteAddr = "192.0.2.10:40000"
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "https://"+host)
	}
	for _, m := range mutators {
		m(req)
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func formBody(v url.Values) io.Reader { return strings.NewReader(v.Encode()) }

func formType(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-session" {
			return c
		}
	}
	t.Fatalf("no session cookie in %v", rec.Header().Values("Set-Cookie"))
	return nil
}

func (e *testEnv) postLogin(t *testing.T, username, password string, mutators ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	body := formBody(url.Values{"username": {username}, "password": {password}})
	return e.do(t, http.MethodPost, adminHost, "/connexion", body, append([]func(*http.Request){formType}, mutators...)...)
}

// login signs alice in and returns her session cookie.
func (e *testEnv) login(t *testing.T) *http.Cookie {
	t.Helper()
	rec := e.postLogin(t, "alice", testPassword)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	return sessionCookie(t, rec)
}

// csrf loads a committee page and returns its anti-CSRF token.
func (e *testEnv) csrf(t *testing.T, cookie *http.Cookie, path string) string {
	t.Helper()
	rec := e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	m := csrfPattern.FindStringSubmatch(rec.Body.String())
	require.NotNil(t, m, "no csrf field on %s", path)
	return m[1]
}

// importMembers imports a fixture directly through the members store.
func (e *testEnv) importMembers(t *testing.T, fixture string) {
	t.Helper()
	memberstest.Import(t, e.deps.Members, fixture)
}

// count counts the rows of a table.
func (e *testEnv) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n))
	return n
}

// pngBytes encodes a small valid PNG screenshot.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 3))))
	return buf.Bytes()
}

// multipartBody encodes values and files (field "captures") as a member form.
func multipartBody(t *testing.T, values url.Values, files ...[]byte) (io.Reader, func(*http.Request)) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, vs := range values {
		for _, v := range vs {
			require.NoError(t, mw.WriteField(name, v))
		}
	}
	for i, data := range files {
		fw, err := mw.CreateFormFile("captures", fmt.Sprintf("capture-%d.png", i+1))
		require.NoError(t, err)
		_, err = fw.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return &buf, func(r *http.Request) { r.Header.Set("Content-Type", mw.FormDataContentType()) }
}
