package tickets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// excerptRunes is the length of the description shown on a board row.
const excerptRunes = 140

// Attachment is a stored screenshot, without its content.
type Attachment struct {
	ID        int64
	MessageID int64 // 0 when sent with the form
	MIME      string
}

// Message is one entry of the thread.
type Message struct {
	ID         int64
	FromMember bool
	Author     string // username of a committee message, "" for the member
	Internal   bool
	Body       string
	CreatedAt  time.Time
	Captures   []Attachment
}

// Event is one journal entry.
type Event struct {
	Type      string
	Actor     string // username, "member" or "system"
	Data      map[string]string
	CreatedAt time.Time
}

// Detail is a request with its thread and journal.
type Detail struct {
	ID          int64
	Ref         string
	Status      Status
	Assignee    string
	Version     int64
	Category    string
	FirstName   string
	LastName    string
	Email       string
	Fields      Fields
	Description string
	Summary     string   // written by the model for the committee, "" without one (spec §5.6)
	KBIDs       []string // fiches chosen at submission, nil when the model did not answer (spec §5.3)
	SubmittedAt time.Time
	ClosedAt    time.Time    // zero while open
	Captures    []Attachment // sent with the form
	Messages    []Message    // oldest first, internal notes included
	Events      []Event      // oldest first
}

// Public returns the messages without internal notes (tracking page, mails).
func (d *Detail) Public() []Message {
	out := make([]Message, 0, len(d.Messages))
	for _, m := range d.Messages {
		if !m.Internal {
			out = append(out, m)
		}
	}
	return out
}

// Row is a request as a list shows it.
type Row struct {
	ID                int64
	Ref               string
	FirstName         string
	LastName          string
	Category          string
	Excerpt           string // description start, one line, 140 runes at most
	Summary           string // the model's summary, shown instead of Excerpt when set
	Status            Status
	Assignee          string
	SubmittedAt       time.Time
	ClosedAt          time.Time
	MemberRepliedLast bool // last public message is the member's
}

// AssigneeNobody filters the requests without a resolver.
const AssigneeNobody = "-"

// Filter selects board rows.
type Filter struct {
	Statuses []Status // empty: todo and in_progress
	Assignee string   // "": anyone; AssigneeNobody; else a username
	Category string   // "": every category
}

// rowColumns are the columns scanRows reads, from tickets aliased t.
const rowColumns = `t.id, t.ref, t.first_name, t.last_name, t.category, t.description, t.summary, t.status, t.assignee,
	t.submitted_at, t.closed_at,
	(SELECT m.author_type FROM messages m WHERE m.ticket_id = t.id AND m.internal = 0 ORDER BY m.id DESC LIMIT 1)`

// Board lists requests, oldest submitted first; drafts never appear.
func (s *Store) Board(ctx context.Context, f Filter) ([]Row, error) {
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = []Status{StatusTodo, StatusInProgress}
	}
	set, err := json.Marshal(statuses)
	if err != nil {
		return nil, fmt.Errorf("encode statuses: %w", err)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+rowColumns+` FROM submitted_tickets t
		WHERE t.status IN (SELECT value FROM json_each(?))
		  AND (? = '' OR t.category = ?)
		  AND (? = '' OR (? = '`+AssigneeNobody+`' AND t.assignee IS NULL) OR t.assignee = ?)
		ORDER BY t.submitted_at, t.id`,
		string(set), f.Category, f.Category, f.Assignee, f.Assignee, f.Assignee)
	if err != nil {
		return nil, fmt.Errorf("board: %w", err)
	}
	return s.scanRows(rows)
}

// Others lists the other requests of the same address: open first, then newest.
func (s *Store) Others(ctx context.Context, id int64) ([]Row, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+rowColumns+` FROM submitted_tickets t
		WHERE t.email_hash = (SELECT email_hash FROM tickets WHERE id = ?) AND t.id != ?
		ORDER BY t.status = 'done', t.submitted_at DESC, t.id DESC`, id, id)
	if err != nil {
		return nil, fmt.Errorf("other tickets: %w", err)
	}
	return s.scanRows(rows)
}

