package assistant

import (
	"context"
	"errors"
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
)

// Store errors.
var (
	ErrNotFound = errors.New("conversation not found or erased")
	ErrBusy     = errors.New("an answer is already running for this account")
	ErrFull     = errors.New("conversation reached its question limit")
)

// Person is someone the model refers to by Ref (m1, m2…): the server keeps
// how to find their data, the model never sees their address.
type Person struct {
	Ref      string
	Name     string
	Email    string
	NameHash []byte
	Seasons  string
	Licence  string
	Shared   int // members bearing this name, this one included
}

// Source is a piece of data an answer read: an import with its date, or an
// outing, a request or a fiche with its committee page.
type Source struct {
	Label string
	Date  string // import date; "" for a page
	Stale bool
	Link  string // committee page; "" for an import
}

// Exchange is one question and its answer, as the page shows them again.
type Exchange struct {
	Question string // as typed: it never leaves the server unmasked
	Steps    []string
	Answer   string // Markdown
	Sources  []Source
}

// Conversation is a resolver's exchange with the model, in memory only.
type Conversation struct {
	ID        string
	Account   string
	TicketID  int64 // the request « Analyser » opened it on; 0 from /assistant
	Created   time.Time
	Seen      time.Time
	History   []Message
	Exchanges []Exchange
	People    []Person
	Emails    []string // [email N] is Emails[N-1]

	session string
}

// AddPerson returns the ref of p, adding it once by address.
func (c *Conversation) AddPerson(p Person) string {
	for _, known := range c.People {
		if known.Email == p.Email {
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

// Expires is when c is erased if no question comes before.
func (c *Conversation) Expires() time.Time {
	idle, end := c.Seen.Add(idleLife), c.Created.Add(maxLife)
	if idle.Before(end) {
		return idle
	}
	return end
}

// Store keeps the conversations in memory: a restart erases them all.
type Store struct {
	mu    sync.Mutex
	now   func() time.Time
	convs map[string]*Conversation
	busy  map[string]context.CancelFunc // accounts with an answer in flight, and how to stop it
}

// NewStore returns an empty store; now is injectable for tests.
func NewStore(now func() time.Time) *Store {
	return &Store{now: now, convs: map[string]*Conversation{}, busy: map[string]context.CancelFunc{}}
}

// Begin reserves account's answer slot and returns a copy of the
// conversation to continue: id's when set; else session's conversation on
// ticketID when there is one; else a new one. stop cancels the answer (an
// erasure calls it through DropAll). The caller ends with Finish or Abort.
func (s *Store) Begin(session, account, id string, ticketID int64, stop context.CancelFunc) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if s.busy[account] != nil {
		return Conversation{}, ErrBusy
	}
	if stop == nil {
		stop = func() {} // a nil stop would read as a free slot
	}
	var c *Conversation
	switch {
	case id != "":
		if c = s.convs[id]; c == nil || c.session != session {
			return Conversation{}, ErrNotFound
		}
	case ticketID != 0:
		c = s.forTicket(session, ticketID)
	}
	if c == nil {
		token, err := secure.NewToken()
		if err != nil {
			return Conversation{}, err
		}
		now := s.now()
		c = &Conversation{ID: token, Account: account, TicketID: ticketID, Created: now, Seen: now, session: session}
		s.convs[c.ID] = c
	}
	if len(c.Exchanges) >= MaxQuestions {
		return Conversation{}, ErrFull
	}
	s.busy[account] = stop
	return copyOf(c), nil
}

// DropAll erases every conversation and stops the answers in flight: an
// erasure or a deleted request must leave no copy of what it removed, and a
// conversation only lives half an hour anyway.
func (s *Store) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.convs)
	for _, stop := range s.busy {
		stop()
	}
}

// Finish stores c, grown by an answer, and frees its account's slot. A
// conversation dropped meanwhile (logout) stays dropped.
func (s *Store) Finish(c Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, c.Account)
	if stored := s.convs[c.ID]; stored != nil {
		c.Seen = s.now()
		*stored = c
	}
}

// Abort frees c's account slot and keeps the conversation as it was.
func (s *Store) Abort(c Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, c.Account)
}

// Find returns a copy of session's conversation id.
func (s *Store) Find(session, id string) (Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	c := s.convs[id]
	if c == nil || c.session != session {
		return Conversation{}, false
	}
	return copyOf(c), true
}

// ForTicket returns a copy of session's conversation on ticketID.
func (s *Store) ForTicket(session string, ticketID int64) (Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if c := s.forTicket(session, ticketID); c != nil {
		return copyOf(c), true
	}
	return Conversation{}, false
}

// Drop erases session's conversations, at logout.
func (s *Store) Drop(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.convs {
		if c.session == session {
			delete(s.convs, id)
		}
	}
}

// sweep erases the expired conversations; s.mu is held.
func (s *Store) sweep() {
	now := s.now()
	for id, c := range s.convs {
		if !now.Before(c.Expires()) {
			delete(s.convs, id)
		}
	}
}

// forTicket returns session's conversation on ticketID; s.mu is held.
func (s *Store) forTicket(session string, ticketID int64) *Conversation {
	for _, c := range s.convs {
		if c.session == session && c.TicketID == ticketID {
			return c
		}
	}
	return nil
}

// copyOf is c with its slices copied, so that an answer grows its own.
func copyOf(c *Conversation) Conversation {
	out := *c
	out.History = slices.Clone(c.History)
	out.Exchanges = slices.Clone(c.Exchanges)
	out.People = slices.Clone(c.People)
	out.Emails = slices.Clone(c.Emails)
	return out
}
