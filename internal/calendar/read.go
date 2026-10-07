package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// ErrNotFound reports an event the calendar does not hold: deleted in
// VPDive, or started more than 12 months ago.
var ErrNotFound = errors.New("calendar event not found")

// Until is when the event ends on screen: an end before the start reads as
// the start (owner decision, lot 8 part 2).
func (e Event) Until() time.Time {
	if e.EndInverted() {
		return e.Start
	}
	return e.End
}

// EndInverted reports an end before the start, as VPDive sent it.
func (e Event) EndInverted() bool { return e.End.Before(e.Start) }

// Cancelled reports an event whose title says it is cancelled (spec §7.4).
func (e Event) Cancelled() bool { return strings.Contains(strings.ToLower(e.Title), "annul") }

// Participation is a person's place on an event: the event, and that
// person's participant row.
type Participation struct {
	Event       Event
	Participant Participant
}

// overlaps selects the events that overlap [?1, ?2), in Unix seconds: an
// event that ends at ?1 or starts at ?2 is outside.
const overlaps = `starts_at < ?2 AND (starts_at >= ?1 OR ends_at > ?1)`

// readTx opens the transaction a read of events then participants runs in.
// A read-only transaction is a plain deferred BEGIN (the DSN's
// _txlock=immediate applies to writes only), so the reads of Range or Event
// share one WAL snapshot, even when a push commits in between, and no writer
// waits on them. The caller ends it with endRead.
func (s *Store) readTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin calendar read: %w", err)
	}
	return tx, nil
}

// endRead rolls back a read transaction; one the context already ended is
// not an error, and err keeps its identity when the rollback succeeds.
func endRead(tx *sql.Tx, err *error) {
	if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
		*err = errors.Join(*err, rerr)
	}
}

