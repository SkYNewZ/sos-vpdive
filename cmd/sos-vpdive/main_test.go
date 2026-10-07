package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
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
	return map[string]string{
		"APP_ENV":        "development",
		"BASE_URL":       "http://sos.localhost:" + strconv.Itoa(port),
		"ADMIN_BASE_URL": "http://comite.localhost:" + strconv.Itoa(port),
		"SECRET_KEY":     key(5),
		"PORT":           strconv.Itoa(port),
		"DATA_DIR":       filepath.Join(dir, "data"),
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
	require.NoError(t, a.db.Close())

	env["SECRET_KEY"] = key(6)
	other, err := config.Load(getenv(env))
	require.NoError(t, err)
	_, err = setup(ctx, other, quietLogger())
	require.ErrorIs(t, err, store.ErrWrongKey)
	assert.Contains(t, err.Error(), "SECRET_KEY does not match")
}

func TestBackupThenRestoreOnABlankMachine(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	importFixture(t, a.members)
	require.NoError(t, a.db.Close())

	backupFile := filepath.Join(t.TempDir(), "support-backup.db")
	require.NoError(t, run(ctx, []string{"backup", backupFile}, getenv(env), nil, &bytes.Buffer{}))

	blank := devEnv(t, freePort(t))
	blank["SECRET_KEY"] = env["SECRET_KEY"]
	require.NoError(t, run(ctx, []string{"restore", backupFile}, getenv(blank), nil, &bytes.Buffer{}))
	restoredCfg, err := config.Load(getenv(blank))
	require.NoError(t, err)
	restored, err := setup(ctx, restoredCfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, restored.db.Close()) }()
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
	env["UMAMI_SCRIPT_URL"] = "analytics.example.org/script.js" // invalid: Umami off, server up
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
	assert.Contains(t, logs.String(), `"version":"dev"`)
	assert.Contains(t, logs.String(), `"level":"WARN","msg":"configuration ignored","error":"UMAMI_SCRIPT_URL: `)
	assert.NotContains(t, logs.String(), env["SECRET_KEY"])
}

func importFixture(t *testing.T, s *members.Store) {
	t.Helper()
	memberstest.Import(t, s, "members_valid.xlsx")
}

// addAccounts stores committee accounts whose password hashes to testHash.
func addAccounts(t *testing.T, r *admins.Registry, usernames ...string) {
	t.Helper()
	for _, u := range usernames {
		require.NoError(t, r.Insert(context.Background(), admins.Account{
			Username: u, Name: strings.ToUpper(u[:1]) + u[1:], Role: "Membre du comité", PasswordHash: testHash(),
		}))
	}
}

// An account removed while the service was stopped must not keep requests:
// setup releases them before serving (spec §4.1, §8.1).
func TestSetupReleasesRequestsOfAccountsRemovedWhileStopped(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)

	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	addAccounts(t, a.admins, "alice", "bob")
	importFixture(t, a.members)
	_, err = a.tickets.Submit(ctx, tickets.Submission{
		FormKey:     "acceptance-form-key",
		FirstName:   "Léa",
		LastName:    "Martin",
		Email:       "lea.martin@example.org",
		Fields:      tickets.Fields{Category: "autre", Values: map[string]string{}},
		Description: "Je ne retrouve pas mon inscription à la sortie de samedi.",
	}, nil)
	require.NoError(t, err)
	rows, err := a.tickets.Board(ctx, tickets.Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	d, err := a.tickets.Detail(ctx, rows[0].ID)
	require.NoError(t, err)
	require.NoError(t, a.tickets.Apply(ctx, tickets.Command{
		Action: tickets.ActionTake, TicketID: d.ID, Version: d.Version, Actor: "bob",
	}))
	// A restored backup can lack an account that held requests.
	_, err = a.db.ExecContext(ctx, `DELETE FROM accounts WHERE username = 'bob'`)
	require.NoError(t, err)
	require.NoError(t, a.db.Close())

	again, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, again.db.Close()) }()
	d, err = again.tickets.Detail(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, tickets.StatusTodo, d.Status)
	assert.Empty(t, d.Assignee)
}

func TestValidateKBNeedsNoEnvironment(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"validate-kb"}, getenv(nil), nil, &out))
	// Shape only: the fiches and their marks change with the content.
	assert.Regexp(t, `^(warning: kb/[a-z0-9-]+\.md: \d+ \[À COMPLÉTER\] mark\(s\) to fill in\n)*\d+ fiches are valid\n$`, out.String())
}

// withVAPIDKeys runs vapid-keys and copies its variables into env.
func withVAPIDKeys(t *testing.T, env map[string]string) string {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"vapid-keys"}, getenv(nil), nil, &out))
	for line := range strings.SplitSeq(out.String(), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(name, "#") {
			env[name] = value
		}
	}
	env["VAPID_SUBJECT"] = "mailto:club@example.org"
	return out.String()
}

