package admins

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"html/template"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/admins/adminstest"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

const testPassword = adminstest.Password

func alice() Account {
	return Account{Username: "alice", Name: "Alice", Role: "Présidente", PasswordHash: adminstest.Hash}
}

func bob() Account {
	return Account{Username: "bob", Name: "Bob", Role: "Trésorier", PasswordHash: adminstest.Hash}
}

func testKeys(t *testing.T) *secure.Keys {
	t.Helper()
	keys, err := secure.NewKeys(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	return keys
}

func newRegistry(t *testing.T, accounts ...Account) (*Registry, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	r, err := Open(ctx, db, testKeys(t), filepath.Join(t.TempDir(), "avatars"), slog.New(slog.DiscardHandler), time.Now)
	require.NoError(t, err)
	for _, a := range accounts {
		require.NoError(t, r.Insert(ctx, a))
	}
	return r, db
}

func TestInsertSealsNameAndPushoverKey(t *testing.T) {
	a := alice()
	a.PushoverUserKey = strings.Repeat("k", 30)
	r, db := newRegistry(t, a)

	got, ok := r.Get("alice")
	require.True(t, ok)
	assert.Equal(t, "Alice", got.Name)
	assert.Equal(t, a.PushoverUserKey, got.PushoverUserKey)
	assert.NotEmpty(t, got.Avatar)

	var name, key []byte
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT name, pushover_user_key FROM accounts WHERE username = 'alice'`).Scan(&name, &key))
	assert.NotContains(t, string(name), "Alice")
	assert.NotContains(t, string(key), a.PushoverUserKey)
}

func TestInsertRefusesInvalidAndTaken(t *testing.T) {
	r, _ := newRegistry(t, alice())
	ctx := context.Background()
	require.ErrorIs(t, r.Insert(ctx, alice()), ErrTaken)
	for _, a := range []Account{
		{Username: "Alice", Name: "A", Role: "R"},
		{Username: "", Name: "A", Role: "R"},
		{Username: ".", Name: "A", Role: "R"},  // browsers normalise /comptes/. away
		{Username: "..", Name: "A", Role: "R"}, // and /comptes/..
		{Username: "carol", Name: " ", Role: "R"},
		{Username: "carol", Name: "Carol", Role: ""},
	} {
		assert.ErrorIs(t, r.Insert(ctx, a), ErrInvalid, a)
	}
}

func TestCreateGivesATemporaryPassword(t *testing.T) {
	r, _ := newRegistry(t)
	password, err := r.Create(context.Background(), "carol", " Carol ", "Secrétaire")
	require.NoError(t, err)

	a, ok := r.Get("carol")
	require.True(t, ok)
	assert.Equal(t, "Carol", a.Name)
	assert.True(t, a.MustChangePassword)
	ok, err = VerifyPassword(a.PasswordHash, password)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTemporaryPasswordShape(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		p := TemporaryPassword()
		assert.Regexp(t, `^[a-km-np-z2-9]{4}(-[a-km-np-z2-9]{4}){3}$`, p)
		assert.False(t, seen[p])
		seen[p] = true
	}
}

func TestResetPasswordCallsOnChange(t *testing.T) {
	r, _ := newRegistry(t, alice(), bob())
	calls := 0
	r.OnChange = func(context.Context) { calls++ }

	password, err := r.ResetPassword(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	a, _ := r.Get("alice")
	assert.True(t, a.MustChangePassword)
	ok, err := VerifyPassword(a.PasswordHash, password)
	require.NoError(t, err)
	assert.True(t, ok)

	_, err = r.ResetPassword(context.Background(), "carol")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestChangePasswordKeepsOneSession(t *testing.T) {
	a := alice()
	a.MustChangePassword = true
	r, db := newRegistry(t, a)
	ctx := context.Background()
	for _, token := range []string{"kept", "other"} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (?, 'alice', ?, 0, 0)`,
			[]byte(token), a.CredentialHash())
		require.NoError(t, err)
	}

	_, err := r.ChangePassword(ctx, a, "short", []byte("kept"))
	require.ErrorIs(t, err, ErrTooShort)
	_, err = r.ChangePassword(ctx, a, testPassword, []byte("kept"))
	require.ErrorIs(t, err, ErrSamePassword)
	_, err = r.ChangePassword(ctx, Account{Username: "carol", PasswordHash: adminstest.Hash}, "a brand new password", nil)
	require.ErrorIs(t, err, ErrPasswordChanged, "no such account")

	hash, err := r.ChangePassword(ctx, a, "a brand new password", []byte("kept"))
	require.NoError(t, err)
	got, _ := r.Get("alice")
	assert.False(t, got.MustChangePassword)
	assert.Equal(t, got.PasswordHash, hash)
	for token, valid := range map[string]bool{"kept": true, "other": false} {
		var credential []byte
		require.NoError(t, db.QueryRowContext(ctx, `SELECT credential_hash FROM sessions WHERE token_hash = ?`, []byte(token)).Scan(&credential))
		_, ok := r.Current("alice", credential)
		assert.Equal(t, valid, ok, token)
	}
}

