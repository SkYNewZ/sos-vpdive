package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Preview compares a campaign export with the people in place. It lives in
// memory only and can be confirmed by its uploader alone.
type Preview struct {
	imports.Meta

	Exported time.Time // the date on row 2, indicative
	People   int
	Current  int // people in place
	Skipped  int

	people []Person
}

// Store keeps the people of the latest campaign export (table
// campaign_lines) under the rules of the payment lines: sealed JSON keyed by
// the name hash, the « ambiguë » mark, purge after 90 days without an
// import, erasure by name (design 2026-10-20 « Stockage »).
type Store struct {
	payments.LineTable[Person]

	db       *sql.DB
	now      func() time.Time
	previews *imports.Previews[*Preview]
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{
		LineTable: payments.NewLineTable[Person](db, keys, now, imports.Campaign, "campaign", "campaign_lines"),
		db:        db, now: now, previews: imports.NewPreviews[*Preview](now),
	}
}

// NewPreview compares exp with the people in place and keeps the result for
// 15 minutes, bound to username and to the import in place.
func (s *Store) NewPreview(ctx context.Context, username string, exp *Export) (*Preview, error) {
	meta, current, err := imports.NewMeta(ctx, s.db, imports.Campaign, username, len(exp.People), exp.FileHash)
	if err != nil {
		return nil, err
	}
	p := &Preview{Meta: meta, Exported: exp.Exported, People: len(exp.People), Current: current, Skipped: exp.Skipped, people: exp.People}
	if err := s.previews.Put(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Preview returns a live preview of username.
func (s *Store) Preview(id, username string) (*Preview, error) {
	return s.previews.Get(id, username)
}

// Confirm replaces every person with the previewed export and journals the
// import, in one transaction; with secondConfirm it is the only way past a
// file under half of the people in place.
func (s *Store) Confirm(ctx context.Context, id, username string, secondConfirm bool) error {
	p, err := s.previews.Take(id, username, secondConfirm)
	if err != nil {
		return err
	}
	e := imports.Entry{
		Kind: imports.Campaign, ExportedAt: p.Exported, ImportedAt: s.now(), ImportedBy: username,
		Rows: p.People, Skipped: p.Skipped, FileHash: p.FileHash,
	}
	return imports.Replace(ctx, s.db, "campaign.replace", &p.Meta, e, s.replaceWith(p.people))
}

// Import replaces every person with a pushed export, without preview:
// imports.Push refuses an unchanged file and a file under half of the people
// in place.
func (s *Store) Import(ctx context.Context, exp *Export) error {
	e := imports.Entry{
		Kind: imports.Campaign, ExportedAt: exp.Exported, ImportedAt: s.now(), ImportedBy: imports.ScriptAuthor,
		Rows: len(exp.People), Skipped: exp.Skipped, FileHash: exp.FileHash,
	}
	return imports.Push(ctx, s.db, "campaign.replace", e, s.replaceWith(exp.People))
}

// EraseTx deletes inside tx the people of nameHash, a homonym's included,
// and blanks the member as the validator of the others (design
// « Effacement »). It returns how many lines went. The next import brings
// the name back while VPDive keeps it.
func (s *Store) EraseTx(ctx context.Context, tx *sql.Tx, nameHash []byte, last, first string) (int, error) {
	n, err := s.LineTable.EraseTx(ctx, tx, nameHash)
	if err != nil {
		return 0, err
	}
	isMember := secure.FullName(last, first)
	if isMember == nil { // never on half a name
		return n, nil
	}
	return n, s.RewriteTx(ctx, tx, func(p *Person) bool {
		if !isMember(p.ValidatedBy) {
			return false
		}
		p.ValidatedBy = ""
		return true
	})
}

// replaceWith replaces the people, then marks the names the export itself
// holds twice, whatever the members list says; imports.MarkAmbiguous marks
// those several members share (design « Stockage », Codex review).
func (s *Store) replaceWith(people []Person) func(context.Context, *sql.Tx, int64) error {
	replace := s.ReplaceWith(people)
	return func(ctx context.Context, tx *sql.Tx, importID int64) error {
		if err := replace(ctx, tx, importID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE campaign_lines SET ambiguous = 1
			WHERE name_hash IN (SELECT name_hash FROM campaign_lines GROUP BY name_hash HAVING COUNT(*) > 1)`); err != nil {
			return fmt.Errorf("mark names twice in the campaign: %w", err)
		}
		return nil
	}
}