func (s *Store) scanRows(rows *sql.Rows) ([]Row, error) {
	out, err := store.Collect(rows, nil, func(rows *sql.Rows) (Row, error) {
		var (
			r                        Row
			ref, assignee, lastBy    sql.NullString
			submitted, closed        sql.NullInt64
			first, last, description []byte
			summary                  []byte
		)
		if err := rows.Scan(&r.ID, &ref, &first, &last, &r.Category, &description, &summary, &r.Status, &assignee,
			&submitted, &closed, &lastBy); err != nil {
			return r, fmt.Errorf("scan ticket row: %w", err)
		}
		if err := s.openAll([]*string{&r.FirstName, &r.LastName, &r.Excerpt}, first, last, description); err != nil {
			return r, err
		}
		if err := s.openOptional(&r.Summary, summary); err != nil {
			return r, err
		}
		r.Ref, r.Assignee = ref.String, assignee.String
		r.SubmittedAt, r.ClosedAt = store.UnixTime(submitted), store.UnixTime(closed)
		r.Excerpt = truncate(strings.Join(strings.Fields(r.Excerpt), " "), excerptRunes)
		r.MemberRepliedLast = lastBy.String == authorMember
		return r, nil
	})
	if err != nil {
		return nil, fmt.Errorf("ticket rows: %w", err)
	}
	return out, nil
}

