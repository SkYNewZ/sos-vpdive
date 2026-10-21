package web

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

var testDatabase struct {
	once sync.Once
	data []byte
}

// Migrate once, then give each test its own file and connections.
func newTestDB(ctx context.Context, t *testing.T, path string) *sql.DB {
	t.Helper()
	testDatabase.once.Do(func() {
		dir := t.TempDir()
		source, err := store.Open(ctx, filepath.Join(dir, "source.db"))
		require.NoError(t, err)
		defer func() { assert.NoError(t, source.Close()) }()
		snapshot := filepath.Join(dir, store.FileName)
		require.NoError(t, store.Backup(ctx, source, snapshot))
		testDatabase.data, err = os.ReadFile(snapshot)
		require.NoError(t, err)
	})
	require.NoError(t, os.WriteFile(path, testDatabase.data, 0o600))
	db, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

func TestTestDatabasesAreIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first := newTestDB(ctx, t, filepath.Join(t.TempDir(), store.FileName))
	_, err := first.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('test_marker', 'first database only')`)
	require.NoError(t, err)

	for range 4 {
		t.Run("copy", func(t *testing.T) {
			t.Parallel()
			db := newTestDB(ctx, t, filepath.Join(t.TempDir(), store.FileName))
			var count int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key = 'test_marker'`).Scan(&count))
			assert.Zero(t, count, "another test's rows must not reach a fresh database")

			var mode string
			require.NoError(t, db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode))
			assert.Equal(t, "wal", mode)
			var foreignKeys int
			require.NoError(t, db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys))
			assert.Equal(t, 1, foreignKeys)
			_, err := db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('test_marker', 'this copy only')`)
			require.NoError(t, err)
		})
	}
}
