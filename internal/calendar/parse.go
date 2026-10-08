// Package calendar holds the club's activity calendar that the external
// script pushes (spec §7.6 as amended, lot 8): its reading, its storage by
// event, the purge of the history and the erasure of a person's
// participations.
package calendar

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Event is one activity of the calendar, with every field the script sends.
// Participants are stored in their own rows: the event's sealed data leaves
// them out.
type Event struct {
	ID              string           `json:"id"`
	URL             string           `json:"url"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	StartsAt        string           `json:"starts_at"` // RFC 3339, as pushed
	EndsAt          string           `json:"ends_at"`   // may precede StartsAt: kept as pushed
	AllDay          bool             `json:"all_day"`
	Category        string           `json:"category"`
	Color           string           `json:"color"`
	TextColor       string           `json:"text_color"`
	Activity        string           `json:"activity"`
	Environment     string           `json:"environment"`
	Location        string           `json:"location"`
	MaxParticipants *int             `json:"max_participants"` // nil without a limit
	Boats           []string         `json:"boats"`
	Participants    []Participant    `json:"participants,omitempty"`
	Unregistrations []Unregistration `json:"unregistrations,omitempty"` // in their own rows

	// Start and End are StartsAt and EndsAt read: parsed on a push, from the
	// stored columns on a read; never in the sealed data. End may precede
	// Start: kept as pushed.
	Start, End time.Time `json:"-"`
}

// Participant is a person the event knows: registered, pilot or payer.
type Participant struct {
	VPDiveID       int64    `json:"vpdive_id"`
	Name           string   `json:"name"`
	LastName       string   `json:"last_name"`  // empty unless registered
	FirstName      string   `json:"first_name"` // empty unless registered
	Registered     bool     `json:"registered"`
	WaitingList    bool     `json:"waiting_list"`
	Status         string   `json:"status"`
	People         int      `json:"people"`
	Guest          bool     `json:"guest"`
	Tariff         string   `json:"tariff"`
	Qualifications []string `json:"qualifications"`
	Roles          []Role   `json:"roles"`
	Payment        *Payment `json:"payment"` // nil without a cart

	// Members is how many members the participant's match names (spec §7.3
	// as amended), set by Store.Event: 0 when nothing matches.
	Members int `json:"-"`
}

// Unregistration is a person who left the event: when, and by whom (lot 8
// part 3). It names them only; no VPDive id.
type Unregistration struct {
	LastName  string `json:"last_name"`
	FirstName string `json:"first_name"`
	At        string `json:"at"` // RFC 3339, as pushed
	By        string `json:"by"` // the author as the script writes it; may be empty

	// Time is At read: parsed on a push and on a read, never sealed.
	Time time.Time `json:"-"`
}

// Role is a role on the event; Confirmed is false while only proposed.
type Role struct {
	Name      string `json:"name"`
	Boat      string `json:"boat,omitempty"`
	Confirmed bool   `json:"confirmed"`
}

// PaymentStatus is the state of a participant's cart.
type PaymentStatus string

// Cart states of the contract.
const (
	PaymentPaid    PaymentStatus = "paid"
	PaymentPartial PaymentStatus = "partial"
	PaymentUnpaid  PaymentStatus = "unpaid"
)

// Payment is what a participant's cart says, in cents.
type Payment struct {
	Status    PaymentStatus `json:"status"`
	DueCents  int64         `json:"due_cents"`
	PaidCents int64         `json:"paid_cents"`
}

// Export is a pushed calendar, read and validated.
type Export struct {
	From, To time.Time // first and last day of the window, midnight in Paris
	Events   []Event   // the events that started within the retention
	Skipped  int       // events that started before it: never stored
	FileHash []byte    // HMAC of the body, set by the caller (spec §7.6)

	start time.Time // window start: From, or the retention cutoff when later
}

// window is where the push is complete: stored events that start there and
// are missing from it go, and the « under half » guard counts there.
func (e *Export) window() (start, end time.Time) { return e.start, e.To.AddDate(0, 0, 1) }

// inWindow counts the pushed events that start in the window.
func (e *Export) inWindow() int {
	start, end := e.window()
	n := 0
	for _, ev := range e.Events {
		if !ev.Start.Before(start) && ev.Start.Before(end) {
			n++
		}
	}
	return n
}

// cutoff is the retention limit: an event is kept 12 months after its start
// (owner decision, spec §8.3 as amended).
func cutoff(now time.Time) time.Time { return now.AddDate(-1, 0, 0) }

// ProblemKind classifies a refused calendar.
type ProblemKind string

// Reasons to refuse a calendar. The pushed route answers invalid_calendar
// for all of them.
const (
	ProblemUnreadable ProblemKind = "unreadable" // not JSON of the expected shape
	ProblemWindow     ProblemKind = "window"     // from or to missing, unreadable or reversed
	ProblemEventID    ProblemKind = "event_id"   // empty or repeated event id
	ProblemDates      ProblemKind = "dates"      // starts_at or ends_at unreadable
	ProblemPerson     ProblemKind = "person"     // participant without vpdive_id, or unregistration without both names
)

// ParseError refuses a calendar. Event is the id of the event in cause,
// never a name. It wraps no error: a time.Parse error quotes the raw value,
// which could be anything the body holds.
type ParseError struct {
	Kind  ProblemKind
	Event string
}

func (e *ParseError) Error() string {
	if e.Event != "" {
		return "calendar refused: " + string(e.Kind) + " (event " + e.Event + ")"
	}
	return "calendar refused: " + string(e.Kind)
}

// Parse reads a pushed calendar. The window is made of Paris days; events
// that started before the retention cutoff of now are counted in Skipped
// and dropped. Unknown fields are ignored.
func Parse(data []byte, paris *time.Location, now time.Time) (*Export, error) {
	var doc struct {
		From   string  `json:"from"`
		To     string  `json:"to"`
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, &ParseError{Kind: ProblemUnreadable}
	}
	from, errFrom := time.ParseInLocation(time.DateOnly, doc.From, paris)
	to, errTo := time.ParseInLocation(time.DateOnly, doc.To, paris)
	if errFrom != nil || errTo != nil || to.Before(from) {
		return nil, &ParseError{Kind: ProblemWindow}
	}
	seen := make(map[string]bool, len(doc.Events))
	for i := range doc.Events {
		ev := &doc.Events[i]
		if ev.ID == "" || seen[ev.ID] {
			return nil, &ParseError{Kind: ProblemEventID, Event: ev.ID}
		}
		seen[ev.ID] = true
		var errStart, errEnd error
		ev.Start, errStart = time.Parse(time.RFC3339, ev.StartsAt)
		ev.End, errEnd = time.Parse(time.RFC3339, ev.EndsAt)
		if errStart != nil || errEnd != nil {
			return nil, &ParseError{Kind: ProblemDates, Event: ev.ID}
		}
		if slices.ContainsFunc(ev.Participants, func(p Participant) bool { return p.VPDiveID <= 0 }) {
			return nil, &ParseError{Kind: ProblemPerson, Event: ev.ID}
		}
		if kind := checkUnregistrations(ev.Unregistrations); kind != "" {
			return nil, &ParseError{Kind: kind, Event: ev.ID}
		}
	}
	limit := cutoff(now)
	read := len(doc.Events)
	events := slices.DeleteFunc(doc.Events, func(ev Event) bool { return ev.Start.Before(limit) })
	exp := &Export{From: from, To: to, Events: events, Skipped: read - len(events), start: from}
	if limit.After(from) {
		exp.start = limit
	}
	return exp, nil
}

// checkUnregistrations sets the Time of each unregistration and returns the
// problem of the first bad one, or "": both names must hold a letter, and
// at must be RFC 3339.
func checkUnregistrations(us []Unregistration) ProblemKind {
	for i := range us {
		u := &us[i]
		if secure.NormalizeName(u.LastName) == "" || secure.NormalizeName(u.FirstName) == "" {
			return ProblemPerson
		}
		t, err := time.Parse(time.RFC3339, u.At)
		if err != nil {
			return ProblemDates
		}
		u.Time = t
	}
	return ""
}
