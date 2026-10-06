package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

func openTemp(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db, path
}

func keys(t *testing.T, b byte) *secure.Keys {
	t.Helper()
	k, err := secure.NewKeys(bytes.Repeat([]byte{b}, 32))
	require.NoError(t, err)
	return k
}

func TestOpenCreatesSchemaAndIsIdempotent(t *testing.T) {
	db, path := openTemp(t)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var tables []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"attachments", "counters", "deflections", "events", "imports", "members", "messages",
		"meta", "outbox", "sessions", "stats_monthly", "tickets",
	}, tables)

	var mode string
	require.NoError(t, db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode))
	assert.Equal(t, "wal", mode)
	var tempStore int
	require.NoError(t, db.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&tempStore))
	assert.Equal(t, 2, tempStore, "temporary tables in memory")

	again, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, again.Close())
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	db, path := openTemp(t)
	_, err := db.ExecContext(context.Background(), `UPDATE meta SET value = CAST('99' AS BLOB) WHERE key = 'schema_version'`)
	require.NoError(t, err)

	_, err = Open(context.Background(), path)
	require.ErrorContains(t, err, "newer than this binary")
}

func TestCheckKey(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	require.NoError(t, CheckKey(ctx, db, keys(t, 1)), "creates the witness on a fresh database")
	require.NoError(t, CheckKey(ctx, db, keys(t, 1)), "same key accepted")
	require.ErrorIs(t, CheckKey(ctx, db, keys(t, 2)), ErrWrongKey)

	_, err := db.ExecContext(ctx, `UPDATE meta SET value = X'0100' WHERE key = 'key_check'`)
	require.NoError(t, err)
	require.ErrorIs(t, CheckKey(ctx, db, keys(t, 1)), ErrWrongKey, "altered witness")
}

func TestTxCommitsAndRollsBackWithSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	db, _ := openTemp(t)
	ctx := context.Background()
	insert := func(key string) func(context.Context, *sql.Tx) error {
		return func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO counters (key, window_start, count) VALUES (?, 0, 1)`, key)
			return err
		}
	}

	require.NoError(t, Tx(ctx, db, "test.commit", insert("kept")))
	boom := errors.New("boom")
	err := Tx(ctx, db, "test.rollback", func(ctx context.Context, tx *sql.Tx) error {
		require.NoError(t, insert("dropped")(ctx, tx))
		return boom
	})
	require.ErrorIs(t, err, boom)

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM counters`).Scan(&n))
	assert.Equal(t, 1, n)

	names := map[string]codes.Code{}
	for _, s := range rec.Ended() {
		names[s.Name()] = s.Status().Code
	}
	assert.Equal(t, codes.Unset, names["db test.commit"])
	assert.Equal(t, codes.Error, names["db test.rollback"])
}

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	require.NoError(t, CheckKey(ctx, db, keys(t, 1)))
	_, err := db.ExecContext(ctx, `INSERT INTO counters (key, window_start, count) VALUES ('k', 0, 42)`)
	require.NoError(t, err)

	backup := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, Backup(ctx, db, backup))
	require.ErrorContains(t, Backup(ctx, db, backup), "already exists")

	require.ErrorIs(t, Restore(ctx, backup, filepath.Join(t.TempDir(), FileName), keys(t, 2)), ErrWrongKey)

	dest := filepath.Join(t.TempDir(), FileName)
	require.NoError(t, Restore(ctx, backup, dest, keys(t, 1)))
	restored, err := Open(ctx, dest)
	require.NoError(t, err)
	defer func() { assert.NoError(t, restored.Close()) }()
	require.NoError(t, CheckKey(ctx, restored, keys(t, 1)))
	var n int
	require.NoError(t, restored.QueryRowContext(ctx, `SELECT count FROM counters WHERE key = 'k'`).Scan(&n))
	assert.Equal(t, 42, n)
}

func TestBackupIsARollbackJournalFile(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	require.NoError(t, CheckKey(ctx, db, keys(t, 1)))
	backup := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, Backup(ctx, db, backup))
	header, err := os.ReadFile(backup)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 1}, header[18:20], "file format write/read versions: 1 is rollback journal, 2 is WAL")
}

func TestRestoreFromReadOnlyBackup(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	require.NoError(t, CheckKey(ctx, db, keys(t, 1)))
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup.db")
	require.NoError(t, Backup(ctx, db, backup))
	require.NoError(t, os.Chmod(backup, 0o444))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o700)) })

	require.NoError(t, Restore(ctx, backup, filepath.Join(t.TempDir(), FileName), keys(t, 1)))
}

func TestRestoreFromMissingFile(t *testing.T) {
	err := Restore(context.Background(), filepath.Join(t.TempDir(), "nope.db"), filepath.Join(t.TempDir(), FileName), keys(t, 1))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRestoreRefusesForeignFile(t *testing.T) {
	foreign := filepath.Join(t.TempDir(), "other.db")
	other, err := sql.Open("sqlite", foreign)
	require.NoError(t, err)
	_, err = other.ExecContext(context.Background(), `CREATE TABLE x (y INTEGER)`)
	require.NoError(t, err)
	require.NoError(t, other.Close())

	err = Restore(context.Background(), foreign, filepath.Join(t.TempDir(), FileName), keys(t, 1))
	require.ErrorContains(t, err, "not a backup")
}
