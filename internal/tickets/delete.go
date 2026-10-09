package tickets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Deletions (spec §4.5). They take a version like any committee action.
const (
	ActionDeleteCapture Action = "delete_capture"
	ActionDeleteMessage Action = "delete_message"
	ActionDelete        Action = "delete"
)

// Erasure counts what an erasure removes.
type Erasure struct {
	Tickets        int
	Member         bool
	PaymentLines   int // every line of the member's name, a homonym's included
	MollieLines    int // same rule for the Mollie lines
	Participations int // calendar participations of the member's name and of the same people (lot 8)
	Cards          int // carnet cards of the member's name, a homonym's included (design 2026-10-09)
}

// deleteCapture removes one screenshot, a CACI sent by mistake for instance.
func (s *Store) deleteCapture(ctx context.Context, tx *sql.Tx, t ticketRow, cmd Command) (outcome, error) {
	var key string
	err := tx.QueryRowContext(ctx, `SELECT object_key FROM attachments WHERE id = ? AND ticket_id = ?`,
		cmd.AttachmentID, t.id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return outcome{}, ErrNotFound
	}
	if err != nil {
		return outcome{}, fmt.Errorf("find capture: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, cmd.AttachmentID); err != nil {
		return outcome{}, fmt.Errorf("delete capture: %w", err)
	}
	if err := s.transition(ctx, tx, t, t, cmd.Actor, eventCaptureDeleted, nil); err != nil {
		return outcome{}, err
	}
	return outcome{change: ChangeUpdated, objects: []string{key}}, nil
}

// deleteMessage removes one message; its captures and the mails citing it
// go with it (foreign keys ON DELETE CASCADE).
func (s *Store) deleteMessage(ctx context.Context, tx *sql.Tx, t ticketRow, cmd Command) (outcome, error) {
	keys, err := objectKeys(ctx, tx, `SELECT object_key FROM attachments WHERE message_id = ? AND ticket_id = ?`,
		cmd.MessageID, t.id)
	if err != nil {
		return outcome{}, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ? AND ticket_id = ?`, cmd.MessageID, t.id)
	if err != nil {
		return outcome{}, fmt.Errorf("delete message: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return outcome{}, errors.Join(ErrNotFound, err)
	}
	if err := s.transition(ctx, tx, t, t, cmd.Actor, eventMessageDeleted, nil); err != nil {
		return outcome{}, err
	}
	return outcome{change: ChangeUpdated, objects: keys}, nil
}

// deleteTicket removes a whole request: messages, captures, journal and mails
// cascade. Its tracking link stops working.
func (s *Store) deleteTicket(ctx context.Context, tx *sql.Tx, t ticketRow) (outcome, error) {
	keys, err := s.drop(ctx, tx, []ticketRow{t})
	if err != nil {
		return outcome{}, err
	}
	return outcome{change: ChangeDeleted, objects: keys}, nil
}

// drop deletes requests and returns their object keys. A done request is
// counted in stats_monthly first (spec §8.3).
func (s *Store) drop(ctx context.Context, tx *sql.Tx, list []ticketRow) ([]string, error) {
	var keys []string
	for _, d := range list {
		k, err := objectKeys(ctx, tx, `SELECT object_key FROM attachments WHERE ticket_id = ?`, d.id)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k...)
		if d.status == StatusDone {
			if err := s.addStats(ctx, tx, d); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tickets WHERE id = ?`, d.id); err != nil {
			return nil, fmt.Errorf("delete ticket: %w", err)
		}
	}
	return keys, nil
}

// addStats counts a closed request in its month (Europe/Paris) and category.
func (s *Store) addStats(ctx context.Context, tx *sql.Tx, d ticketRow) error {
	month := time.Unix(d.closedAt, 0).In(s.paris).Format("2006-01")
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO stats_monthly (month, category, closed_count, hours_to_close_total) VALUES (?, ?, 1, ?)
		 ON CONFLICT (month, category) DO UPDATE SET closed_count = closed_count + 1,
		   hours_to_close_total = hours_to_close_total + excluded.hours_to_close_total`,
		month, d.category, (d.closedAt-d.submittedAt)/3600); err != nil {
		return fmt.Errorf("monthly stats: %w", err)
	}
	return nil
}

// PreviewErasure counts what Erase would remove for email.
func (s *Store) PreviewErasure(ctx context.Context, email string) (Erasure, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return Erasure{}, ErrInvalid
	}
	var e Erasure
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE email_hash = ?`,
		s.Keys.Hash(normalized)).Scan(&e.Tickets); err != nil {
		return Erasure{}, fmt.Errorf("count tickets of address: %w", err)
	}
	p, member, err := s.Members.Find(ctx, normalized)
	if err != nil {
		return Erasure{}, err
	}
	e.Member = member
	if e.PaymentLines, err = s.Payments.Count(ctx, p.NameHash); err != nil {
		return Erasure{}, err
	}
	if e.MollieLines, err = s.Mollie.Count(ctx, p.NameHash); err != nil {
		return Erasure{}, err
	}
	if e.Participations, err = s.Calendar.Count(ctx, p.NameHash, p.LastName, p.FirstName); err != nil {
		return Erasure{}, err
	}
	if e.Cards, err = s.Carnets.Count(ctx, p.NameHash); err != nil {
		return Erasure{}, err
	}
	return e, nil
}

