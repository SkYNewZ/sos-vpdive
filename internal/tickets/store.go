package tickets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/blobs"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/carnets"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Status is the state of a request (spec §8.1). A draft is invisible to the
// committee.
type Status string

// Statuses, as stored in tickets.status.
const (
	StatusDraft      Status = "draft"
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusWaiting    Status = "waiting"
	StatusDone       Status = "done"
)

// Label is the status in words, as the committee reads it.
func (s Status) Label() string {
	switch s {
	case StatusTodo:
		return "À traiter"
	case StatusInProgress:
		return "En cours"
	case StatusWaiting:
		return "En attente de l'adhérent"
	case StatusDone:
		return "Fait"
	case StatusDraft:
		return "Brouillon"
	default:
		return string(s)
	}
}

// MemberLabel is the status in words on the member's tracking page.
func (s Status) MemberLabel() string {
	if s == StatusWaiting {
		return "En attente de ta réponse"
	}
	return s.Label()
}

// Open reports whether the request is still being handled.
func (s Status) Open() bool {
	return s == StatusTodo || s == StatusInProgress || s == StatusWaiting
}

// ChangeType is the kind of change pushed to the live board.
type ChangeType string

// Change types (spec §4.2).
const (
	ChangeCreated ChangeType = "created"
	ChangeUpdated ChangeType = "changed"
	ChangeReplied ChangeType = "replied"
	ChangeDeleted ChangeType = "deleted"
)

// Change is what the live board receives: never personal data.
type Change struct {
	Type     ChangeType
	TicketID int64
	Actor    string // the resolver who made the change; "" for the member or the system
}

// Errors returned by the store.
var (
	ErrNotFound        = errors.New("ticket not found")
	ErrStale           = errors.New("ticket changed since the page was displayed")
	ErrNotAllowed      = errors.New("action not allowed in this status")
	ErrInvalid         = errors.New("invalid command")
	ErrTooManyCaptures = errors.New("too many captures")
	ErrStorage         = errors.New("capture storage unavailable")
)

// Limits of spec §3.1 and §11.4.
const (
	MaxCapturesPerPost   = 3
	MaxCapturesPerTicket = 10
	ReplyWindow          = 14 * 24 * time.Hour
	DescriptionMin       = 20
	DescriptionMax       = 4000
	MessageMax           = 4000
	NameMax              = 100
)

// Keys of events.data.
const (
	dataFrom   = "from"
	dataTo     = "to"
	dataStatus = "status"
)

// Message authors, as stored in messages.author_type.
const (
	authorMember = "member"
	authorAdmin  = "admin"
)

// Journal actors that are not committee usernames (events.actor).
const (
	ActorMember = "member"
	ActorSystem = "system"
)

// eventType is a journal event type, as stored in events.type.
type eventType string

// Journal event types.
const (
	eventSubmitted       eventType = "submitted"
	eventTaken           eventType = "taken"
	eventReassigned      eventType = "reassigned"
	eventUnassigned      eventType = "unassigned"
	eventWaiting         eventType = "waiting"
	eventResumed         eventType = "resumed"
	eventMemberReplied   eventType = "member_replied"
	eventReopened        eventType = "reopened"
	eventCategoryChanged eventType = "category_changed"
	eventClosed          eventType = "closed"
	eventClosedByMember  eventType = "closed_by_member"
	eventReleased        eventType = "released"
	eventCaptureDeleted  eventType = "capture_deleted"
	eventMessageDeleted  eventType = "message_deleted"
)

// CleanText turns CRLF and CR into LF and trims spaces; ok is false when the
// rune count is outside [minRunes, maxRunes]. Browsers send textarea line
// breaks as CRLF: counting after the conversion keeps the limit the member sees.
func CleanText(s string, minRunes, maxRunes int) (clean string, ok bool) {
	s = strings.TrimSpace(normalizeNewlines(strings.ToValidUTF8(s, string(utf8.RuneError))))
	n := utf8.RuneCountInString(s)
	return s, n >= minRunes && n <= maxRunes
}

// Deps are the store's collaborators.
type Deps struct {
	DB            *sql.DB
	Keys          *secure.Keys
	Catalog       *Catalog
	Members       *members.Store
	Payments      *payments.Store
	Mollie        *payments.MollieStore
	Calendar      *calendar.Store
	Carnets       *carnets.Store
	Outbox        *mail.Outbox
	Blobs         blobs.Store
	Account       func(username string) (admins.Account, bool)
	BaseURL       *url.URL       // members site, in member mails
	AdminBaseURL  *url.URL       // committee site, in club mails
	ClubEmail     string         // NOTIFY_EMAIL address, recipient of club mails
	Alerts        []mail.Channel // configured alert channels besides the club mail; none allowed
	RetentionDays int
	Now           func() time.Time
	Logger        *slog.Logger
	OnChange      func(Change) // after each commit; nil allowed
}

// Store reads and changes requests.
type Store struct {
	Deps

	paris *time.Location
}

// NewStore returns a Store.
func NewStore(d Deps) *Store {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		// The distroless image ships the zone database; this only guards odd hosts.
		d.Logger.Warn("Europe/Paris time zone unavailable, monthly stats use UTC", "error", err)
		paris = time.UTC
	}
	return &Store{Deps: d, paris: paris}
}