func TestSetPushoverKey(t *testing.T) {
	r, _ := newRegistry(t, alice())
	ctx := context.Background()
	key := strings.Repeat("a1", 15)
	require.ErrorIs(t, r.SetPushoverKey(ctx, "alice", "too short"), ErrPushoverKey)
	require.NoError(t, r.SetPushoverKey(ctx, "alice", key))
	a, _ := r.Get("alice")
	assert.Equal(t, key, a.PushoverUserKey)
	require.NoError(t, r.SetPushoverKey(ctx, "alice", ""))
	a, _ = r.Get("alice")
	assert.Empty(t, a.PushoverUserKey)
}

// avatarFiles lists the photo files of r.
func avatarFiles(t *testing.T, r *Registry) []string {
	t.Helper()
	entries, err := fs.ReadDir(r.avatars.FS(), ".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSetAvatarReplacesTheFile(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t, alice())
	drawn, _ := r.Get("alice")
	assert.False(t, drawn.HasPhoto())

	require.NoError(t, r.SetAvatar(ctx, "alice", []byte("first photo")))
	first := avatarFiles(t, r)
	require.Len(t, first, 1)
	got, _ := r.Get("alice")
	assert.True(t, got.HasPhoto())
	assert.Equal(t, template.URL("data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString([]byte("first photo"))), got.Avatar)
	sealed, err := r.avatars.ReadFile(first[0])
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), "first photo", "the file is sealed")

	require.NoError(t, r.SetAvatar(ctx, "alice", []byte("second photo")))
	second := avatarFiles(t, r)
	require.Len(t, second, 1, "the old file is gone")
	assert.NotEqual(t, first, second)

	require.NoError(t, r.SetAvatar(ctx, "alice", nil))
	assert.Empty(t, avatarFiles(t, r))
	got, _ = r.Get("alice")
	assert.False(t, got.HasPhoto())
	assert.Equal(t, drawn.Avatar, got.Avatar, "back to the drawn avatar")

	require.ErrorIs(t, r.SetAvatar(ctx, "nobody", []byte("photo")), ErrNotFound)
	assert.Empty(t, avatarFiles(t, r), "a refused change leaves no file")
}

func TestDeleteRemovesAvatar(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t, alice(), bob())
	require.NoError(t, r.SetAvatar(ctx, "bob", []byte("photo")))
	require.NoError(t, r.Delete(ctx, "bob"))
	assert.Empty(t, avatarFiles(t, r))
}

// A database restored without the photo files must still load its accounts.
func TestMissingAvatarFallsBackToDrawn(t *testing.T) {
	ctx := context.Background()
	r, db := newRegistry(t, alice())
	drawn, _ := r.Get("alice")
	require.NoError(t, r.SetAvatar(ctx, "alice", []byte("photo")))

	restarted, err := Open(ctx, db, testKeys(t), t.TempDir(), slog.New(slog.DiscardHandler), time.Now)
	require.NoError(t, err)
	got, ok := restarted.Get("alice")
	require.True(t, ok)
	assert.Equal(t, drawn.Avatar, got.Avatar)
	assert.True(t, got.HasPhoto(), "« Retirer ma photo » stays offered")
}

func TestDeleteCallsOnChange(t *testing.T) {
	r, _ := newRegistry(t, alice(), bob())
	calls := 0
	r.OnChange = func(context.Context) { calls++ }
	require.NoError(t, r.Delete(context.Background(), "bob"))
	assert.Equal(t, 1, calls)
	_, ok := r.Get("bob")
	assert.False(t, ok)
	assert.ErrorIs(t, r.Delete(context.Background(), "bob"), ErrNotFound)
}

// Inserting an account changes no session: OnChange is not called.
func TestInsertDoesNotCallOnChange(t *testing.T) {
	r, _ := newRegistry(t)
	r.OnChange = func(context.Context) { t.Error("OnChange called on insert") }
	require.NoError(t, r.Insert(context.Background(), alice()))
}

// reset-password writes from another process: Reload reports the account and
// Watch hands the change to OnChange.
func TestWatchSeesChangesFromAnotherProcess(t *testing.T) {
	r, db := newRegistry(t, alice(), bob())
	other, err := Open(context.Background(), db, testKeys(t), t.TempDir(), slog.New(slog.DiscardHandler), time.Now)
	require.NoError(t, err)

	got := make(chan struct{}, 1)
	r.OnChange = func(context.Context) { got <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Watch(ctx, 10*time.Millisecond)
		close(done)
	}()

	require.NoError(t, other.Delete(context.Background(), "bob"))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("OnChange not called")
	}
	cancel()
	<-done
	_, ok := r.Get("bob")
	assert.False(t, ok)
}

