package admins

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func newRegistry(t *testing.T, content string) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admins.yaml")
	writeFile(t, path, content)
	r, err := Load(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return r, path
}

func TestLoadRefusesInvalidFileAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admins.yaml")
	writeFile(t, path, "admins: []\n")
	_, err := Load(path, slog.Default())
	require.Error(t, err)
	_, err = Load(filepath.Join(t.TempDir(), "missing.yaml"), slog.Default())
	require.Error(t, err)
}

func TestReloadReportsRemovedAndChangedAccounts(t *testing.T) {
	r, path := newRegistry(t, accountsYAML(alice(), bob()))

	changed, err := r.Reload()
	require.NoError(t, err)
	assert.Empty(t, changed, "unchanged file")

	renamed := alice()
	renamed.Name = "Alice M."
	writeFile(t, path, accountsYAML(renamed, bob()))
	changed, err = r.Reload()
	require.NoError(t, err)
	assert.Empty(t, changed, "a new display name keeps sessions")
	a, _ := r.Get("alice")
	assert.Equal(t, "Alice M.", a.Name)

	newPassword := alice()
	newPassword.PasswordHash, err = HashPassword("another long password")
	require.NoError(t, err)
	writeFile(t, path, accountsYAML(newPassword))
	changed, err = r.Reload()
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, changed)
	_, ok := r.Get("bob")
	assert.False(t, ok)
}

// A file read while an editor rewrites it keeps the
// previous accounts active.
func TestReloadKeepsAccountsWhenFileBecomesInvalid(t *testing.T) {
	r, path := newRegistry(t, accountsYAML(alice()))
	original, err := os.ReadFile(path)
	require.NoError(t, err)

	for _, content := range []string{"", "admins:\n  - username: al"} {
		writeFile(t, path, content)
		_, err := r.Reload()
		require.Error(t, err)
		require.Error(t, r.Err())
		_, ok := r.Get("alice")
		assert.True(t, ok)
	}

	writeFile(t, path, string(original))
	_, err = r.Reload()
	require.NoError(t, err)
	assert.NoError(t, r.Err())
}

func TestWatchCallsOnChangeAndStops(t *testing.T) {
	r, path := newRegistry(t, accountsYAML(alice(), bob()))
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan []string, 1)
	done := make(chan struct{})
	go func() {
		r.Watch(ctx, 10*time.Millisecond, func(_ context.Context, users []string) { got <- users })
		close(done)
	}()

	writeFile(t, path, accountsYAML(alice()))
	select {
	case users := <-got:
		assert.Equal(t, []string{"bob"}, users)
	case <-time.After(5 * time.Second):
		t.Fatal("onChange not called")
	}
	cancel()
	<-done
}

func TestAccountsAreSortedByName(t *testing.T) {
	r, _ := newRegistry(t, accountsYAML(bob(), alice()))
	accounts := r.Accounts()
	require.Len(t, accounts, 2)
	assert.Equal(t, "alice", accounts[0].Username)
	assert.Equal(t, "bob", accounts[1].Username)
}

func TestCurrentChecksAccountAndPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admins.yaml")
	require.NoError(t, os.WriteFile(path, []byte(accountsYAML(alice())), 0o600))
	r, err := Load(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	a, ok := r.Current("alice", alice().CredentialHash())
	require.True(t, ok)
	assert.Equal(t, "Alice", a.Name)
	_, ok = r.Current("alice", []byte("an older password hash"))
	assert.False(t, ok, "the password changed since the session began")
	_, ok = r.Current("bob", bob().CredentialHash())
	assert.False(t, ok, "the account left the file")
}
