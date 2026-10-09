package payments

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// ErrCheckNotFound reports a fingerprint that matches no line to check in
// place: unknown, malformed, or gone with the latest import.
var ErrCheckNotFound = errors.New("no line to check has this fingerprint")

// CheckSignal is why a line needs a look (spec §7.7). The tool never says it
// is an error, only that someone must look.
type CheckSignal string

// Signals of the « Paiements à vérifier » page. Cancelled outings keep their
// own page (spec §7.4).
const (
	SignalUnsettled CheckSignal = "unsettled" // collected by Mollie, not settled in VPDive
	SignalPartial   CheckSignal = "partial"   // a partial payment in the payments export
)

// Person is who a line belongs to, as the members list says: lines hold no
// name.
type Person struct {
	Name   string // first and last name of a member of that name; "" when none
	Shared bool   // several members bear the name, or the line is marked « ambiguë »
}

// Dismissal records who masked a line, and when.
type Dismissal struct {
	By string
	At time.Time
}

// Check is a line to look at.
type Check struct {
	Signal      CheckSignal
	Fingerprint string // HMAC (hex) of source, name, date, product and amount: survives imports
	Person      Person
	Date        time.Time // payment date; a partial line without one gives its creation date
	Product     string
	Service     string     // Mollie: the outing or rental the line pays for
	Amount      Amount     // Mollie: the amount collected; partial: the amount paid
	UnitPrice   Amount     // partial only
	Dismissal   *Dismissal // nil while not masked
}

// CheckStore lists the lines to check of both imports and keeps their
// dismissals (spec §7.7).
type CheckStore struct {
	db       *sql.DB
	keys     *secure.Keys
	now      func() time.Time
	members  *members.Store
	payments LineTable[Line]
	mollie   LineTable[MollieLine]
}

// NewCheckStore returns a CheckStore; now is injectable for tests.
func NewCheckStore(db *sql.DB, keys *secure.Keys, ms *members.Store, now func() time.Time) *CheckStore {
	return &CheckStore{db: db, keys: keys, now: now, members: ms,
		payments: paymentLines(db, keys, now), mollie: mollieLines(db, keys, now)}
}

// List returns the lines to check in place, open and masked, oldest first.
//
// ponytail: decrypts every payment and Mollie line at each call (a few
// thousand, a few ms), as Cancellations does; Count and Dismiss too. Store
// the fingerprint in clear at import if it ever shows in traces.
func (s *CheckStore) List(ctx context.Context) (open, masked []Check, err error) {
	checks, err := s.checks(ctx)
	if err != nil {
		return nil, nil, err
	}
	dismissed, err := dismissals(ctx, s.db)
	if err != nil {
		return nil, nil, err
	}
	names := map[string]Person{}
	for _, c := range checks {
		person, ok := names[string(c.nameHash)]
		if !ok {
			name, members, err := s.members.Named(ctx, c.nameHash)
			if err != nil {
				return nil, nil, err
			}
			person = Person{Name: name, Shared: members > 1}
			names[string(c.nameHash)] = person
		}
		c.Person.Name = person.Name
		c.Person.Shared = c.Person.Shared || person.Shared
		if d, ok := dismissed[c.Fingerprint]; ok {
			c.Dismissal = &d
			masked = append(masked, c.Check)
			continue
		}
		open = append(open, c.Check)
	}
	return open, masked, nil
}

// Count counts the lines in place still to check, by signal: the report of
// each import gives them (spec §7.7). A masked line is checked.
func (s *CheckStore) Count(ctx context.Context) (unsettled, partial int, err error) {
	open, _, err := s.List(ctx)
	unsettled, partial = Tally(open)
	return unsettled, partial, err
}

// Tally counts checks by signal.
func Tally(checks []Check) (unsettled, partial int) {
	for _, c := range checks {
		if c.Signal == SignalUnsettled {
			unsettled++
		} else {
			partial++
		}
	}
	return unsettled, partial
}