func TestReloadReportsRemovedAndChangedAccounts(t *testing.T) {
	r, db := newRegistry(t, alice(), bob())
	ctx := context.Background()
	changed, err := r.Reload(ctx)
	require.NoError(t, err)
	assert.Empty(t, changed)

	_, err = db.ExecContext(ctx, `UPDATE accounts SET password_hash = 'x' WHERE username = 'alice'`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM accounts WHERE username = 'bob'`)
	require.NoError(t, err)
	changed, err = r.Reload(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, changed)
}

func TestAccountsAreSortedByName(t *testing.T) {
	r, _ := newRegistry(t, bob(), alice())
	accounts := r.Accounts()
	require.Len(t, accounts, 2)
	assert.Equal(t, "alice", accounts[0].Username)
	assert.Equal(t, "bob", accounts[1].Username)
}

func TestCurrentChecksAccountAndPassword(t *testing.T) {
	r, _ := newRegistry(t, alice())
	a, ok := r.Current("alice", alice().CredentialHash())
	require.True(t, ok)
	assert.Equal(t, "Alice", a.Name)
	_, ok = r.Current("alice", []byte("an older password hash"))
	assert.False(t, ok, "the password changed since the session began")
	_, ok = r.Current("bob", bob().CredentialHash())
	assert.False(t, ok, "no such account")
}

// A write whose request ends right after the commit still revokes sessions:
// OnChange never gets a cancelled context. sync is called directly because a
// cancelled context makes the write's own transaction fail first.
func TestSyncIgnoresTheCallersCancellation(t *testing.T) {
	r, db := newRegistry(t, alice(), bob())
	_, err := db.ExecContext(context.Background(), `DELETE FROM accounts WHERE username = 'bob'`)
	require.NoError(t, err)
	got := context.Canceled
	r.OnChange = func(ctx context.Context) { got = ctx.Err() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, r.sync(ctx))
	require.NoError(t, got, "OnChange ran with a live context")
	_, ok := r.Get("bob")
	assert.False(t, ok)
}

func TestChangePasswordRefusesAPasswordChangedMeanwhile(t *testing.T) {
	r, db := newRegistry(t, alice())
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `UPDATE accounts SET password_hash = 'reset by the owner' WHERE username = 'alice'`)
	require.NoError(t, err)
	_, err = r.ChangePassword(ctx, alice(), "a brand new password", nil)
	require.ErrorIs(t, err, ErrPasswordChanged)
	var hash string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT password_hash FROM accounts WHERE username = 'alice'`).Scan(&hash))
	assert.Equal(t, "reset by the owner", hash)
}

// A reload that keeps failing (every Watch tick) logs once, not every tick;
// it logs again after a successful reload.
func TestWatchLogsAPersistentErrorOnce(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	var logs bytes.Buffer
	r, err := Open(ctx, db, testKeys(t), t.TempDir(), slog.New(slog.NewTextHandler(&logs, nil)), time.Now)
	require.NoError(t, err)
	require.NoError(t, r.Insert(ctx, alice()))
	corrupt := func() {
		_, err := db.ExecContext(ctx, `UPDATE accounts SET name = x'00' WHERE username = 'alice'`)
		require.NoError(t, err)
	}

	corrupt()
	failure := r.poll(ctx, "")
	failure = r.poll(ctx, failure)
	assert.Equal(t, 1, strings.Count(logs.String(), "reload accounts"))
	_, err = db.ExecContext(ctx, `DELETE FROM accounts`)
	require.NoError(t, err)
	failure = r.poll(ctx, failure)
	assert.Empty(t, failure)
	require.NoError(t, r.Insert(ctx, alice()))
	corrupt()
	r.poll(ctx, failure)
	assert.Equal(t, 2, strings.Count(logs.String(), "reload accounts"))
}

// The owner resets the password after the request authenticated with the old
// one: the change made under the old password must not overwrite the reset.
func TestChangePasswordIsBoundToTheAuthenticatedPassword(t *testing.T) {
	r, db := newRegistry(t, alice())
	ctx := context.Background()
	authenticated, _ := r.Get("alice")
	_, err := db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES ('kept', 'alice', ?, 0, 0)`,
		authenticated.CredentialHash())
	require.NoError(t, err)
	_, err = r.ResetPassword(ctx, "alice")
	require.NoError(t, err)
	reset, _ := r.Get("alice")

	_, err = r.ChangePassword(ctx, authenticated, "a brand new password", []byte("kept"))
	require.ErrorIs(t, err, ErrPasswordChanged)
	got, _ := r.Get("alice")
	assert.Equal(t, reset.PasswordHash, got.PasswordHash, "the reset stays in place")
}

// The kept session must still be the one that authenticated, in the same
// transaction: a session revoked meanwhile gets no new password.
func TestChangePasswordNeedsTheKeptSession(t *testing.T) {
	r, db := newRegistry(t, alice())
	ctx := context.Background()
	authenticated, _ := r.Get("alice")
	_, err := db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES ('changed', 'alice', 'another credential', 0, 0)`)
	require.NoError(t, err)

	for _, token := range []string{"gone", "changed"} {
		_, err = r.ChangePassword(ctx, authenticated, "a brand new password", []byte(token))
		require.ErrorIs(t, err, ErrPasswordChanged, token)
		got, _ := r.Get("alice")
		assert.Equal(t, authenticated.PasswordHash, got.PasswordHash, token)
	}
}
