package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

var testHash = sync.OnceValue(func() string {
	h, err := admins.HashPassword("correct horse battery staple")
	if err != nil {
		return "unreachable"
	}
	return h
})

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

// devEnv is a complete development environment rooted in a temp directory.
func devEnv(t *testing.T, port int) map[string]string {
	t.Helper()
	dir := t.TempDir()
	adminsPath := filepath.Join(dir, "admins.yaml")
	require.NoError(t, os.WriteFile(adminsPath, []byte("admins:\n  - username: alice\n    name: Alice\n    role: Présidente\n    password_hash: \""+testHash()+"\"\n"), 0o600))
	return map[string]string{
		"APP_ENV":        "development",
		"BASE_URL":       "http://sos.localhost:" + strconv.Itoa(port),
		"ADMIN_BASE_URL": "http://comite.localhost:" + strconv.Itoa(port),
		"SECRET_KEY":     key(5),
		"PORT":           strconv.Itoa(port),
		"DATA_DIR":       filepath.Join(dir, "data"),
		"ADMINS_FILE":    adminsPath,
		"SMTP_HOST":      "smtp.example.org",
		"SMTP_PORT":      "465",
		"SMTP_USERNAME":  "user",
		"SMTP_PASSWORD":  "pass",
		"MAIL_FROM":      "support@example.org",
		"NOTIFY_EMAIL":   "club@example.org",
	}
}

func getenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestUsage(t *testing.T) {
	var u usageError
	require.ErrorAs(t, run(context.Background(), nil, getenv(nil), nil, &bytes.Buffer{}), &u)
	require.ErrorAs(t, run(context.Background(), []string{"dance"}, getenv(nil), nil, &bytes.Buffer{}), &u)
	require.ErrorAs(t, run(context.Background(), []string{"backup"}, getenv(nil), nil, &bytes.Buffer{}), &u)
}

func TestServeNamesMissingVariable(t *testing.T) {
	env := devEnv(t, freePort(t))
	delete(env, "SECRET_KEY")
	err := run(context.Background(), []string{"serve"}, getenv(env), nil, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SECRET_KEY")
}

func TestSetupRefusesAnotherKey(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	require.NoError(t, a.close())

	env["SECRET_KEY"] = key(6)
	other, err := config.Load(getenv(env))
	require.NoError(t, err)
	_, err = setup(ctx, other, quietLogger())
	require.ErrorIs(t, err, store.ErrWrongKey)
	assert.Contains(t, err.Error(), "SECRET_KEY does not match")
}

func TestSetupRefusesInvalidAccountsFile(t *testing.T) {
	env := devEnv(t, freePort(t))
	require.NoError(t, os.WriteFile(env["ADMINS_FILE"], []byte("admins: []\n"), 0o600))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	_, err = setup(context.Background(), cfg, quietLogger())
	require.ErrorContains(t, err, "no account")
}

func TestHashPasswordFromPipe(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"hash-password"}, getenv(nil), strings.NewReader("a long enough password\n"), &out))
	hash := strings.TrimSpace(out.String())
	ok, err := admins.VerifyPassword(hash, "a long enough password")
	require.NoError(t, err)
	assert.True(t, ok)

	err = run(context.Background(), []string{"hash-password"}, getenv(nil), strings.NewReader("short\n"), &bytes.Buffer{})
	require.ErrorContains(t, err, "at least 12")
}

func TestBackupThenRestoreOnABlankMachine(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	importFixture(t, a.members)
	require.NoError(t, a.close())

	backupFile := filepath.Join(t.TempDir(), "support-backup.db")
	require.NoError(t, run(ctx, []string{"backup", backupFile}, getenv(env), nil, &bytes.Buffer{}))

	blank := devEnv(t, freePort(t))
	blank["SECRET_KEY"] = env["SECRET_KEY"]
	require.NoError(t, run(ctx, []string{"restore", backupFile}, getenv(blank), nil, &bytes.Buffer{}))
	restoredCfg, err := config.Load(getenv(blank))
	require.NoError(t, err)
	restored, err := setup(ctx, restoredCfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, restored.close()) }()
	found, err := restored.members.Lookup(ctx, "lea.martin@example.org")
	require.NoError(t, err)
	assert.True(t, found)

	wrong := devEnv(t, freePort(t))
	wrong["SECRET_KEY"] = key(7)
	require.ErrorIs(t, run(ctx, []string{"restore", backupFile}, getenv(wrong), nil, &bytes.Buffer{}), store.ErrWrongKey)
}

func TestBackupRefusesMissingDatabase(t *testing.T) {
	env := devEnv(t, freePort(t))
	env["DATA_DIR"] = t.TempDir()
	backupFile := filepath.Join(t.TempDir(), "backup.db")
	err := run(context.Background(), []string{"backup", backupFile}, getenv(env), nil, &bytes.Buffer{})
	require.ErrorContains(t, err, "no database to back up")
	entries, err := os.ReadDir(env["DATA_DIR"])
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.NoFileExists(t, backupFile)
}

func TestServeAnswersHealthcheckAndStops(t *testing.T) {
	env := devEnv(t, freePort(t))
	ctx, cancel := context.WithCancel(context.Background())
	logs := &lockedBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve"}, getenv(env), nil, logs) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		err := run(context.Background(), []string{"healthcheck"}, getenv(env), nil, &bytes.Buffer{})
		if err == nil {
			break
		}
		require.True(t, time.Now().Before(deadline), "server never became healthy: %v\n%s", err, logs.String())
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	assert.Contains(t, logs.String(), `"msg":"listening"`)
	assert.NotContains(t, logs.String(), env["SECRET_KEY"])
}

func importFixture(t *testing.T, s *members.Store) {
	t.Helper()
	memberstest.Import(t, s, "members_valid.xlsx")
}