// Dismiss masks the line to check of fingerprint for good: the mask follows
// the line through later imports. The first dismissal is kept.
func (s *CheckStore) Dismiss(ctx context.Context, fingerprint, username string) error {
	checks, err := s.checks(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(checks, func(c check) bool { return c.Fingerprint == fingerprint }) {
		return ErrCheckNotFound
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dismissed_checks (fingerprint, dismissed_by, dismissed_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		fingerprint, username, s.now().Unix()); err != nil {
		return fmt.Errorf("dismiss check: %w", err)
	}
	return nil
}

// check is a Check with the name hash it was found under.
type check struct {
	Check

	nameHash []byte
}

// checks reads every line to check in place, oldest first.
func (s *CheckStore) checks(ctx context.Context) ([]check, error) {
	var out []check
	mollie, err := s.mollie.sealed(ctx, s.mollie.q.all)
	if err != nil {
		return nil, err
	}
	for _, r := range mollie {
		l, err := openLine[MollieLine](s.keys, r.data)
		if err != nil {
			return nil, err
		}
		if l.Unsettled() {
			out = append(out, check{
				nameHash: r.nameHash, Signal: SignalUnsettled, Fingerprint: l.fingerprint(s.keys, r.nameHash),
				Person: Person{Shared: r.ambiguous}, Date: l.PaidAt, Product: l.Product, Service: l.Service, Amount: l.Amount,
			})
		}
	}
	payments, err := s.payments.sealed(ctx, s.payments.q.all)
	if err != nil {
		return nil, err
	}
	for _, r := range payments {
		l, err := openLine[Line](s.keys, r.data)
		if err != nil {
			return nil, err
		}
		if l.Partial() {
			out = append(out, check{
				nameHash: r.nameHash, Signal: SignalPartial, Fingerprint: l.fingerprint(s.keys, r.nameHash),
				Person: Person{Shared: r.ambiguous}, Date: l.checkDate(), Product: l.Product, Amount: l.Paid, UnitPrice: l.UnitPrice,
			})
		}
	}
	slices.SortStableFunc(out, func(a, b check) int {
		return cmp.Or(a.Date.Compare(b.Date), strings.Compare(a.Product, b.Product))
	})
	return out, nil
}

// dismissals returns every dismissal by fingerprint: a handful of rows.
func dismissals(ctx context.Context, db *sql.DB) (map[string]Dismissal, error) {
	rows, err := db.QueryContext(ctx, `SELECT fingerprint, dismissed_by, dismissed_at FROM dismissed_checks`)
	type row struct {
		fingerprint, by string
		at              int64
	}
	found, err := store.Collect(rows, err, func(rows *sql.Rows) (r row, err error) {
		err = rows.Scan(&r.fingerprint, &r.by, &r.at)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("dismissed checks: %w", err)
	}
	out := make(map[string]Dismissal, len(found))
	for _, r := range found {
		out[r.fingerprint] = Dismissal{By: r.by, At: time.Unix(r.at, 0).UTC()}
	}
	return out, nil
}

// dismissal returns the dismissal of the line of fingerprint, nil when none.
func dismissal(dismissed map[string]Dismissal, fingerprint string) *Dismissal {
	d, ok := dismissed[fingerprint]
	if !ok {
		return nil
	}
	return &d
}

// fingerprint is the hex HMAC of the parts of a line that identify it
// across imports: the export has no line identifier (spec §7.7).
func fingerprint(keys *secure.Keys, parts ...string) string {
	return keys.HashHex(strings.Join(parts, "\x1f"))
}

func (l MollieLine) fingerprint(keys *secure.Keys, nameHash []byte) string {
	return fingerprint(keys, string(SignalUnsettled), string(nameHash), l.PaidAt.UTC().Format(time.RFC3339),
		l.Product, l.Service, strconv.FormatInt(int64(l.Amount), 10))
}

func (l Line) fingerprint(keys *secure.Keys, nameHash []byte) string {
	return fingerprint(keys, string(SignalPartial), string(nameHash), l.checkDate().UTC().Format(time.RFC3339),
		l.Product, strconv.FormatInt(int64(l.Paid), 10))
}

// checkDate is the date of a line to check: its payment date, or its
// creation date when the export gives none.
func (l Line) checkDate() time.Time {
	if l.PaidAt.IsZero() {
		return l.Created
	}
	return l.PaidAt
}
