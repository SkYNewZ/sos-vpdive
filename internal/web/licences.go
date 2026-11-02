package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/SkYNewZ/sos-vpdive/internal/ffessm"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// licenceTTL is how long a register answer is kept in memory.
const licenceTTL = time.Hour

// licenceSearcher is the federation's licence register (ffessm.Client).
type licenceSearcher interface {
	Search(ctx context.Context, query string) ([]ffessm.Licence, error)
}

// licenceRow is what the committee may see of a register entry: no contact
// detail or address exists. The birth date and the club tell homonyms apart:
// the register holds many.
type licenceRow struct {
	Number string
	Name   string
	Club   string
	Born   time.Time
	Until  time.Time
	Active bool
}

// licenceState is the verdict on one person.
type licenceState string

const (
	licenceNone    licenceState = "none"
	licenceValid   licenceState = "valid"
	licenceExpired licenceState = "expired"
	licenceSeveral licenceState = "several"
)

// licenceCache keeps register answers in memory only: queries and results
// are personal data.
type licenceCache struct {
	mu sync.Mutex
	m  map[string]licenceEntry
}

type licenceEntry struct {
	rows []licenceRow
	at   time.Time
}

// licenceRows searches the register, an hour at most behind it.
func (s *Server) licenceRows(ctx context.Context, query string) ([]licenceRow, error) {
	key := strings.ToLower(strings.Join(strings.Fields(query), " "))
	now := s.now()
	c := &s.licenceMemo
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < licenceTTL {
		return e.rows, nil
	}
	found, err := s.licences.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	rows := make([]licenceRow, len(found))
	for i, l := range found {
		rows[i] = licenceRow{Number: l.Number, Name: licenceName(l.LastName, l.FirstName), Club: l.Club, Born: s.licenceDay(l.BirthDate), Until: s.licenceDay(l.ValidUntil), Active: l.Active}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]licenceEntry{}
	}
	c.m[key] = licenceEntry{rows: rows, at: now}
	time.AfterFunc(licenceTTL, func() { c.sweep(s.now()) })
	return rows, nil
}

// sweep drops the answers older than licenceTTL.
func (c *licenceCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.m {
		if now.Sub(e.at) >= licenceTTL {
			delete(c.m, k)
		}
	}
}

// clear forgets every answer, after a person's data is erased.
func (c *licenceCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = nil
}

// licenceName spells a holder LEMAIRE Marie-Christine: the case tells the last
// name from the first, whatever case the register holds.
func licenceName(last, first string) string {
	var b strings.Builder
	start := true
	for _, r := range first {
		if start {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		start = !unicode.IsLetter(r)
	}
	return strings.TrimSpace(strings.ToUpper(last) + " " + b.String())
}

// licenceDay reads an ISO date of the register; zero when it holds none.
func (s *Server) licenceDay(v string) time.Time {
	day, err := time.ParseInLocation(time.DateOnly, v, s.paris)
	if err != nil {
		return time.Time{}
	}
	return day
}

// valid tells whether the register calls the licence active and today is not
// past its last day.
func (s *Server) licenceValid(r licenceRow) bool {
	return r.Active && !r.Until.IsZero() && !r.Until.Before(midnight(s.now(), s.paris))
}

// nameKey makes first-last and last-first spellings equal, Jean-Pierre and
// Jean Pierre too.
func nameKey(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == '-' })
	for i, w := range words {
		words[i] = secure.NormalizeName(w)
	}
	slices.Sort(words)
	return strings.Join(words, " ")
}

// licenceOf finds one person's entries by email, never by name alone: the
// register holds many homonyms. The name must match too, since relatives may
// share an address.
func (s *Server) licenceOf(ctx context.Context, email, name string) ([]licenceRow, error) {
	if email == "" {
		return nil, nil
	}
	rows, err := s.licenceRows(ctx, email)
	if err != nil {
		return nil, err
	}
	want := nameKey(name)
	return slices.DeleteFunc(slices.Clone(rows), func(r licenceRow) bool { return nameKey(r.Name) != want }), nil
}

// licenceSummary is the verdict on one person's entries.
type licenceSummary struct {
	State  licenceState
	Number string
	Until  string // dd/mm/yyyy
}

func (s *Server) summarize(rows []licenceRow) licenceSummary {
	switch len(rows) {
	case 0:
		return licenceSummary{State: licenceNone}
	case 1:
		r := rows[0]
		st := licenceExpired
		if s.licenceValid(r) {
			st = licenceValid
		}
		return licenceSummary{State: st, Number: r.Number, Until: s.formatDate(r.Until)}
	}
	return licenceSummary{State: licenceSeveral}
}

// licenceLine is one row of the search page.
type licenceLine struct {
	Name  string
	Born  string
	Club  string
	Until string
	Valid bool
}

type licencesData struct {
	Q        string
	Searched bool
	Problem  string
	Lines    []licenceLine
}

// licencesPage searches the register on demand: who each entry is, so that
// homonyms are told apart, whether it is valid and until when.
func (s *Server) licencesPage(w http.ResponseWriter, r *http.Request) {
	var d licencesData
	if r.Method == http.MethodPost { // never in the URL: a name would land in the proxy's logs
		if !s.postForm(w, r) {
			return
		}
		d.Q = strings.TrimSpace(r.PostForm.Get("q"))
	}
	if d.Q != "" {
		rows, err := s.licenceRows(r.Context(), d.Q)
		switch {
		case errors.Is(err, ffessm.ErrQueryTooShort):
			d.Problem = "Tape au moins 3 caractères."
		case err != nil:
			s.logger.WarnContext(r.Context(), "licence search failed", "error", err)
			d.Problem = "Le registre de la FFESSM ne répond pas, réessaie dans un moment."
		default:
			d.Searched = true
			for _, row := range rows {
				d.Lines = append(d.Lines, licenceLine{Name: row.Name, Born: s.formatDate(row.Born), Club: row.Club, Until: s.formatDate(row.Until), Valid: s.licenceValid(row)})
			}
		}
	}
	p, err := s.adminPage(r, "Licence FFESSM")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, http.StatusOK, "licences", p)
}

// licenceTile is the fragment the request page fetches.
type licenceTile struct {
	licenceSummary

	Problem bool
	Search  string // name to look for by hand when the address finds nothing
	CSRF    string
}

// ticketLicence answers the tile of a request: the requester's licence number
// and last day, nothing more.
func (s *Server) ticketLicence(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	t, err := s.tickets.Detail(r.Context(), id)
	if errors.Is(err, tickets.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	name := strings.TrimSpace(t.FirstName + " " + t.LastName)
	rows, err := s.licenceOf(r.Context(), t.Email, name)
	if err != nil {
		s.logger.WarnContext(r.Context(), "licence lookup failed", "error", err)
		s.renderFragment(w, r, "licence_tuile", licenceTile{Problem: true})
		return
	}
	sess, _ := sessionFrom(r.Context())
	s.renderFragment(w, r, "licence_tuile", licenceTile{licenceSummary: s.summarize(rows), Search: name, CSRF: s.keys.CSRFToken(sess.hash)})
}
