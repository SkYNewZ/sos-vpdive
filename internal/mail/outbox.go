// Package mail queues notifications in the outbox table and delivers them
// by SMTP from a background worker (spec §6). A notification is born in the
// transaction of the event that causes it; delivery never blocks a request.
package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

const (
	tracerName = "github.com/SkYNewZ/sos-vpdive/internal/mail"
	// deliveryWindow is how long a mail keeps being retried (spec §6).
	deliveryWindow = 7 * 24 * time.Hour
	// retention is how long a sent or failed mail stays in the outbox (spec §8.3).
	retention = 30 * 24 * time.Hour
)

// Event names what a mail is about. It is stored and logged: it never
// carries personal data.
type Event string

// Mail events (spec §6).
const (
	EventSubmitted     Event = "submitted"      // member: acknowledgement with tracking link
	EventNewTicket     Event = "new_ticket"     // club
	EventTaken         Event = "taken"          // member
	EventReplied       Event = "replied"        // member
	EventWaiting       Event = "waiting"        // member
	EventClosed        Event = "closed"         // member
	EventMemberReplied Event = "member_replied" // club
	EventLostLink      Event = "lost_link"      // member
	EventReleased      Event = "released"       // club
)

// Label is the French label shown on /envois.
func (e Event) Label() string {
	switch e {
	case EventSubmitted:
		return "Accusé de réception"
	case EventNewTicket:
		return "Nouvelle demande (club)"
	case EventTaken:
		return "Prise en charge"
	case EventReplied:
		return "Réponse du comité"
	case EventWaiting:
		return "Réponse attendue"
	case EventClosed:
		return "Demande réglée"
	case EventMemberReplied:
		return "Réponse de l'adhérent (club)"
	case EventLostLink:
		return "Liens de suivi"
	case EventReleased:
		return "Demande remise à traiter (club)"
	default:
		return string(e)
	}
}

// Mail is one notification to queue. TicketID and MessageID are 0 when the
// mail cites no request or no message; deleting either deletes the mail.
type Mail struct {
	TicketID  int64
	MessageID int64
	Event     Event
	To        string // bare address, normalized
	Subject   string // never member-typed text
	Text      string // text/plain body
}

// Message is what a Sender delivers.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender delivers one message. An error wrapping ErrPermanent is a
// definitive refusal; any other error is retried.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

var (
	// ErrPermanent marks a definitive refusal, such as an unknown recipient.
	ErrPermanent = errors.New("permanent delivery failure")
	// ErrNotFound reports a retry of a mail that is not in failure.
	ErrNotFound = errors.New("failed mail not found")
)

// outboxStatus is the status column of the outbox table.
type outboxStatus string

const (
	statusPending outboxStatus = "pending"
	statusSent    outboxStatus = "sent"
	statusFailed  outboxStatus = "failed"
)

// Failed is a mail that will not be retried without a committee action.
type Failed struct {
	ID        int64
	TicketID  int64  // 0 for a lost-link mail
	Ref       string // "" for a lost-link mail
	Event     Event
	To        string
	CreatedAt time.Time
	FailedAt  time.Time
}

// Outbox is the queue of notifications (table outbox).
type Outbox struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
	wake chan struct{}
}

// NewOutbox returns the queue; now is injectable for tests.
func NewOutbox(db *sql.DB, keys *secure.Keys, now func() time.Time) *Outbox {
	return &Outbox{db: db, keys: keys, now: now, wake: make(chan struct{}, 1)}
}

// Enqueue seals and inserts m inside the caller's transaction (spec §6: a
// notification is born with its event). Call Wake after the commit.
func (o *Outbox) Enqueue(ctx context.Context, tx *sql.Tx, m Mail) error {
	to, err := secure.NormalizeEmail(m.To)
	if err != nil {
		return fmt.Errorf("enqueue %s mail: recipient: %w", m.Event, err)
	}
	now := o.now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO outbox (ticket_id, message_id, event, status, next_attempt_at, give_up_at, created_at,
		                     recipient_hash, recipient, subject, body)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		store.NullIfZero(m.TicketID), store.NullIfZero(m.MessageID), string(m.Event), string(statusPending),
		now.Unix(), now.Add(deliveryWindow).Unix(), now.Unix(),
		o.keys.Hash(to), o.keys.SealString(to), o.keys.SealString(m.Subject), o.keys.SealString(m.Text),
	); err != nil {
		return fmt.Errorf("enqueue %s mail: %w", m.Event, err)
	}
	return nil
}

// Wake asks the worker for a pass now. It never blocks.
func (o *Outbox) Wake() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Failed lists the mails in failure, newest first.
func (o *Outbox) Failed(ctx context.Context) ([]Failed, error) {
	rows, err := o.db.QueryContext(ctx,
		`SELECT o.id, COALESCE(o.ticket_id, 0), COALESCE(t.ref, ''), o.event, o.recipient, o.created_at, o.failed_at
		 FROM outbox o LEFT JOIN tickets t ON t.id = o.ticket_id
		 WHERE o.status = ? ORDER BY o.failed_at DESC, o.id DESC`, string(statusFailed))
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (f Failed, err error) {
		var (
			event            string
			recipient        []byte
			created, failure int64
		)
		if err = rows.Scan(&f.ID, &f.TicketID, &f.Ref, &event, &recipient, &created, &failure); err != nil {
			return f, err
		}
		if f.To, err = o.keys.OpenString(recipient); err != nil {
			return f, fmt.Errorf("mail %d: recipient: %w", f.ID, err)
		}
		f.Event = Event(event)
		f.CreatedAt, f.FailedAt = time.Unix(created, 0).UTC(), time.Unix(failure, 0).UTC()
		return f, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed mails: %w", err)
	}
	return out, nil
}

// FailedCount counts the mails in failure, for the committee banner.
func (o *Outbox) FailedCount(ctx context.Context) (int, error) {
	var n int
	if err := o.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE status = ?`,
		string(statusFailed)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count failed mails: %w", err)
	}
	return n, nil
}

// Retry puts a failed mail back in the queue with a new 7-day window.
func (o *Outbox) Retry(ctx context.Context, id int64) error {
	now := o.now()
	res, err := o.db.ExecContext(ctx,
		`UPDATE outbox SET status = ?, attempts = 0, next_attempt_at = ?, give_up_at = ?, failed_at = NULL
		 WHERE id = ? AND status = ?`,
		string(statusPending), now.Unix(), now.Add(deliveryWindow).Unix(), id, string(statusFailed))
	if err != nil {
		return fmt.Errorf("retry mail %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("retry mail %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	o.Wake()
	return nil
}

// DeleteRecipient removes every mail to email (normalized) in tx (erasure).
func (o *Outbox) DeleteRecipient(ctx context.Context, tx *sql.Tx, email string) error {
	to, err := secure.NormalizeEmail(email)
	if err != nil {
		return fmt.Errorf("delete mails of a recipient: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE recipient_hash = ?`, o.keys.Hash(to)); err != nil {
		return fmt.Errorf("delete mails of a recipient: %w", err)
	}
	return nil
}

// Purge deletes sent and failed mails 30 days after sending or failure.
func (o *Outbox) Purge(ctx context.Context) error {
	cutoff := o.now().Add(-retention).Unix()
	if _, err := o.db.ExecContext(ctx,
		`DELETE FROM outbox WHERE (status = ? AND sent_at < ?) OR (status = ? AND failed_at < ?)`,
		string(statusSent), cutoff, string(statusFailed), cutoff); err != nil {
		return fmt.Errorf("purge outbox: %w", err)
	}
	return nil
}
