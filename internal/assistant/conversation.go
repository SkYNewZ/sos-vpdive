package assistant

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Conversation limits (design: Limits).
const (
	idleLife     = 30 * time.Minute
	maxLife      = 2 * time.Hour
	MaxQuestions = 20
	// maxHistory is the most history, in bytes, a new question starts
	// from: about 70 000 tokens, well under the providers' context, with
	// room left for one answer's tool results.
	maxHistory = 256 << 10
)

// Store errors.
var (
	ErrNotFound = errors.New("conversation not found or erased")
	ErrExpired  = errors.New("conversation resumption expired")
	ErrBusy     = errors.New("an answer is already running for this account")
	ErrFull     = errors.New("conversation reached its question limit")
	// ErrSessionEnded is the cause Drop stops an answer with.
	ErrSessionEnded = errors.New("session ended")
)

// Person is someone the model refers to by Ref (m1, m2…): the server keeps
// how to find their data, the model never sees their address.
type Person struct {
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	NameHash []byte `json:"name_hash"`
	Seasons  string `json:"seasons"`
	Licence  string `json:"licence"`
	Shared   int    `json:"shared"` // members bearing this name, this one included; 2 for a name the campaign holds twice
}

// Source is a piece of data an answer read: an import with its date, or an
// outing, a request or a fiche with its committee page.
type Source struct {
	Label string `json:"label"`
	Date  string `json:"date"` // import date; "" for a page
	Stale bool   `json:"stale"`
	Link  string `json:"link"` // committee page; "" for an import
}

// Exchange is one question and its answer, as the page shows them again.
type Exchange struct {
	Question string   `json:"question"` // as typed: it never leaves the server unmasked
	Steps    []string `json:"steps"`
	Answer   string   `json:"answer"` // Markdown
	Sources  []Source `json:"sources"`
}

// Conversation is an account-private exchange with the model.
type Conversation struct {
	Title     string
	ID        string
	Account   string
	TicketID  int64 // the request « Analyser » opened it on; 0 from /assistant
	Created   time.Time
	Seen      time.Time
	History   []Message
	Exchanges []Exchange
	People    []Person
	Emails    []string // [email N] is Emails[N-1]

	revision     int64
	historyBytes int
	answer       uint64 // the reservation that only this copy can release
}

// AddPerson returns the ref of p, adding it once by address, or by name
// hash for someone known by the membership campaign only, who has none.
func (c *Conversation) AddPerson(p Person) string {
	for _, known := range c.People {
		if known.Email == p.Email && (p.Email != "" || bytes.Equal(known.NameHash, p.NameHash)) {
			return known.Ref
		}
	}
	p.Ref = "m" + strconv.Itoa(len(c.People)+1)
	c.People = append(c.People, p)
	return p.Ref
}

// Person returns the person of ref.
func (c *Conversation) Person(ref string) (Person, bool) {
	i := slices.IndexFunc(c.People, func(p Person) bool { return p.Ref == ref })
	if i < 0 {
		return Person{}, false
	}
	return c.People[i], true
}

// Expires is the resumption deadline; the archive lives for seven days.
func (c *Conversation) Expires() time.Time {
	idle, end := c.Seen.Add(idleLife), c.Created.Add(maxLife)
	if idle.Before(end) {
		return idle
	}
	return end
}

// CanResume reports whether a new question may reuse this context.
func (c *Conversation) CanResume(now time.Time) bool { return c.resumeError(now) == nil }

func (c *Conversation) resumeError(now time.Time) error {
	if !now.Before(c.Expires()) {
		return ErrExpired
	}
	if len(c.Exchanges) >= MaxQuestions || c.historyBytes > maxHistory {
		return ErrFull
	}
	return nil
}

// Store persists encrypted conversations; only live answer reservations stay in memory.
type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	keys    *secure.Keys
	now     func() time.Time
	busy    map[string]slot
	answers uint64
}

type slot struct {
	answer  uint64
	session string
	stop    context.CancelCauseFunc
	err     error
}

// NewStore opens account-private history and initializes live answer reservations.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{db: db, keys: keys, now: now, busy: map[string]slot{}}
}

// Begin reserves one account's answer, loading context only after checking its limits.
func (s *Store) Begin(ctx context.Context, session, account, id string, ticketID int64, stop context.CancelCauseFunc) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.busy[account]; ok {
		return Conversation{}, ErrBusy
	}
	var c Conversation
	var err error
	if id != "" {
		c, err = s.read(ctx, account, id, true)
		if err != nil {
			return Conversation{}, err
		}
	} else {
		c.ID, err = secure.NewToken()
		if err != nil {
			return Conversation{}, err
		}
		c.Account, c.TicketID, c.Created, c.Seen = account, ticketID, s.now(), s.now()
		c.Title = "Conversation du " + c.Created.Format("02/01/2006")
		if err = s.create(ctx, c); err != nil {
			return Conversation{}, err
		}
	}
	if stop == nil {
		stop = func(error) {}
	}
	s.answers++
	c.answer = s.answers
	s.busy[account] = slot{answer: c.answer, session: session, stop: stop}
	return c, nil
}

// Finish atomically saves a complete exchange before the caller signals success.
// A late completion cannot recreate a deleted row or overwrite a newer answer.
func (s *Store) Finish(ctx context.Context, c Conversation) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.busy[c.Account]
	if !ok || a.answer != c.answer {
		return Conversation{}, ErrNotFound
	}
	defer delete(s.busy, c.Account)
	if a.err != nil {
		return Conversation{}, a.err
	}
	c.Seen = s.now()
	if err := s.save(ctx, c); err != nil {
		return Conversation{}, err
	}
	c.revision++
	c.historyBytes = historySize(c.History)
	return c, nil
}

// Abort releases only this answer and removes an uncompleted initial conversation.
func (s *Store) Abort(ctx context.Context, c Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.busy[c.Account]; ok && a.answer != c.answer {
		return nil
	}
	delete(s.busy, c.Account)
	_, err := s.db.ExecContext(ctx, `DELETE FROM assistant_conversations WHERE id=? AND account=? AND question_count=0 AND revision=?`, c.ID, c.Account, c.revision)
	if err != nil {
		return fmt.Errorf("abort conversation: %w", err)
	}
	return nil
}

// Find loads the private visible archive, never its provider context.
func (s *Store) Find(ctx context.Context, account, id string) (Conversation, error) {
	return s.read(ctx, account, id, false)
}

// ForTicket loads the most recent completed analysis, including its read-only state.
func (s *Store) ForTicket(ctx context.Context, account string, ticketID int64) (Conversation, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM assistant_conversations WHERE account=? AND ticket_id=? AND question_count>0 AND created_at>? ORDER BY seen_at DESC,rowid DESC LIMIT 1`, account, ticketID, s.now().Add(-historyLife).UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("find ticket conversation: %w", err)
	}
	return s.Find(ctx, account, id)
}

// Drop cancels live answers of sessions that ended; saved archives stay account-private.
func (s *Store) Drop(sessions ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for account, a := range s.busy {
		if slices.Contains(sessions, a.session) {
			a.err = ErrSessionEnded
			s.busy[account] = a
			a.stop(ErrSessionEnded)
		}
	}
}

// DropAll cancels live answers after transactional content erasure.
func (s *Store) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for account, a := range s.busy {
		a.err = ErrNotFound
		s.busy[account] = a
		a.stop(nil)
	}
}

// historySize is the size of h as sent to the provider, in bytes.
func historySize(h []Message) int {
	n := 0
	for _, m := range h {
		for _, b := range m.Content {
			n += len(b)
		}
	}
	return n
}
