package web

import (
	"errors"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// personView is a participant with how it matches the members list.
type personView struct {
	calendar.Participant

	Match string
}

// peopleList is a section of the outing page: its heading, its anchor and
// its people sorted by name.
type peopleList struct {
	Title, ID string
	People    []personView
}

// eventPageData is /calendrier/{id}: an event, its staff and its people.
type eventPageData struct {
	eventView

	Description string // the club's text without its markup
	Back        string // the week of the event
	Lists       []peopleList
}

// matchLabel writes how many members a participant's match names.
func matchLabel(members int) string {
	switch members {
	case 0:
		return "Non rapproché"
	case 1:
		return "Membre"
	}
	return "Homonyme, non rapproché"
}

// ponytail: tags dropped by pattern; html/template escapes what is left, so
// the worst case is odd spacing. A real HTML parser if layouts get lost.
var (
	lineTags  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/h[1-6])\b[^>]*>`)
	scripts   = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)\s*>`)
	anyTag    = regexp.MustCompile(`<[^>]*>`)
	blankRuns = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
)

// plainText turns the club's rich text into plain lines.
func plainText(s string) string {
	s = scripts.ReplaceAllString(s, "")
	s = lineTags.ReplaceAllString(s, "\n")
	s = anyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = blankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// eventPage shows an event and its people (lot 8 part 2). A missing one
// answers 404 with a page that says why.
func (s *Server) eventPage(w http.ResponseWriter, r *http.Request) {
	ev, err := s.calendar.Event(r.Context(), r.PathValue("id"))
	status, title := http.StatusOK, ev.Title
	var data *eventPageData
	switch {
	case errors.Is(err, calendar.ErrNotFound):
		status, title = http.StatusNotFound, "Sortie introuvable"
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		// A sorted copy: the staff of eventView keeps the pushed order.
		byName := slices.SortedStableFunc(slices.Values(ev.Participants), func(a, b calendar.Participant) int {
			return strings.Compare(secure.NormalizeName(a.Name), secure.NormalizeName(b.Name))
		})
		var registered, waiting, unregistered []personView
		for _, p := range byName {
			pv := personView{Participant: p, Match: matchLabel(p.Members)}
			switch {
			case !p.Registered:
				unregistered = append(unregistered, pv)
			case p.WaitingList:
				waiting = append(waiting, pv)
			default:
				registered = append(registered, pv)
			}
		}
		data = &eventPageData{
			eventView:   s.eventView(ev, true),
			Description: plainText(ev.Description),
			Back:        calendarLink(viewWeek, midnight(ev.Start, s.paris)),
			Lists: []peopleList{
				{Title: "Inscrits", ID: "inscrits", People: registered},
				{Title: "Liste d'attente", ID: "attente", People: waiting},
				{Title: "Non inscrits : pilotes et payeurs", ID: "non-inscrits", People: unregistered},
			},
		}
	}
	p, err := s.adminPage(r, title)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = data
	s.render(w, r, status, "sortie", p)
}