// Erase answers an erasure request (spec §4.5): every request of email, its
// members row, the payment and Mollie lines, the cards and the calendar participations
// of that member's name and every mail to it, in one transaction, then the
// stored captures. The next imports may list the address, the lines and the
// participations again.
func (s *Store) Erase(ctx context.Context, email, actor string) (Erasure, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return Erasure{}, ErrInvalid
	}
	var (
		e    Erasure
		list []ticketRow
		keys []string
	)
	err = s.tx(ctx, "erase", func(ctx context.Context, tx *sql.Tx) error {
		list, err = listDoomed(ctx, tx,
			`SELECT id, status, category, submitted_at, closed_at FROM tickets WHERE email_hash = ?`, s.Keys.Hash(normalized))
		if err != nil {
			return err
		}
		if keys, err = s.drop(ctx, tx, list); err != nil {
			return err
		}
		p, member, err := s.Members.EraseTx(ctx, tx, normalized)
		if err != nil {
			return err
		}
		e.Member = member
		if e.PaymentLines, err = s.Payments.EraseTx(ctx, tx, p.NameHash); err != nil {
			return err
		}
		if e.MollieLines, err = s.Mollie.EraseTx(ctx, tx, p.NameHash); err != nil {
			return err
		}
		if e.Participations, err = s.Calendar.EraseTx(ctx, tx, p.NameHash, p.LastName, p.FirstName); err != nil {
			return err
		}
		if e.Cards, err = s.Carnets.EraseTx(ctx, tx, p.NameHash, p.LastName, p.FirstName); err != nil {
			return err
		}
		return s.Outbox.DeleteRecipient(ctx, tx, normalized)
	})
	if err != nil {
		return Erasure{}, err
	}
	e.Tickets = len(list)
	s.deleteObjects(ctx, keys)
	for _, d := range list {
		s.changed(ChangeDeleted, d.id, actor)
	}
	s.Logger.InfoContext(ctx, "person erased", "actor", actor, "tickets", e.Tickets, "member", e.Member,
		"payment_lines", e.PaymentLines, "mollie_lines", e.MollieLines, "participations", e.Participations, "cards", e.Cards)
	return e, nil
}

// ReleaseMissing returns to todo the open requests of accounts known() rejects
// (spec §4.1): at startup and after each reload of the accounts.
func (s *Store) ReleaseMissing(ctx context.Context, known func(username string) bool) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM tickets WHERE status IN ('in_progress', 'waiting')`)
	ids, err := store.Collect(rows, err, func(rows *sql.Rows) (id int64, err error) {
		if err = rows.Scan(&id); err != nil {
			err = fmt.Errorf("scan assigned ticket: %w", err)
		}
		return id, err
	})
	if err != nil {
		return fmt.Errorf("assigned tickets: %w", err)
	}
	var errs []error
	for _, id := range ids {
		errs = append(errs, s.release(ctx, id, known))
	}
	return errors.Join(errs...)
}

func (s *Store) release(ctx context.Context, id int64, known func(string) bool) error {
	released := false
	err := s.tx(ctx, "release", func(ctx context.Context, tx *sql.Tx) error {
		t, err := s.load(ctx, tx, id)
		if err != nil {
			return err
		}
		if (t.status != StatusInProgress && t.status != StatusWaiting) || known(t.assignee) {
			return nil
		}
		after := t
		after.status, after.assignee = StatusTodo, ""
		if err := s.transition(ctx, tx, t, after, ActorSystem, eventReleased, map[string]string{dataFrom: t.assignee}); err != nil {
			return err
		}
		released = true
		return s.clubMail(ctx, tx, after, 0, mail.EventReleased, mailData{Former: s.AccountName(t.assignee)})
	})
	if err != nil {
		return err
	}
	if released {
		s.Logger.InfoContext(ctx, "ticket released, its resolver's account was deleted", "ticket_id", id)
		s.changed(ChangeUpdated, id, "")
	}
	return nil
}

// listDoomed reads the requests a fixed query selects.
func listDoomed(ctx context.Context, q store.Querier, query string, args ...any) ([]ticketRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (d ticketRow, err error) {
		var submitted, closed sql.NullInt64
		if err = rows.Scan(&d.id, &d.status, &d.category, &submitted, &closed); err != nil {
			return d, fmt.Errorf("scan ticket to delete: %w", err)
		}
		d.submittedAt, d.closedAt = submitted.Int64, closed.Int64
		return d, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list tickets to delete: %w", err)
	}
	return out, nil
}

// objectKeys reads the object keys a fixed query selects.
func objectKeys(ctx context.Context, q store.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	keys, err := store.Collect(rows, err, func(rows *sql.Rows) (k string, err error) {
		if err = rows.Scan(&k); err != nil {
			err = fmt.Errorf("scan capture object: %w", err)
		}
		return k, err
	})
	if err != nil {
		return nil, fmt.Errorf("list capture objects: %w", err)
	}
	return keys, nil
}