// Range returns the events overlapping [from, to), by start then id, each
// once. With participants, each event carries its own in pushed order.
func (s *Store) Range(ctx context.Context, from, to time.Time, participants bool) (_ []Event, err error) {
	tx, err := s.readTx(ctx)
	if err != nil {
		return nil, err
	}
	defer endRead(tx, &err)
	rows, err := tx.QueryContext(ctx,
		`SELECT starts_at, ends_at, data FROM calendar_events WHERE `+overlaps+` ORDER BY starts_at, id`,
		from.Unix(), to.Unix())
	events, err := store.Collect(rows, err, func(rows *sql.Rows) (Event, error) {
		var (
			start, end int64
			sealed     []byte
		)
		if err := rows.Scan(&start, &end, &sealed); err != nil {
			return Event{}, err
		}
		return s.openEvent(start, end, sealed)
	})
	if err != nil {
		return nil, fmt.Errorf("read calendar events: %w", err)
	}
	if !participants || len(events) == 0 {
		return events, nil
	}
	index := make(map[string]int, len(events))
	for i, ev := range events {
		index[ev.ID] = i
	}
	type row struct {
		event string
		p     Participant
	}
	rows, err = tx.QueryContext(ctx,
		`SELECT event_id, data FROM calendar_participants
		 WHERE event_id IN (SELECT id FROM calendar_events WHERE `+overlaps+`) ORDER BY id`,
		from.Unix(), to.Unix())
	found, err := store.Collect(rows, err, func(rows *sql.Rows) (r row, err error) {
		var sealed []byte
		if err = rows.Scan(&r.event, &sealed); err != nil {
			return r, err
		}
		r.p, err = s.openParticipant(sealed)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read calendar participants: %w", err)
	}
	for _, r := range found {
		if i, ok := index[r.event]; ok { // always, inside the transaction
			events[i].Participants = append(events[i].Participants, r.p)
		}
	}
	return events, nil
}

// Event returns the event id with its participants. Each one carries how
// many members its match names (spec §7.3 as amended): its own name hash,
// else the one its other registrations share. It reads the members table,
// as payments do; never a name.
func (s *Store) Event(ctx context.Context, id string) (_ Event, err error) {
	tx, err := s.readTx(ctx)
	if err != nil {
		return Event{}, err
	}
	defer endRead(tx, &err)
	var (
		start, end int64
		sealed     []byte
	)
	err = tx.QueryRowContext(ctx, `SELECT starts_at, ends_at, data FROM calendar_events WHERE id = ?`, id).
		Scan(&start, &end, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("read calendar event: %w", err)
	}
	ev, err := s.openEvent(start, end, sealed)
	if err != nil {
		return Event{}, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT data, (SELECT COUNT(*) FROM members m WHERE m.name_hash = matched) FROM (
		   SELECT p.id, p.data, COALESCE(p.name_hash, (
		     SELECT CASE WHEN COUNT(DISTINCT o.name_hash) = 1 THEN MIN(o.name_hash) END
		     FROM calendar_participants o WHERE o.person_hash = p.person_hash AND o.name_hash IS NOT NULL)) AS matched
		   FROM calendar_participants p WHERE p.event_id = ?)
		 ORDER BY id`, id)
	ev.Participants, err = store.Collect(rows, err, func(rows *sql.Rows) (Participant, error) {
		var (
			sealed  []byte
			members int
		)
		if err := rows.Scan(&sealed, &members); err != nil {
			return Participant{}, err
		}
		p, err := s.openParticipant(sealed)
		p.Members = members
		return p, err
	})
	if err != nil {
		return Event{}, fmt.Errorf("read participants of a calendar event: %w", err)
	}
	return ev, nil
}

// Participations returns the participations of the person of nameHash,
// newest event first, by the rule of Event: the rows of the name hash itself,
// and the unregistered ones (a pilot, a payer) of the VPDive accounts whose
// only registered name is that hash. An account registered under two names is
// followed by neither, so no other person's row ever lands on this one. Never
// by name: a nil hash finds nothing.
func (s *Store) Participations(ctx context.Context, nameHash []byte) ([]Participation, error) {
	if nameHash == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.starts_at, e.ends_at, e.data, p.data
		 FROM calendar_participants p JOIN calendar_events e ON e.id = p.event_id
		 WHERE p.name_hash = ?1 OR (p.name_hash IS NULL AND p.person_hash IN (
		   SELECT person_hash FROM calendar_participants WHERE name_hash IS NOT NULL
		   GROUP BY person_hash HAVING COUNT(DISTINCT name_hash) = 1 AND MIN(name_hash) = ?1))
		 ORDER BY e.starts_at DESC, e.id, p.id`, nameHash)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (pa Participation, err error) {
		var (
			start, end    int64
			event, person []byte
		)
		if err = rows.Scan(&start, &end, &event, &person); err != nil {
			return pa, err
		}
		if pa.Event, err = s.openEvent(start, end, event); err != nil {
			return pa, err
		}
		pa.Participant, err = s.openParticipant(person)
		return pa, err
	})
	if err != nil {
		return nil, fmt.Errorf("read participations of a person: %w", err)
	}
	return out, nil
}

// openEvent decrypts an event and sets its times from the clear columns.
func (s *Store) openEvent(start, end int64, sealed []byte) (Event, error) {
	plain, err := s.keys.Open(sealed)
	if err != nil {
		return Event{}, fmt.Errorf("decrypt calendar event: %w", err)
	}
	var ev Event
	if err := json.Unmarshal(plain, &ev); err != nil {
		return Event{}, fmt.Errorf("decode calendar event: %w", err)
	}
	ev.Start, ev.End = time.Unix(start, 0), time.Unix(end, 0)
	return ev, nil
}

// openParticipant decrypts a participant.
func (s *Store) openParticipant(sealed []byte) (Participant, error) {
	plain, err := s.keys.Open(sealed)
	if err != nil {
		return Participant{}, fmt.Errorf("decrypt participant: %w", err)
	}
	var p Participant
	if err := json.Unmarshal(plain, &p); err != nil {
		return Participant{}, fmt.Errorf("decode participant: %w", err)
	}
	return p, nil
}
