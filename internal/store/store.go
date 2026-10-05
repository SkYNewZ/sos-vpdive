// Package store opens the SQLite database, applies the embedded migrations,
// runs traced transactions and guards the encryption key (spec §8).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// FileName is the database file inside DATA_DIR.
const FileName = "support.db"

const (
	tracerName = "github.com/SkYNewZ/sos-vpdive/internal/store"
	// Every connection gets WAL, foreign keys and a busy timeout; temporary
	// tables stay in memory because the image's file system is read-only
	// outside /data; transactions start with BEGIN IMMEDIATE so concurrent
	// writers queue instead of failing.
	dsnParams = "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)" +
		"&_pragma=temp_store(memory)&_txlock=immediate"
	keyCheckPlaintext = "sos-vpdive key check v1"
)

// ErrWrongKey reports a SECRET_KEY that does not decrypt the existing data.
var ErrWrongKey = errors.New("SECRET_KEY does not match the data in " + FileName)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens or creates the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// Tx runs fn in a transaction traced as one span named "db <name>", where
// name is an operation name, never data. It commits when fn returns nil and
// rolls back otherwise; fn's error stays matchable with errors.Is.
func Tx(ctx context.Context, db *sql.DB, name string, fn func(context.Context, *sql.Tx) error) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "db "+name)
	defer span.End()
	defer func() {
		if err != nil {
			telemetry.Fail(span, "db_error")
		}
	}()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", name, err)
	}
	if err := fn(ctx, tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("%s: rollback: %w", name, rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", name, err)
	}
	return nil
}

// CheckKey verifies that keys decrypt the witness stored in meta, and creates
// the witness on a fresh database (spec §8.4).
func CheckKey(ctx context.Context, db *sql.DB, keys *secure.Keys) error {
	return Tx(ctx, db, "check_key", func(ctx context.Context, tx *sql.Tx) error {
		var sealed []byte
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'key_check'`).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('key_check', ?)`,
				keys.SealString(keyCheckPlaintext)); err != nil {
				return fmt.Errorf("create key witness: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read key witness: %w", err)
		}
		return verifyWitness(keys, sealed)
	})
}

func verifyWitness(keys *secure.Keys, sealed []byte) error {
	got, err := keys.OpenString(sealed)
	if err != nil || got != keyCheckPlaintext {
		return ErrWrongKey
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	return Tx(ctx, db, "migrate", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value BLOB NOT NULL)`); err != nil {
			return fmt.Errorf("create meta: %w", err)
		}
		current, err := schemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		entries, err := fs.ReadDir(migrations, "migrations")
		if err != nil {
			return fmt.Errorf("list migrations: %w", err)
		}
		if current > len(entries) {
			return fmt.Errorf("database schema version %d is newer than this binary (%d)", current, len(entries))
		}
		for i, e := range entries[current:] {
			version := current + i + 1
			if !strings.HasPrefix(e.Name(), fmt.Sprintf("%04d_", version)) {
				return fmt.Errorf("migration %s: expected prefix %04d_", e.Name(), version)
			}
			script, err := migrations.ReadFile("migrations/" + e.Name())
			if err != nil {
				return fmt.Errorf("read %s: %w", e.Name(), err)
			}
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return fmt.Errorf("apply %s: %w", e.Name(), err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO meta (key, value) VALUES ('schema_version', ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
				[]byte(strconv.Itoa(version))); err != nil {
				return fmt.Errorf("record schema version %d: %w", version, err)
			}
		}
		return nil
	})
}

func schemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	v, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, fmt.Errorf("schema version %q: %w", raw, err)
	}
	return v, nil
}