// AccountName is "Name (Role)", or the username of an account that left the file.
func (s *Store) AccountName(username string) string {
	if a, ok := s.Account(username); ok {
		return a.Name + " (" + a.Role + ")"
	}
	return username
}

// ticketRow is a request as actions see it, personal columns decrypted.
type ticketRow struct {
	id          int64
	ref         string
	status      Status
	assignee    string
	version     int64
	category    string
	firstName   string
	lastName    string
	email       string
	token       string
	submittedAt int64
	closedAt    int64
}

// tx runs fn in a transaction traced as "db tickets.<name>". It owns the
// outbox wake-up: once committed, the mails fn queued are sent at once.
func (s *Store) tx(ctx context.Context, name string, fn func(context.Context, *sql.Tx) error) error {
	if err := store.Tx(ctx, s.DB, "tickets."+name, fn); err != nil {
		return err
	}
	s.Outbox.Wake()
	return nil
}

// load reads one request, drafts included. ErrNotFound when absent.
func (s *Store) load(ctx context.Context, q store.Querier, id int64) (ticketRow, error) {
	var (
		t                         ticketRow
		ref, assignee             sql.NullString
		submitted, closed         sql.NullInt64
		first, last, email, token []byte
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, ref, status, assignee, version, category, first_name, last_name, email, token, submitted_at, closed_at
		 FROM tickets WHERE id = ?`, id).
		Scan(&t.id, &ref, &t.status, &assignee, &t.version, &t.category, &first, &last, &email, &token, &submitted, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return ticketRow{}, ErrNotFound
	}
	if err != nil {
		return ticketRow{}, fmt.Errorf("load ticket: %w", err)
	}
	t.ref, t.assignee, t.submittedAt, t.closedAt = ref.String, assignee.String, submitted.Int64, closed.Int64
	if err := s.openAll([]*string{&t.firstName, &t.lastName, &t.email, &t.token}, first, last, email, token); err != nil {
		return ticketRow{}, err
	}
	return t, nil
}

// openAll decrypts sealed values into dst, in order.
func (s *Store) openAll(dst []*string, sealed ...[]byte) error {
	for i, v := range sealed {
		plain, err := s.Keys.OpenString(v)
		if err != nil {
			return fmt.Errorf("decrypt ticket data: %w", err)
		}
		*dst[i] = plain
	}
	return nil
}

// openOptional decrypts a sealed column that may be NULL, such as the
// summary, into dst; NULL leaves dst empty.
func (s *Store) openOptional(dst *string, sealed []byte) error {
	if sealed == nil {
		return nil
	}
	return s.openAll([]*string{dst}, sealed)
}

// addEvent journals an event. data never holds personal data.
func (s *Store) addEvent(ctx context.Context, tx *sql.Tx, ticketID int64, typ eventType, actor string, data map[string]string) error {
	raw := []byte("{}")
	if len(data) > 0 {
		var err error
		if raw, err = json.Marshal(data); err != nil {
			return fmt.Errorf("encode event data: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (ticket_id, type, actor, data, created_at) VALUES (?, ?, ?, ?, ?)`,
		ticketID, string(typ), actor, string(raw), s.Now().Unix()); err != nil {
		return fmt.Errorf("journal %s: %w", typ, err)
	}
	return nil
}

// changed runs after a commit: it tells the live board. actor is the
// resolver who made the change, "" for the member or the system.
func (s *Store) changed(typ ChangeType, id int64, actor string) {
	if s.OnChange != nil {
		s.OnChange(Change{Type: typ, TicketID: id, Actor: actor})
	}
}

// deleteObjects removes stored captures after a commit. A failure is left to
// the daily orphan sweep (spec §9.8).
func (s *Store) deleteObjects(ctx context.Context, keys []string) {
	for _, k := range keys {
		if err := s.Blobs.Delete(ctx, k); err != nil {
			s.Logger.WarnContext(ctx, "delete capture object, the orphan sweep will retry", "error", err)
		}
	}
}

func (s *Store) trackingLink(token string) string {
	return s.BaseURL.String() + "/suivi/" + token
}

func (s *Store) adminLink(id int64) string {
	return s.AdminBaseURL.String() + "/demandes/" + strconv.FormatInt(id, 10)
}

// save writes the mutable columns of after and bumps the version. Zero rows
// means another action changed the request since before was read.
func (s *Store) save(ctx context.Context, tx *sql.Tx, before, after ticketRow) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET status = ?, assignee = ?, category = ?, closed_at = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		after.status, store.NullIfZero(after.assignee), after.category, store.NullIfZero(after.closedAt), s.Now().Unix(),
		before.id, before.version)
	if err != nil {
		return fmt.Errorf("update ticket: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update ticket: %w", err)
	}
	if n == 0 {
		return ErrStale
	}
	return nil
}

// insertMessage adds a thread message; author "" is the member.
func (s *Store) insertMessage(ctx context.Context, tx *sql.Tx, ticketID int64, author string, internal bool, body string) (int64, error) {
	authorType := authorAdmin
	if author == "" {
		authorType = authorMember
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, author_type, author, internal, created_at, body) VALUES (?, ?, ?, ?, ?, ?)`,
		ticketID, authorType, store.NullIfZero(author), internal, s.Now().Unix(), s.Keys.SealString(body))
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	return id, nil
}

// truncate cuts s to n runes, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
