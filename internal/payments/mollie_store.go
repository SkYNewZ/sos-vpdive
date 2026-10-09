package payments

import (
	"context"
	"database/sql"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
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

// Read counts every data row of the file, nameless ones included.
func (p *MolliePreview) Read() int { return p.Lines + p.Skipped }

// MollieStore imports VPayDive exports and reads the Mollie lines back
// (table online_payment_lines, spec §7.5).
type MollieStore struct {
	LineTable[MollieLine]

	previews *imports.Previews[*MolliePreview]
}

// NewMollieStore returns a MollieStore; now is injectable for tests.
func NewMollieStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *MollieStore {
	return &MollieStore{LineTable: mollieLines(db, keys, now), previews: imports.NewPreviews[*MolliePreview](now)}
}

// mollieLines is the table of the VPayDive export.
func mollieLines(db *sql.DB, keys *secure.Keys, now func() time.Time) LineTable[MollieLine] {
	return NewLineTable[MollieLine](db, keys, now, imports.Mollie, "mollie", "online_payment_lines")
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
	return imports.Replace(ctx, s.db, "mollie.replace", &p.Meta, e, s.ReplaceWith(p.lines))
}

// Import replaces every Mollie line with a pushed export, without preview
// (spec §7.6): imports.Push refuses an unchanged file and a file with less
// than half of the lines in place.
func (s *MollieStore) Import(ctx context.Context, exp *MollieExport) error {
	e := imports.Entry{
		Kind: imports.Mollie, ExportedAt: exp.Created, PeriodFrom: exp.PeriodFrom, PeriodTo: exp.PeriodTo,
		ImportedAt: s.now(), ImportedBy: imports.ScriptAuthor, Rows: len(exp.Lines), Skipped: exp.Skipped, FileHash: exp.FileHash,
	}
	return imports.Push(ctx, s.db, "mollie.replace", e, s.ReplaceWith(exp.Lines))
}
