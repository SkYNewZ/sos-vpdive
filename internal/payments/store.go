package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// retention is how long lines outlive their import when no new one comes
// (spec §8.3).
const retention = 90 * 24 * time.Hour

// Preview compares an export with the lines in place. It lives in memory
// only and can be confirmed by its uploader alone.
type Preview struct {
	imports.Meta

	Created       time.Time // workbook creation date, indicative
	Lines         int
	Current       int // lines in place
	Skipped       int
	UnknownStates map[string]int
	PeriodFrom    time.Time
	PeriodTo      time.Time
	ShortPeriod   bool // under 12 months: the VPDive filter was not set right

	lines []Line
}

// Count is a number of lines and of distinct payers.
type Count struct {
	Lines  int
	Payers int
}

// Report splits the lines in place as the ticket pages attribute them (spec
// §7.3): to exactly one member, to a name several members share or that is
// marked, or to no member.
type Report struct {
	Attached  Count
	Ambiguous Count
	Unmatched Count
}

// Store imports payments exports and reads the lines back.
type Store struct {
	db       *sql.DB
	keys     *secure.Keys
	now      func() time.Time
	previews *imports.Previews[*Preview]
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{db: db, keys: keys, now: now, previews: imports.NewPreviews[*Preview](now)}
}

// ShortPeriod reports a period under 12 months (spec §7.3).
func ShortPeriod(from, to time.Time) bool {
	return from.AddDate(1, 0, 0).After(to)
}

// NewPreview compares exp with the lines in place and keeps the result for
// 15 minutes, bound to username and to the import in place.
func (s *Store) NewPreview(ctx context.Context, username string, exp *Export) (*Preview, error) {
	base, err := imports.LatestID(ctx, s.db, imports.Payments)
	if err != nil {
		return nil, err
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payment_lines`).Scan(&current); err != nil {
		return nil, fmt.Errorf("count payment lines: %w", err)
	}
	p := &Preview{
		Username:           username,
		NeedsSecondConfirm: len(exp.Lines)*2 < current,
		Base:               base,
		Created:            exp.Created,
		Lines:              len(exp.Lines),
		Current:            current,
		Skipped:            exp.Skipped,
		UnknownStates:      exp.UnknownStates,
		PeriodFrom:         exp.PeriodFrom,
		PeriodTo:           exp.PeriodTo,
		ShortPeriod:        ShortPeriod(exp.PeriodFrom, exp.PeriodTo),
		lines:              exp.Lines,
	}
	if _, err := s.previews.Put(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Preview returns a live preview of username.
func (s *Store) Preview(id, username string) (*Preview, error) {
	return s.previews.Get(id, username)
}

// Confirm replaces every line with the previewed export and journals the
// import, in one transaction, then marks the lines of homonyms. The preview
// is consumed first, so a second confirmation gets imports.ErrPreviewNotFound.
func (s *Store) Confirm(ctx context.Context, id, username string, secondConfirm bool) error {
	p, err := s.previews.Take(id, username, secondConfirm)
	if err != nil {
		return err
	}
	e := imports.Entry{
		Kind: imports.Payments, ExportedAt: p.Created, PeriodFrom: p.PeriodFrom, PeriodTo: p.PeriodTo,
		ImportedAt: s.now(), ImportedBy: username, Rows: p.Lines, Skipped: p.Skipped,
	}
	return imports.Replace(ctx, s.db, "payments.replace", &p.Meta, e, func(ctx context.Context, tx *sql.Tx, importID int64) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM payment_lines`); err != nil {
			return fmt.Errorf("clear payment lines: %w", err)
		}
		return s.insertLines(ctx, tx, importID, p.lines)
	})
}

// LastImport returns the latest payments import, if any.
func (s *Store) LastImport(ctx context.Context) (imports.Info, bool, error) {
	return imports.Last(ctx, s.db, imports.Payments)
}

// Report counts the lines in place by attribution.
func (s *Store) Report(ctx context.Context) (Report, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT MAX(p.ambiguous), COUNT(*), (SELECT COUNT(*) FROM members m WHERE m.name_hash = p.name_hash)
		 FROM payment_lines p GROUP BY p.name_hash`)
	payers, err := store.Collect(rows, err, func(rows *sql.Rows) (c struct{ marked, lines, members int }, err error) {
		err = rows.Scan(&c.marked, &c.lines, &c.members)
		return c, err
	})
	if err != nil {
		return Report{}, fmt.Errorf("payments report: %w", err)
	}
	var r Report
	for _, p := range payers {
		dst := &r.Unmatched
		switch {
		case p.marked == 1 || p.members > 1:
			dst = &r.Ambiguous
		case p.members == 1:
			dst = &r.Attached
		}
		dst.Lines += p.lines
		dst.Payers++
	}
	return r, nil
}

// Purge deletes every line when no payments import happened for 90 days
// (spec §8.3). The imports journal holds no personal data and stays.
func (s *Store) Purge(ctx context.Context) error {
	cutoff := s.now().Add(-retention).Unix()
	return store.Tx(ctx, s.db, "payments.purge", func(ctx context.Context, tx *sql.Tx) error {
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(imported_at) FROM imports WHERE kind = ?`,
			string(imports.Payments)).Scan(&last); err != nil {
			return fmt.Errorf("last payments import date: %w", err)
		}
		if !last.Valid || last.Int64 >= cutoff {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM payment_lines`); err != nil {
			return fmt.Errorf("purge payment lines: %w", err)
		}
		return nil
	})
}

// Count returns the number of lines of nameHash, ambiguous ones included.
func (s *Store) Count(ctx context.Context, nameHash []byte) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payment_lines WHERE name_hash = ?`, nameHash).Scan(&n); err != nil {
		return 0, fmt.Errorf("count payment lines of a name: %w", err)
	}
	return n, nil
}

// EraseTx deletes every line of nameHash inside tx, a homonym's included
// (erasure, spec §4.5; owner decision). The next import brings them back
// while VPDive has them.
func (s *Store) EraseTx(ctx context.Context, tx *sql.Tx, nameHash []byte) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM payment_lines WHERE name_hash = ?`, nameHash)
	if err != nil {
		return 0, fmt.Errorf("erase payment lines: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("erase payment lines: %w", err)
	}
	return int(n), nil
}

func (s *Store) insertLines(ctx context.Context, tx *sql.Tx, importID int64, lines []Line) (err error) {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO payment_lines (import_id, name_hash, data) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare payment line insert: %w", err)
	}
	defer func() { err = errors.Join(err, stmt.Close()) }()
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			return fmt.Errorf("encode payment line of row %d: %w", l.Row, err)
		}
		if _, err := stmt.ExecContext(ctx, importID, s.keys.Hash(l.NameKey), s.keys.Seal(data)); err != nil {
			return fmt.Errorf("insert payment line of row %d: %w", l.Row, err)
		}
	}
	return nil
}
