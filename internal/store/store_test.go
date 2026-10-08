package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// dbAtVersion builds the database file of a binary at schema version n, runs
// seed on it and returns its path: Open then migrates it forward.
func dbAtVersion(t *testing.T, n int, seed string) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	old, err := sql.Open("sqlite", path+dsnParams)
	require.NoError(t, err)
	entries, err := migrations.ReadDir("migrations")
	require.NoError(t, err)
	require.NoError(t, Tx(ctx, old, "test.v"+strconv.Itoa(n), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE meta (key TEXT PRIMARY KEY, value BLOB NOT NULL)`); err != nil {
			return err
		}
		for _, e := range entries[:n] {
			script, err := migrations.ReadFile("migrations/" + e.Name())
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('schema_version', CAST('`+strconv.Itoa(n)+`' AS BLOB));`+seed)
		return err
	}))
	require.NoError(t, old.Close())
	return path
}

// foreignKeyViolations lists the tables of the rows that point at nothing.
func foreignKeyViolations(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	violations, err := Collect(rows, err, func(rows *sql.Rows) (string, error) {
		var table string
		var rowid, parent, fkid sql.NullString
		return table, rows.Scan(&table, &rowid, &parent, &fkid)
	})
	require.NoError(t, err)
	return violations
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
		"accounts", "attachments", "calendar_events", "calendar_participants", "calendar_unregistrations", "counters", "deflections", "dismissed_checks",
		"events", "imports", "members", "messages", "meta", "online_payment_lines", "outbox", "payment_lines",
		"push_subscriptions", "sessions", "stats_monthly", "tickets",
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

// TestMigration6KeepsImportsAndPaymentLines migrates a version-5 database
// holding imports and payment lines: imports is rebuilt under the lines that
// point at it.
func TestMigration6KeepsImportsAndPaymentLines(t *testing.T) {
	ctx := context.Background()
	path := dbAtVersion(t, 5, `
		INSERT INTO imports (id, kind, exported_at, imported_at, imported_by, row_count, skipped_count)
		VALUES (3, 'members', 100, 200, 'alice', 2, 0), (7, 'payments', NULL, 300, 'bob', 2, 1);
		INSERT INTO payment_lines (id, import_id, name_hash, ambiguous, data) VALUES (11, 7, x'01', 1, x'02'), (12, 7, x'03', 0, x'04');`)
	entries, err := migrations.ReadDir("migrations")
	require.NoError(t, err)

	db, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	var version []byte
	require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version))
	assert.Equal(t, strconv.Itoa(len(entries)), string(version), "every migration applied")
	type journal struct {
		id                  int64
		kind, by            string
		exported            sql.NullInt64
		imported, rows, skp int
		hash                []byte
	}
	rows, err := db.QueryContext(ctx, `SELECT id, kind, imported_by, exported_at, imported_at, row_count, skipped_count, file_hash FROM imports ORDER BY id`)
	got, err := Collect(rows, err, func(rows *sql.Rows) (j journal, err error) {
		err = rows.Scan(&j.id, &j.kind, &j.by, &j.exported, &j.imported, &j.rows, &j.skp, &j.hash)
		return j, err
	})
	require.NoError(t, err)
	assert.Equal(t, []journal{
		{id: 3, kind: "members", by: "alice", exported: sql.NullInt64{Int64: 100, Valid: true}, imported: 200, rows: 2},
		{id: 7, kind: "payments", by: "bob", imported: 300, rows: 2, skp: 1},
	}, got, "ids and values kept, no file hash before lot 7")
	type line struct {
		id, importID int64
		hash, data   []byte
		ambiguous    bool
	}
	lineRows, err := db.QueryContext(ctx, `SELECT id, import_id, name_hash, ambiguous, data FROM payment_lines ORDER BY id`)
	lines, err := Collect(lineRows, err, func(rows *sql.Rows) (l line, err error) {
		err = rows.Scan(&l.id, &l.importID, &l.hash, &l.ambiguous, &l.data)
		return l, err
	})
	require.NoError(t, err)
	assert.Equal(t, []line{
		{id: 11, importID: 7, hash: []byte{1}, data: []byte{2}, ambiguous: true},
		{id: 12, importID: 7, hash: []byte{3}, data: []byte{4}},
	}, lines, "lines kept with their ids, content and homonym mark")
	assert.Empty(t, foreignKeyViolations(t, db))

	_, err = db.ExecContext(ctx, `INSERT INTO imports (kind, imported_at, imported_by, row_count, skipped_count, file_hash)
		VALUES ('vpaydive', 400, 'script', 1, 0, x'05')`)
	require.NoError(t, err, "the new kind is accepted")
	_, err = db.ExecContext(ctx, `INSERT INTO imports (kind, imported_at, imported_by, row_count, skipped_count) VALUES ('other', 0, 'x', 0, 0)`)
	require.Error(t, err, "the kind is still checked")
	_, err = db.ExecContext(ctx, `INSERT INTO payment_lines (import_id, name_hash, data) VALUES (7, x'05', x'06')`)
	require.NoError(t, err, "payment lines point at the rebuilt imports")
	_, err = db.ExecContext(ctx, `INSERT INTO payment_lines (import_id, name_hash, data) VALUES (99, x'01', x'02')`)
	require.Error(t, err, "and the reference is still enforced")
	_, err = db.ExecContext(ctx, `INSERT INTO online_payment_lines (import_id, name_hash, data) VALUES (99, x'01', x'02')`)
	require.Error(t, err, "so do Mollie lines")
}

