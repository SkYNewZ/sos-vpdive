package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"modernc.org/sqlite"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// The online backup API sits on modernc's unexported connection type; these
// interfaces reach it through sql.Conn.Raw.
type (
	backuper interface {
		NewBackup(dstURI string) (*sqlite.Backup, error)
	}
	restorer interface {
		NewRestore(srcURI string) (*sqlite.Backup, error)
	}
)

// Backup writes a consistent copy of db to dest with SQLite's online backup
// API (spec §9.3). The copy stays encrypted and does not contain the key.
func Backup(ctx context.Context, db *sql.DB, dest string) (err error) {
	if _, statErr := os.Stat(dest); statErr == nil {
		return fmt.Errorf("backup: %s already exists", dest)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	return conn.Raw(func(dc any) error {
		b, ok := dc.(backuper)
		if !ok {
			return errors.New("backup: driver lacks the backup API")
		}
		bk, err := b.NewBackup(dest)
		if err != nil {
			return fmt.Errorf("backup: start: %w", err)
		}
		return copyPages(bk)
	})
}

// Restore replaces the database at dest with the backup at src, after checking
// that keys decrypt the backup's witness. The service must be stopped.
func Restore(ctx context.Context, src, dest string, keys *secure.Keys) (err error) {
	if err := checkBackup(ctx, src, keys); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dest+dsnParams)
	if err != nil {
		return fmt.Errorf("restore: open %s: %w", dest, err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	return conn.Raw(func(dc any) error {
		r, ok := dc.(restorer)
		if !ok {
			return errors.New("restore: driver lacks the backup API")
		}
		bk, err := r.NewRestore(src)
		if err != nil {
			return fmt.Errorf("restore: start: %w", err)
		}
		return copyPages(bk)
	})
}

func checkBackup(ctx context.Context, src string, keys *secure.Keys) (err error) {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro")
	if err != nil {
		return fmt.Errorf("restore: open %s: %w", src, err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var sealed []byte
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'key_check'`).Scan(&sealed); err != nil {
		return fmt.Errorf("restore: %s is not a backup of this application: %w", src, err)
	}
	return verifyWitness(keys, sealed)
}

func copyPages(bk *sqlite.Backup) error {
	for {
		more, err := bk.Step(-1)
		if err != nil {
			return errors.Join(fmt.Errorf("copy pages: %w", err), bk.Finish())
		}
		if !more {
			break
		}
	}
	if err := bk.Finish(); err != nil {
		return fmt.Errorf("finish copy: %w", err)
	}
	return nil
}
