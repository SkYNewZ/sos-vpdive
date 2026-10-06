package tickets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Screen 2 (spec §3.2): a draft the model matched with fiches waits for the
// member's choice behind a draft token, distinct from the tracking link. Only
// its hash is stored; it dies with the draft, after 24 hours.

// ErrDraftGone reports an unknown draft token, or a draft past its 24 hours.
var ErrDraftGone = errors.New("draft unknown or expired")

// Draft is a request waiting on screen 2.
type Draft struct {
	ID          int64
	Category    string
	KBIDs       []string // fiches to show, in the model's order
	Email       string   // normalized, for the link resend
	OpenRequest bool     // the address has another request in progress (spec §3.3)
	Ref         string   // set once confirmed: show its confirmation instead
}

// suggested stores the model's answer. Fiches on a draft lead to screen 2;
// otherwise, or when the draft was confirmed meanwhile, the request is
// confirmed. The answer is kept either way for the committee (spec §5.3).
func (s *Store) suggested(ctx context.Context, id int64, sg Suggestion) (Outcome, error) {
	ids := sg.KBIDs
	if ids == nil {
		ids = []string{}
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return Outcome{}, fmt.Errorf("encode fiches: %w", err)
	}
	var token string
	err = s.tx(ctx, "suggested", func(ctx context.Context, tx *sql.Tx) error {
		var summary []byte
		if sg.Summary != "" {
			summary = s.Keys.SealString(sg.Summary)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tickets SET kb_ids = ?, summary = ? WHERE id = ? AND kb_ids IS NULL`,
			string(raw), summary, id); err != nil {
			return fmt.Errorf("store suggestion: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		token, err = s.newDraftToken(ctx, tx, id)
		return err
	})
	if err != nil || token != "" {
		return Outcome{Token: token}, err
	}
	ref, err := s.Confirm(ctx, id)
	return Outcome{Ref: ref}, err
}

// newDraftToken draws a draft token for a request still in draft; "" when it
// was confirmed meanwhile. A former token stops working.
func (s *Store) newDraftToken(ctx context.Context, q store.Querier, id int64) (string, error) {
	token, err := secure.NewToken()
	if err != nil {
		return "", err
	}
	var updated int64
	err = q.QueryRowContext(ctx,
		`UPDATE tickets SET draft_token_hash = ? WHERE id = ? AND status = 'draft' RETURNING id`,
		secure.TokenHash(token), id).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store draft token: %w", err)
	}
	return token, nil
}

// DraftByToken returns the request behind a screen 2 token. ErrDraftGone for
// an unknown token or a draft past 24 hours, even before the daily purge.
func (s *Store) DraftByToken(ctx context.Context, token string) (Draft, error) {
	var (
		d         Draft
		status    Status
		ref       sql.NullString
		kbIDs     sql.NullString
		email     []byte
		emailHash []byte
		created   int64
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, status, ref, category, kb_ids, email, email_hash, created_at FROM tickets WHERE draft_token_hash = ?`,
		secure.TokenHash(token)).Scan(&d.ID, &status, &ref, &d.Category, &kbIDs, &email, &emailHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrDraftGone
	}
	if err != nil {
		return Draft{}, fmt.Errorf("find draft token: %w", err)
	}
	if status == StatusDraft && created <= s.Now().Add(-draftTTL).Unix() {
		return Draft{}, ErrDraftGone
	}
	d.Ref = ref.String
	if kbIDs.Valid {
		if err := json.Unmarshal([]byte(kbIDs.String), &d.KBIDs); err != nil {
			return Draft{}, fmt.Errorf("decode draft fiches: %w", err)
		}
	}
	if err := s.openAll([]*string{&d.Email}, email); err != nil {
		return Draft{}, err
	}
	err = s.DB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM submitted_tickets
		  WHERE email_hash = ? AND id != ? AND status IN ('todo', 'in_progress', 'waiting'))`,
		emailHash, d.ID).Scan(&d.OpenRequest)
	if err != nil {
		return Draft{}, fmt.Errorf("find open requests: %w", err)
	}
	return d, nil
}

// Abandon is « Ça règle mon problème »: it deletes the draft and its
// captures and counts an avoided request, without personal data. A request
// already confirmed is kept and its ref returned; an unknown token (a
// double tap) does nothing.
func (s *Store) Abandon(ctx context.Context, token string) (string, error) {
	var (
		ref  string
		keys []string
	)
	err := s.tx(ctx, "abandon", func(ctx context.Context, tx *sql.Tx) error {
		var (
			t      ticketRow
			stored sql.NullString
			kbIDs  sql.NullString
		)
		err := tx.QueryRowContext(ctx, `SELECT id, status, ref, category, kb_ids FROM tickets WHERE draft_token_hash = ?`,
			secure.TokenHash(token)).Scan(&t.id, &t.status, &stored, &t.category, &kbIDs)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("find draft token: %w", err)
		case t.status != StatusDraft:
			ref = stored.String
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deflections (category, kb_ids, created_at) VALUES (?, ?, ?)`,
			t.category, kbIDs.String, s.Now().Unix()); err != nil {
			return fmt.Errorf("count deflection: %w", err)
		}
		keys, err = s.drop(ctx, tx, []ticketRow{t})
		return err
	})
	if err != nil {
		return "", err
	}
	s.deleteObjects(ctx, keys)
	return ref, nil
}

// DeflectionCounts returns, per fiche id, how many avoided requests showed it.
func (s *Store) DeflectionCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT j.value, COUNT(*) FROM deflections d, json_each(d.kb_ids) j GROUP BY j.value`)
	type count struct {
		id string
		n  int
	}
	list, err := store.Collect(rows, err, func(rows *sql.Rows) (count, error) {
		var c count
		return c, rows.Scan(&c.id, &c.n)
	})
	if err != nil {
		return nil, fmt.Errorf("count deflections: %w", err)
	}
	out := make(map[string]int, len(list))
	for _, c := range list {
		out[c.id] = c.n
	}
	return out, nil
}
