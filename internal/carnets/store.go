package carnets

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Store keeps the pushed cards (table carnets) under the rules of the
// payment lines: sealed JSON keyed by the holder's name hash, the « ambiguë »
// mark, purge after 90 days without a push, erasure by name (design
// 2026-10-09 §3).
type Store struct {
	payments.LineTable[Card]

	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{LineTable: payments.NewLineTable[Card](db, keys, now, imports.Carnets, "carnets", "carnets"),
		db: db, keys: keys, now: now}
}

// Resolve attaches each card of exp to its holder through the members list
// (design §2). A card no split of its holder's name designates, or two
// splits for two names, leaves exp and counts in ToCheck; a member added
// later gets their cards with the next push.
func (s *Store) Resolve(ctx context.Context, exp *Export) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT name_hash FROM members`)
	hashes, err := store.Collect(rows, err, func(rows *sql.Rows) (h []byte, err error) {
		err = rows.Scan(&h)
		return h, err
	})
	if err != nil {
		return fmt.Errorf("names of the members: %w", err)
	}
	known := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		known[string(h)] = true
	}
	exp.resolve(func(nameKey string) bool { return known[string(s.keys.Hash(nameKey))] })
	return nil
}

// Import replaces every card with a pushed list, without preview (design
// §2): imports.Push refuses an unchanged body, and a list under half of the
// cards in place, all of them, since a push covers 24 months.
func (s *Store) Import(ctx context.Context, exp *Export) error {
	e := imports.Entry{
		Kind: imports.Carnets, PeriodFrom: exp.From, PeriodTo: exp.To, ImportedAt: s.now(),
		ImportedBy: imports.ScriptAuthor, Rows: len(exp.Cards), Skipped: exp.Skipped, FileHash: exp.FileHash,
	}
	return imports.Push(ctx, s.db, "carnets.replace", e, s.ReplaceWith(exp.Cards))
}

// EraseTx deletes inside tx the cards of nameHash, a homonym's included, and
// blanks the author of the other cards' lines the member wrote (design §3).
// It returns how many cards went. Comments are free text: a name in one stays.
func (s *Store) EraseTx(ctx context.Context, tx *sql.Tx, nameHash []byte, last, first string) (int, error) {
	n, err := s.LineTable.EraseTx(ctx, tx, nameHash)
	if err != nil {
		return 0, err
	}
	return n, s.forgetAuthor(ctx, tx, last, first)
}

// forgetAuthor blanks By where it is the member's full name in either order,
// as the calendar does for unregistrations; never with an empty name part.
// The next push whose bytes differ brings the name back while VPDive keeps it.
func (s *Store) forgetAuthor(ctx context.Context, tx *sql.Tx, last, first string) error {
	isMember := secure.FullName(last, first)
	if isMember == nil {
		return nil
	}
	type row struct {
		id     int64
		sealed []byte
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, data FROM carnets`)
	all, err := store.Collect(rows, err, func(rows *sql.Rows) (r row, err error) {
		err = rows.Scan(&r.id, &r.sealed)
		return r, err
	})
	if err != nil {
		return fmt.Errorf("cards to check for an author: %w", err)
	}
	for _, r := range all {
		plain, err := s.keys.Open(r.sealed)
		if err != nil {
			return fmt.Errorf("decrypt card: %w", err)
		}
		var c Card
		if err := json.Unmarshal(plain, &c); err != nil {
			return fmt.Errorf("decode card: %w", err)
		}
		changed := false
		for i := range c.Entries {
			if isMember(c.Entries[i].By) {
				c.Entries[i].By, changed = "", true
			}
		}
		if !changed {
			continue
		}
		data, err := json.Marshal(c)
		if err != nil {
			return fmt.Errorf("encode card: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE carnets SET data = ? WHERE id = ?`, s.keys.Seal(data), r.id); err != nil {
			return fmt.Errorf("blank a card's author: %w", err)
		}
	}
	return nil
}
