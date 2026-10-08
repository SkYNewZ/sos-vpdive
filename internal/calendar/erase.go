package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Count returns how many participations and unregistrations EraseTx would
// delete for the member named last, first, whose name hash is nameHash
// (spec §4.5).
func (s *Store) Count(ctx context.Context, nameHash []byte, last, first string) (int, error) {
	persons, err := s.persons(ctx, s.db, nameHash, last, first)
	if err != nil {
		return 0, err
	}
	total := 0
	for h := range persons {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_participants WHERE person_hash = ?`,
			[]byte(h)).Scan(&n); err != nil {
			return 0, fmt.Errorf("count participations of a person: %w", err)
		}
		total += n
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_unregistrations WHERE name_hash = ?`,
		nameHash).Scan(&left); err != nil {
		return 0, fmt.Errorf("count unregistrations of a person: %w", err)
	}
	return total + left, nil
}

// EraseTx deletes inside tx every participation of the people an erasure of
// the member reaches, and the unregistrations of nameHash (spec §4.5 as
// amended), a homonym's included, and returns how many went. The next push
// brings them back while VPDive has them.
func (s *Store) EraseTx(ctx context.Context, tx *sql.Tx, nameHash []byte, last, first string) (int, error) {
	persons, err := s.persons(ctx, tx, nameHash, last, first)
	if err != nil {
		return 0, err
	}
	total := 0
	for h := range persons {
		res, err := tx.ExecContext(ctx, `DELETE FROM calendar_participants WHERE person_hash = ?`, []byte(h))
		if err != nil {
			return 0, fmt.Errorf("erase participations of a person: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("erase participations of a person: %w", err)
		}
		total += int(n)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM calendar_unregistrations WHERE name_hash = ?`, nameHash)
	if err != nil {
		return 0, fmt.Errorf("erase unregistrations of a person: %w", err)
	}
	left, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("erase unregistrations of a person: %w", err)
	}
	if err := s.forgetAuthor(ctx, tx, last, first); err != nil {
		return 0, err
	}
	return total + int(left), nil
}

// forgetAuthor blanks the author of the unregistrations the member made,
// found by their full name in either order as persons finds unregistered
// participants; never with an empty name part. The next push whose bytes
// differ brings the name back while VPDive keeps it.
func (s *Store) forgetAuthor(ctx context.Context, tx *sql.Tx, last, first string) error {
	isMember := fullName(last, first)
	if isMember == nil {
		return nil
	}
	type row struct {
		id     int64
		sealed []byte
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, data FROM calendar_unregistrations`)
	all, err := store.Collect(rows, err, func(rows *sql.Rows) (r row, err error) {
		err = rows.Scan(&r.id, &r.sealed)
		return r, err
	})
	if err != nil {
		return fmt.Errorf("unregistrations to check for an author: %w", err)
	}
	for _, r := range all {
		u, err := s.openUnregistration(r.sealed)
		if err != nil {
			return err
		}
		if !isMember(u.By) {
			continue
		}
		u.By = ""
		data, err := json.Marshal(u)
		if err != nil {
			return fmt.Errorf("encode unregistration: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE calendar_unregistrations SET data = ? WHERE id = ?`,
			s.keys.Seal(data), r.id); err != nil {
			return fmt.Errorf("blank an unregistration's author: %w", err)
		}
	}
	return nil
}

// persons returns the person hashes an erasure reaches: those with a
// participation of nameHash, and those with a participation without a name
// hash (unregistered, or a name part missing) whose full name is the member's
// in either order. The full name serves
// erasure only, never matching, and never with an empty name part.
func (s *Store) persons(ctx context.Context, q store.Querier, nameHash []byte, last, first string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT person_hash FROM calendar_participants WHERE name_hash = ?`, nameHash)
	named, err := store.Collect(rows, err, func(rows *sql.Rows) (h []byte, err error) {
		err = rows.Scan(&h)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("participations of a name: %w", err)
	}
	out := make(map[string]bool, len(named))
	for _, h := range named {
		out[string(h)] = true
	}
	isMember := fullName(last, first)
	if isMember == nil {
		return out, nil
	}
	rows, err = q.QueryContext(ctx, `SELECT person_hash, data FROM calendar_participants WHERE name_hash IS NULL`)
	unnamed, err := store.Collect(rows, err, func(rows *sql.Rows) (r [2][]byte, err error) {
		err = rows.Scan(&r[0], &r[1])
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("unregistered participations: %w", err)
	}
	for _, r := range unnamed {
		p, err := s.openParticipant(r[1])
		if err != nil {
			return nil, err
		}
		if isMember(p.Name) {
			out[string(r[0])] = true
		}
	}
	return out, nil
}

// fullName tells whether a name is the member's full name in either order;
// NormalizeName keeps letters only: "MARTIN Léa" is Léa Martin's. Nil when a
// name part is empty: never on half a name. For erasure only, never matching.
func fullName(last, first string) func(name string) bool {
	l, f := secure.NormalizeName(last), secure.NormalizeName(first)
	if l == "" || f == "" {
		return nil
	}
	return func(name string) bool {
		n := secure.NormalizeName(name)
		return n == l+f || n == f+l
	}
}
