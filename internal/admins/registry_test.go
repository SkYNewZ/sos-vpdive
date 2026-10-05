package admins

import (
	"bytes"
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
	r, err := Load(path, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
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

// Review focus 4: a file read while an editor rewrites it keeps the
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
