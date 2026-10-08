package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Store keeps the pushed calendar: events and their participants, sealed,
// keyed by the hashes that match them with members and payments.
type Store struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{db: db, keys: keys, now: now}
}

// Import stores a pushed calendar in one transaction (spec §7.6 as amended):
// each event is stored or updated by id with its participants, and the
// stored events that start in the window and are missing from the push go.
// Older events stay: they are the history. imports.PushCounted refuses an
// unchanged body and a window holding under half of the events in place.
func (s *Store) Import(ctx context.Context, exp *Export) error {
	start, end := exp.window()
	e := imports.Entry{
		Kind: imports.Calendar, PeriodFrom: exp.From, PeriodTo: exp.To, ImportedAt: s.now(),
		ImportedBy: imports.ScriptAuthor, Rows: len(exp.Events), Skipped: exp.Skipped, FileHash: exp.FileHash,
	}
	current := func(ctx context.Context, q store.Querier) (int, error) {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events WHERE starts_at >= ? AND starts_at < ?`,
			start.Unix(), end.Unix()).Scan(&n); err != nil {
			return 0, fmt.Errorf("count calendar events in place: %w", err)
		}
		return n, nil
	}
	return imports.PushCounted(ctx, s.db, "calendar.import", e, exp.inWindow(), current, s.save(exp))
}

// Purge deletes the events that started more than 12 months ago and their
// participants (spec §8.3 as amended). The imports journal stays.
func (s *Store) Purge(ctx context.Context) error {
	return store.Tx(ctx, s.db, "calendar.purge", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_events WHERE starts_at < ?`, cutoff(s.now()).Unix()); err != nil {
			return fmt.Errorf("purge calendar events: %w", err)
		}
		return nil
	})
}

// save writes exp inside the import transaction.
func (s *Store) save(exp *Export) func(context.Context, *sql.Tx, int64) error {
	return func(ctx context.Context, tx *sql.Tx, importID int64) (err error) {
		insert, err := tx.PrepareContext(ctx,
			`INSERT INTO calendar_participants (event_id, person_hash, name_hash, data) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare participant insert: %w", err)
		}
		defer func() { err = errors.Join(err, insert.Close()) }()
		for _, ev := range exp.Events {
			if err := s.saveEvent(ctx, tx, insert, importID, ev); err != nil {
				return err
			}
		}
		start, end := exp.window()
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM calendar_events WHERE starts_at >= ? AND starts_at < ? AND import_id <> ?`,
			start.Unix(), end.Unix(), importID); err != nil {
			return fmt.Errorf("delete calendar events missing from the push: %w", err)
		}
		return nil
	}
}

// saveEvent stores or updates ev and replaces its participants.
func (s *Store) saveEvent(ctx context.Context, tx *sql.Tx, insert *sql.Stmt, importID int64, ev Event) error {
	participants, left := ev.Participants, ev.Unregistrations
	ev.Participants, ev.Unregistrations = nil, nil // in their own rows
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode calendar event %s: %w", ev.ID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO calendar_events (id, import_id, starts_at, ends_at, data) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET import_id = excluded.import_id, starts_at = excluded.starts_at,
		   ends_at = excluded.ends_at, data = excluded.data`,
		ev.ID, importID, ev.Start.Unix(), ev.End.Unix(), s.keys.Seal(data)); err != nil {
		return fmt.Errorf("store calendar event %s: %w", ev.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_participants WHERE event_id = ?`, ev.ID); err != nil {
		return fmt.Errorf("clear participants of event %s: %w", ev.ID, err)
	}
	for _, p := range participants {
		data, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("encode participant of event %s: %w", ev.ID, err)
		}
		if _, err := insert.ExecContext(ctx, ev.ID, s.personHash(p.VPDiveID), s.nameHash(p), s.keys.Seal(data)); err != nil {
			return fmt.Errorf("store participant of event %s: %w", ev.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_unregistrations WHERE event_id = ?`, ev.ID); err != nil {
		return fmt.Errorf("clear unregistrations of event %s: %w", ev.ID, err)
	}
	for _, u := range left {
		data, err := json.Marshal(u)
		if err != nil {
			return fmt.Errorf("encode unregistration of event %s: %w", ev.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO calendar_unregistrations (event_id, name_hash, data) VALUES (?, ?, ?)`,
			ev.ID, s.keys.Hash(secure.NameKey(u.LastName, u.FirstName)), s.keys.Seal(data)); err != nil {
			return fmt.Errorf("store unregistration of event %s: %w", ev.ID, err)
		}
	}
	return nil
}

// personHash keys a participant by VPDive id: the same person across
// events. The prefix keeps it apart from name keys and addresses.
func (s *Store) personHash(vpdiveID int64) []byte {
	return s.keys.Hash("vpdive:" + strconv.FormatInt(vpdiveID, 10))
}

// nameHash matches a participant with members and payments (spec §7.3):
// only a registered participant with both names has one, so matching never
// runs on an empty name or on the full name of an unregistered one. NULL
// otherwise: a nil []byte would bind as an empty blob.
func (s *Store) nameHash(p Participant) any {
	if !p.Registered || secure.NormalizeName(p.LastName) == "" || secure.NormalizeName(p.FirstName) == "" {
		return nil
	}
	return s.keys.Hash(secure.NameKey(p.LastName, p.FirstName))
}