func TestVAPIDKeysGivesAPairTheConfigurationAccepts(t *testing.T) {
	env := devEnv(t, freePort(t))
	out := withVAPIDKeys(t, env)
	require.NotEmpty(t, env["VAPID_PUBLIC_KEY"])
	require.NotEmpty(t, env["VAPID_PRIVATE_KEY"])
	assert.Contains(t, out, "# Add VAPID_SUBJECT", "the output reminds the third variable")
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	require.NotNil(t, cfg.VAPID)
	assert.NotEqual(t, out, withVAPIDKeys(t, map[string]string{}), "every run draws a new pair")
}

func TestSetupWiresTheConfiguredAlertChannels(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	assert.Empty(t, a.tickets.Alerts, "no alert without configuration")
	assert.Equal(t, []mail.Channel{mail.ChannelEmail}, slices.Sorted(maps.Keys(a.senders)))
	require.NoError(t, a.db.Close())

	withVAPIDKeys(t, env)
	env["PUSHOVER_APP_TOKEN"] = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	cfg, err = config.Load(getenv(env))
	require.NoError(t, err)
	a, err = setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, a.db.Close()) }()
	assert.Equal(t, []mail.Channel{mail.ChannelPushover, mail.ChannelWebPush}, a.tickets.Alerts)
	assert.Equal(t, []mail.Channel{mail.ChannelEmail, mail.ChannelPushover, mail.ChannelWebPush}, slices.Sorted(maps.Keys(a.senders)))
}

func TestSetupRevokesSessionsOfAccountsChangedWhileStopped(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	_, err = a.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (X'01', 'bob', X'02', 1, 9999999999)`)
	require.NoError(t, err)
	require.NoError(t, a.db.Close())

	again, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, again.db.Close()) }()
	var n int
	require.NoError(t, again.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n))
	assert.Zero(t, n, "bob is not an account")
}

var temporaryPattern = regexp.MustCompile(`[a-z2-9]{4}(?:-[a-z2-9]{4}){3}`)

func TestResetPasswordCreatesThenResets(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	var out bytes.Buffer
	require.ErrorContains(t, run(ctx, []string{"reset-password", "carol"}, getenv(env), nil, &out), "-name and -role")
	// The username is trimmed and lowercased, like at sign-in.
	require.NoError(t, run(ctx, []string{"reset-password", "-name", "Carol", "-role", "Secrétaire", " Carol "}, getenv(env), nil, &out))
	assert.Contains(t, out.String(), "Temporary password for carol:")
	first := temporaryPattern.FindString(out.String())
	require.NotEmpty(t, first, out.String())

	out.Reset()
	require.ErrorContains(t, run(ctx, []string{"reset-password", "-name", "Caro", "carol"}, getenv(env), nil, &out), "never change")
	require.NoError(t, run(ctx, []string{"reset-password", "carol"}, getenv(env), nil, &out))
	second := temporaryPattern.FindString(out.String())
	require.NotEmpty(t, second)
	assert.NotEqual(t, first, second)

	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, a.db.Close()) }()
	acc, ok := a.admins.Get("carol")
	require.True(t, ok)
	assert.Equal(t, "Carol", acc.Name)
	assert.True(t, acc.MustChangePassword)
	ok, err = admins.VerifyPassword(acc.PasswordHash, second)
	require.NoError(t, err)
	assert.True(t, ok)
}

// reset-password run while the service is stopped: the next start ends the
// account's sessions.
func TestResetPasswordWhileStoppedEndsSessions(t *testing.T) {
	ctx := context.Background()
	env := devEnv(t, freePort(t))
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	a, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	addAccounts(t, a.admins, "alice")
	alice, _ := a.admins.Get("alice")
	_, err = a.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (x'01', 'alice', ?, 1, 9999999999)`,
		alice.CredentialHash())
	require.NoError(t, err)
	require.NoError(t, a.db.Close())

	require.NoError(t, run(ctx, []string{"reset-password", "alice"}, getenv(env), nil, &bytes.Buffer{}))
	again, err := setup(ctx, cfg, quietLogger())
	require.NoError(t, err)
	defer func() { assert.NoError(t, again.db.Close()) }()
	var n int
	require.NoError(t, again.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n))
	assert.Zero(t, n)
}

func TestSetupWarnsWhenTheOwnerAccountIsMissing(t *testing.T) {
	env := devEnv(t, freePort(t))
	env["OWNER_USERNAME"] = "alice"
	cfg, err := config.Load(getenv(env))
	require.NoError(t, err)
	var logs bytes.Buffer
	a, err := setup(context.Background(), cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	defer func() { assert.NoError(t, a.db.Close()) }()
	assert.Contains(t, logs.String(), "owner account missing")
	assert.NotContains(t, logs.String(), "alice", "no username in logs")
}
