package web

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	publicHost   = "sos.example.org"
	adminHost    = "comite.example.org"
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

//nolint:unused // used by the session tests of Task 12
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testEnv struct {
	srv        *Server
	deps       Deps
	db         *sql.DB
	logs       *syncBuffer
	clock      *testClock
	adminsPath string
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

//nolint:unparam // Task 12 passes options
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
	deps := Deps{
		Config: &config.Config{
			Env:            config.EnvDevelopment,
			BaseURL:        mustURL(t, "https://"+publicHost),
			AdminBaseURL:   mustURL(t, "https://"+adminHost),
			VPDiveBaseURL:  mustURL(t, "https://vpdive.example.org"),
			NotifyEmail:    &mail.Address{Address: "club@example.org"},
			MembersMaxAge:  336 * time.Hour,
			TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		},
		DB:      db,
		Keys:    keys,
		Members: members.NewStore(db, keys, clock.now),
		Admins:  registry,
		Content: sosvpdive.Content,
		Logger:  logger,
		Now:     clock.now,
	}
	for _, opt := range opts {
		opt(&deps)
	}
	srv, err := New(deps)
	require.NoError(t, err)
	return &testEnv{srv: srv, deps: deps, db: db, logs: logs, clock: clock, adminsPath: adminsPath}
}

// do sends a request to host. Mutations carry the host's Origin unless a
// mutator changes it.
//
//nolint:unparam // Task 12 sends request bodies
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