// TestMigration9ForgetsTheCalendarHash migrates a version-8 database: the
// last calendar push loses its hash, so that the same body imports again
// with its unregistrations; other kinds keep theirs.
func TestMigration9ForgetsTheCalendarHash(t *testing.T) {
	ctx := context.Background()
	path := dbAtVersion(t, 8, `
		INSERT INTO imports (id, kind, imported_at, imported_by, row_count, skipped_count, file_hash)
		VALUES (1, 'calendar', 100, 'script', 2, 0, x'01'), (2, 'payments', 200, 'script', 2, 0, x'02');`)
	db, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	rows, err := db.QueryContext(ctx, `SELECT kind, file_hash FROM imports ORDER BY id`)
	got, err := Collect(rows, err, func(rows *sql.Rows) (string, error) {
		var (
			kind string
			hash []byte
		)
		err := rows.Scan(&kind, &hash)
		return fmt.Sprintf("%s:%x", kind, hash), err
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"calendar:", "payments:02"}, got)
	_, err = db.ExecContext(ctx, `INSERT INTO calendar_unregistrations (event_id, name_hash, data) VALUES ('none', x'01', x'02')`)
	require.Error(t, err, "an unregistration points at its event")
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

// TestMigration8KeepsImportsAndLines migrates a version-7 database holding
// imports, payment and Mollie lines: imports is rebuilt under both line
// tables, and the calendar tables point at it.
func TestMigration8KeepsImportsAndLines(t *testing.T) {
	ctx := context.Background()
	path := dbAtVersion(t, 7, `
		INSERT INTO imports (id, kind, exported_at, imported_at, imported_by, row_count, skipped_count)
		VALUES (3, 'members', 100, 200, 'alice', 2, 0);
		INSERT INTO imports (id, kind, period_from, period_to, imported_at, imported_by, row_count, skipped_count, file_hash)
		VALUES (7, 'vpaydive', 10, 20, 300, 'script', 2, 1, x'aa');
		INSERT INTO payment_lines (id, import_id, name_hash, ambiguous, data) VALUES (11, 7, x'01', 1, x'02');
		INSERT INTO online_payment_lines (id, import_id, name_hash, ambiguous, data) VALUES (21, 7, x'03', 0, x'04');`)

	db, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	var (
		kind, by string
		from, to sql.NullInt64
		hash     []byte
		rowCount int
	)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT kind, imported_by, period_from, period_to, row_count, file_hash FROM imports WHERE id = 7`).
		Scan(&kind, &by, &from, &to, &rowCount, &hash))
	assert.Equal(t, "vpaydive", kind)
	assert.Equal(t, "script", by)
	assert.Equal(t, sql.NullInt64{Int64: 10, Valid: true}, from)
	assert.Equal(t, sql.NullInt64{Int64: 20, Valid: true}, to)
	assert.Equal(t, 2, rowCount)
	assert.Equal(t, []byte{0xaa}, hash, "the file hash survives the rebuild")
	var lines int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM payment_lines WHERE id = 11 AND import_id = 7 AND ambiguous = 1)
		      + (SELECT COUNT(*) FROM online_payment_lines WHERE id = 21 AND import_id = 7)`).Scan(&lines))
	assert.Equal(t, 2, lines, "lines kept with their ids, import and mark")
	assert.Empty(t, foreignKeyViolations(t, db))

	_, err = db.ExecContext(ctx, `INSERT INTO imports (id, kind, imported_at, imported_by, row_count, skipped_count)
		VALUES (8, 'calendar', 400, 'script', 1, 0)`)
	require.NoError(t, err, "the calendar kind is accepted")
	_, err = db.ExecContext(ctx, `INSERT INTO imports (kind, imported_at, imported_by, row_count, skipped_count) VALUES ('other', 0, 'x', 0, 0)`)
	require.Error(t, err, "the kind is still checked")
	_, err = db.ExecContext(ctx, `INSERT INTO online_payment_lines (import_id, name_hash, data) VALUES (99, x'01', x'02')`)
	require.Error(t, err, "Mollie lines still point at imports")
	_, err = db.ExecContext(ctx, `INSERT INTO calendar_events (id, import_id, starts_at, ends_at, data) VALUES ('evt-1', 99, 0, 0, x'01')`)
	require.Error(t, err, "an event points at its import")
	_, err = db.ExecContext(ctx, `INSERT INTO calendar_events (id, import_id, starts_at, ends_at, data) VALUES ('evt-1', 8, 0, 0, x'01');
		INSERT INTO calendar_participants (event_id, person_hash, name_hash, data) VALUES ('evt-1', x'05', NULL, x'06')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM calendar_events WHERE id = 'evt-1'`)
	require.NoError(t, err)
	var participants int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_participants`).Scan(&participants))
	assert.Zero(t, participants, "participants go with their event")
}
