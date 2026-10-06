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

// MolliePreview compares a VPayDive export with the Mollie lines in place.
// It lives in memory only and can be confirmed by its uploader alone.
type MolliePreview struct {
	imports.Meta

	Created        time.Time // workbook creation date, indicative
	Lines          int
	Current        int // lines in place
	Skipped        int
	ToCheck        int // collected by Mollie, not settled in VPDive (spec §7.7)
	UnknownSettled map[string]int
	PeriodFrom     time.Time
	PeriodTo       time.Time

	lines []MollieLine
}

// MollieStore imports VPayDive exports and reads the Mollie lines back
// (table online_payment_lines, spec §7.5).
type MollieStore struct {
	db       *sql.DB
	keys     *secure.Keys
	now      func() time.Time
	previews *imports.Previews[*MolliePreview]
}

// NewMollieStore returns a MollieStore; now is injectable for tests.
func NewMollieStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *MollieStore {
	return &MollieStore{db: db, keys: keys, now: now, previews: imports.NewPreviews[*MolliePreview](now)}
}

// NewPreview compares exp with the lines in place and keeps the result for
// 15 minutes, bound to username and to the import in place.
func (s *MollieStore) NewPreview(ctx context.Context, username string, exp *MollieExport) (*MolliePreview, error) {
	meta, current, err := imports.NewMeta(ctx, s.db, imports.Mollie, username, len(exp.Lines), exp.FileHash)
	if err != nil {
		return nil, err
	}
	p := &MolliePreview{
		Meta:           meta,
		Created:        exp.Created,
		Lines:          len(exp.Lines),
		Current:        current,
		Skipped:        exp.Skipped,
		ToCheck:        exp.ToCheck(),
		UnknownSettled: exp.UnknownSettled,
		PeriodFrom:     exp.PeriodFrom,
		PeriodTo:       exp.PeriodTo,
		lines:          exp.Lines,
	}
	if err := s.previews.Put(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Preview returns a live preview of username.
func (s *MollieStore) Preview(id, username string) (*MolliePreview, error) {
	return s.previews.Get(id, username)
}

// Confirm replaces every Mollie line with the previewed export and journals
// the import, in one transaction, then marks the lines of homonyms.
func (s *MollieStore) Confirm(ctx context.Context, id, username string, secondConfirm bool) error {
	p, err := s.previews.Take(id, username, secondConfirm)
	if err != nil {
		return err
	}
	e := imports.Entry{
		Kind: imports.Mollie, ExportedAt: p.Created, PeriodFrom: p.PeriodFrom, PeriodTo: p.PeriodTo,
		ImportedAt: s.now(), ImportedBy: username, Rows: p.Lines, Skipped: p.Skipped, FileHash: p.FileHash,
	}
	return imports.Replace(ctx, s.db, "mollie.replace", &p.Meta, e, s.replaceWith(p.lines))
}

// Import replaces every Mollie line with a pushed export, without preview
// (spec §7.6): imports.Push refuses an unchanged file and a file with less
// than half of the lines in place.
func (s *MollieStore) Import(ctx context.Context, exp *MollieExport) error {
	e := imports.Entry{
		Kind: imports.Mollie, ExportedAt: exp.Created, PeriodFrom: exp.PeriodFrom, PeriodTo: exp.PeriodTo,
		ImportedAt: s.now(), ImportedBy: imports.ScriptAuthor, Rows: len(exp.Lines), Skipped: exp.Skipped, FileHash: exp.FileHash,
	}
	return imports.Push(ctx, s.db, "mollie.replace", e, s.replaceWith(exp.Lines))
}

// LastImport returns the latest VPayDive import, if any.
func (s *MollieStore) LastImport(ctx context.Context) (imports.Info, bool, error) {
	return imports.Last(ctx, s.db, imports.Mollie)
}

// Report counts the Mollie lines in place by attribution, as for payment
// lines (spec §7.5).
func (s *MollieStore) Report(ctx context.Context) (Report, error) {
	return report(ctx, s.db, `SELECT MAX(o.ambiguous), COUNT(*), (SELECT COUNT(*) FROM members m WHERE m.name_hash = o.name_hash)
		 FROM online_payment_lines o GROUP BY o.name_hash`)
}

// Purge deletes every Mollie line when no VPayDive import happened for 90
// days (spec §7.5, §8.3). The imports journal stays.
func (s *MollieStore) Purge(ctx context.Context) error {
	return store.Tx(ctx, s.db, "mollie.purge", func(ctx context.Context, tx *sql.Tx) error {
		// Never imported: MAX is NULL, the comparison is false, nothing goes.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM online_payment_lines WHERE (SELECT MAX(imported_at) FROM imports WHERE kind = ?) < ?`,
			string(imports.Mollie), s.now().Add(-retention).Unix()); err != nil {
			return fmt.Errorf("purge Mollie lines: %w", err)
		}
		return nil
	})
}

// HasLines reports whether Mollie lines are in place.
func (s *MollieStore) HasLines(ctx context.Context) (bool, error) {
	var found bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM online_payment_lines)`).Scan(&found); err != nil {
		return false, fmt.Errorf("presence of Mollie lines: %w", err)
	}
	return found, nil
}

// Count returns the number of Mollie lines of nameHash, ambiguous ones
// included.
func (s *MollieStore) Count(ctx context.Context, nameHash []byte) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM online_payment_lines WHERE name_hash = ?`, nameHash).Scan(&n); err != nil {
		return 0, fmt.Errorf("count Mollie lines of a name: %w", err)
	}
	return n, nil
}

// EraseTx deletes every Mollie line of nameHash inside tx, a homonym's
// included (erasure, spec §4.5; owner decision).
func (s *MollieStore) EraseTx(ctx context.Context, tx *sql.Tx, nameHash []byte) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM online_payment_lines WHERE name_hash = ?`, nameHash)
	if err != nil {
		return 0, fmt.Errorf("erase Mollie lines: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("erase Mollie lines: %w", err)
	}
	return int(n), nil
}

// replaceWith replaces the Mollie lines in place with lines.
func (s *MollieStore) replaceWith(lines []MollieLine) func(context.Context, *sql.Tx, int64) error {
	return func(ctx context.Context, tx *sql.Tx, importID int64) (err error) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM online_payment_lines`); err != nil {
			return fmt.Errorf("clear Mollie lines: %w", err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO online_payment_lines (import_id, name_hash, data) VALUES (?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare Mollie line insert: %w", err)
		}
		defer func() { err = errors.Join(err, stmt.Close()) }()
		for _, l := range lines {
			data, err := json.Marshal(l)
			if err != nil {
				return fmt.Errorf("encode Mollie line of row %d: %w", l.Row, err)
			}
			if _, err := stmt.ExecContext(ctx, importID, s.keys.Hash(l.NameKey), s.keys.Seal(data)); err != nil {
				return fmt.Errorf("insert Mollie line of row %d: %w", l.Row, err)
			}
		}
		return nil
	}
}