// Detail returns a confirmed request with its thread and journal.
func (s *Store) Detail(ctx context.Context, id int64) (*Detail, error) {
	var (
		d                                    Detail
		assignee, kbIDs                      sql.NullString
		submitted, closed                    sql.NullInt64
		first, last, email, fields, describe []byte
		summary                              []byte
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, ref, status, assignee, version, category, first_name, last_name, email, fields, description,
		  summary, kb_ids, submitted_at, closed_at
		 FROM submitted_tickets WHERE id = ?`, id).
		Scan(&d.ID, &d.Ref, &d.Status, &assignee, &d.Version, &d.Category, &first, &last, &email, &fields, &describe,
			&summary, &kbIDs, &submitted, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read ticket: %w", err)
	}
	var rawFields string
	if err := s.openAll([]*string{&d.FirstName, &d.LastName, &d.Email, &rawFields, &d.Description},
		first, last, email, fields, describe); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rawFields), &d.Fields); err != nil {
		return nil, fmt.Errorf("decode ticket fields: %w", err)
	}
	if err := s.openOptional(&d.Summary, summary); err != nil {
		return nil, err
	}
	if d.KBIDs, err = decodeKBIDs(kbIDs); err != nil {
		return nil, err
	}
	d.Assignee = assignee.String
	d.SubmittedAt, d.ClosedAt = store.UnixTime(submitted), store.UnixTime(closed)
	if err := s.readThread(ctx, &d); err != nil {
		return nil, err
	}
	if d.Events, err = s.readEvents(ctx, d.ID); err != nil {
		return nil, err
	}
	return &d, nil
}

// ByToken returns the request of a tracking link. ErrNotFound for drafts too.
func (s *Store) ByToken(ctx context.Context, token string) (*Detail, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM submitted_tickets WHERE token_hash = ?`,
		secure.TokenHash(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find tracking token: %w", err)
	}
	return s.Detail(ctx, id)
}

// CanReply reports whether the member may still write: open, or done within ReplyWindow.
func (s *Store) CanReply(d *Detail) bool {
	return s.canReply(d.Status, d.ClosedAt)
}

// readThread fills d.Messages and d.Captures.
func (s *Store) readThread(ctx context.Context, d *Detail) error {
	captures, err := s.readAttachments(ctx, d.ID)
	if err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, author_type, author, internal, created_at, body FROM messages WHERE ticket_id = ? ORDER BY id`, d.ID)
	d.Messages, err = store.Collect(rows, err, func(rows *sql.Rows) (Message, error) {
		var (
			m          Message
			authorType string
			author     sql.NullString
			created    int64
			body       []byte
		)
		if err := rows.Scan(&m.ID, &authorType, &author, &m.Internal, &created, &body); err != nil {
			return m, fmt.Errorf("scan message: %w", err)
		}
		if m.Body, err = s.Keys.OpenString(body); err != nil {
			return m, fmt.Errorf("decrypt message: %w", err)
		}
		m.FromMember, m.Author, m.CreatedAt = authorType == authorMember, author.String, time.Unix(created, 0).UTC()
		m.Captures = captures[m.ID]
		return m, nil
	})
	if err != nil {
		return fmt.Errorf("read messages: %w", err)
	}
	d.Captures = captures[0]
	return nil
}

// readAttachments groups a request's captures by message id, 0 for the form.
func (s *Store) readAttachments(ctx context.Context, ticketID int64) (map[int64][]Attachment, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, message_id, mime FROM attachments WHERE ticket_id = ? ORDER BY id`, ticketID)
	list, err := store.Collect(rows, err, func(rows *sql.Rows) (Attachment, error) {
		var (
			a       Attachment
			message sql.NullInt64
		)
		if err := rows.Scan(&a.ID, &message, &a.MIME); err != nil {
			return a, fmt.Errorf("scan attachment: %w", err)
		}
		a.MessageID = message.Int64
		return a, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read attachments: %w", err)
	}
	out := map[int64][]Attachment{}
	for _, a := range list {
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	return out, nil
}

func (s *Store) readEvents(ctx context.Context, ticketID int64) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT type, actor, data, created_at FROM events WHERE ticket_id = ? ORDER BY id`, ticketID)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (Event, error) {
		var (
			e       Event
			data    string
			created int64
		)
		if err := rows.Scan(&e.Type, &e.Actor, &data, &created); err != nil {
			return e, fmt.Errorf("scan event: %w", err)
		}
		if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
			return e, fmt.Errorf("decode event data: %w", err)
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return out, nil
}

// Capture returns a decrypted screenshot of ticketID. ErrNotFound, ErrStorage.
func (s *Store) Capture(ctx context.Context, ticketID, attachmentID int64) (data []byte, mime string, err error) {
	var key string
	err = s.DB.QueryRowContext(ctx,
		`SELECT a.object_key, a.mime FROM attachments a JOIN submitted_tickets t ON t.id = a.ticket_id
		 WHERE a.id = ? AND a.ticket_id = ?`, attachmentID, ticketID).Scan(&key, &mime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("find capture: %w", err)
	}
	sealed, err := s.Blobs.Get(ctx, key)
	if err != nil {
		s.Logger.WarnContext(ctx, "capture unavailable", "ticket_id", ticketID, "error", err)
		return nil, "", fmt.Errorf("%w: %w", ErrStorage, err)
	}
	if data, err = s.Keys.Open(sealed); err != nil {
		return nil, "", fmt.Errorf("decrypt capture: %w", err)
	}
	return data, mime, nil
}

// Describe is the French journal line of e, without its actor.
func (s *Store) Describe(e Event) string {
	switch eventType(e.Type) {
	case eventSubmitted:
		return "Demande envoyée"
	case eventTaken:
		return "Prise en charge"
	case eventReassigned:
		return "Réassignée à " + s.AccountName(e.Data[dataTo])
	case eventUnassigned:
		return "Assignation retirée, demande remise à traiter"
	case eventWaiting:
		return "Passée en attente de l'adhérent"
	case eventResumed:
		return "Reprise sans attendre l'adhérent"
	case eventMemberReplied:
		return "Réponse de l'adhérent : demande repassée en cours"
	case eventReopened:
		return "Rouverte par une réponse de l'adhérent : " + Status(e.Data[dataStatus]).Label()
	case eventCategoryChanged:
		return "Catégorie changée : " + s.Catalog.CategoryLabel(e.Data[dataFrom]) + " → " + s.Catalog.CategoryLabel(e.Data[dataTo])
	case eventClosed:
		return "Close"
	case eventClosedByMember:
		return "Marquée réglée par l'adhérent"
	case eventReleased:
		return "Remise à traiter : le compte de " + s.AccountName(e.Data[dataFrom]) + " a été retiré"
	case eventCaptureDeleted:
		return "Capture supprimée"
	case eventMessageDeleted:
		return "Message supprimé"
	default:
		return e.Type
	}
}

// SendLinks queues the lost-link mail when email has confirmed requests
// (spec §3.5): the original links, decrypted from tickets.token.
func (s *Store) SendLinks(ctx context.Context, email string) error {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return nil //nolint:nilerr // an unusable address has no request: same answer as an unknown one
	}
	return s.tx(ctx, "lost_links", func(ctx context.Context, tx *sql.Tx) error {
		links, err := s.linksOf(ctx, tx, s.Keys.Hash(normalized))
		if err != nil || len(links) == 0 {
			return err
		}
		return s.queue(ctx, tx, mail.Mail{Event: mail.EventLostLink, To: normalized}, mailData{Links: links})
	})
}

// linksOf returns the tracking links of every confirmed request of an address.
func (s *Store) linksOf(ctx context.Context, tx *sql.Tx, emailHash []byte) ([]refLink, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ref, token FROM submitted_tickets WHERE email_hash = ? ORDER BY submitted_at, id`, emailHash)
	links, err := store.Collect(rows, err, func(rows *sql.Rows) (refLink, error) {
		var (
			ref    string
			sealed []byte
		)
		if err := rows.Scan(&ref, &sealed); err != nil {
			return refLink{}, fmt.Errorf("scan ticket link: %w", err)
		}
		token, err := s.Keys.OpenString(sealed)
		if err != nil {
			return refLink{}, fmt.Errorf("decrypt tracking token: %w", err)
		}
		return refLink{Ref: ref, Link: s.trackingLink(token)}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("find tickets of address: %w", err)
	}
	return links, nil
}
