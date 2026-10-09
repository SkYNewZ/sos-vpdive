package payments

import (
	"context"
	"database/sql"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Preview compares an export with the lines in place. It lives in memory
// only and can be confirmed by its uploader alone.
type Preview struct {
	imports.Meta

	Created        time.Time // workbook creation date, indicative
	Lines          int
	Current        int // lines in place
	Skipped        int
	ToCheck        int // partial payments (spec §7.7)
	UnknownStates  map[string]int
	MissingColumns []string
	PeriodFrom     time.Time
	PeriodTo       time.Time

	lines []Line
}

// Read counts every data row of the file, nameless ones included.
func (p *Preview) Read() int { return p.Lines + p.Skipped }

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

// Store imports payments exports and reads the lines back (table
// payment_lines).
type Store struct {
	LineTable[Line]

	previews *imports.Previews[*Preview]
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{LineTable: paymentLines(db, keys, now), previews: imports.NewPreviews[*Preview](now)}
}

// paymentLines is the table of the payments export.
func paymentLines(db *sql.DB, keys *secure.Keys, now func() time.Time) LineTable[Line] {
	return NewLineTable[Line](db, keys, now, imports.Payments, "payments", "payment_lines")
}

// ShortPeriod reports a period under 12 months (spec §7.3).
func ShortPeriod(from, to time.Time) bool {
	return from.AddDate(1, 0, 0).After(to)
}

// NewPreview compares exp with the lines in place and keeps the result for
// 15 minutes, bound to username and to the import in place.
func (s *Store) NewPreview(ctx context.Context, username string, exp *Export) (*Preview, error) {
	meta, current, err := imports.NewMeta(ctx, s.db, imports.Payments, username, len(exp.Lines), exp.FileHash)
	if err != nil {
		return nil, err
	}
	p := &Preview{
		Meta:           meta,
		Created:        exp.Created,
		Lines:          len(exp.Lines),
		Current:        current,
		Skipped:        exp.Skipped,
		ToCheck:        exp.ToCheck(),
		UnknownStates:  exp.UnknownStates,
		MissingColumns: exp.MissingColumns,
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
		ImportedAt: s.now(), ImportedBy: username, Rows: p.Lines, Skipped: p.Skipped, FileHash: p.FileHash,
	}
	return imports.Replace(ctx, s.db, "payments.replace", &p.Meta, e, s.ReplaceWith(p.lines))
}

// Import replaces every line with a pushed export, without preview (spec
// §7.6): imports.Push refuses an unchanged file and a file with less than
// half of the lines in place.
func (s *Store) Import(ctx context.Context, exp *Export) error {
	e := imports.Entry{
		Kind: imports.Payments, ExportedAt: exp.Created, PeriodFrom: exp.PeriodFrom, PeriodTo: exp.PeriodTo,
		ImportedAt: s.now(), ImportedBy: imports.ScriptAuthor, Rows: len(exp.Lines), Skipped: exp.Skipped, FileHash: exp.FileHash,
	}
	return imports.Push(ctx, s.db, "payments.replace", e, s.ReplaceWith(exp.Lines))
}
