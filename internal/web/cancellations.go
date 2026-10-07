package web

import (
	"context"
	"net/http"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

// cancellationsData is the treasurer's list of cancelled outings (spec §7.4).
type cancellationsData struct {
	payments.Cancellations

	Rows []outingView
}

// outingView is an outing with its age, counted from its date: the date of
// the cancellation is not in the export.
type outingView struct {
	payments.Outing

	Age  ageView
	Link string // its calendar page, or its day view; "" without a date
}

func (s *Server) cancellationsPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.payments.Cancellations(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := cancellationsData{Cancellations: c, Rows: make([]outingView, len(c.Outings))}
	for i, o := range c.Outings {
		d.Rows[i].Outing = o
		d.Rows[i].Age = ageView{Text: "inconnue", Level: ageNeutral}
		if !o.Starts.IsZero() {
			d.Rows[i].Age = s.age(o.Starts, time.Time{})
		}
	}
	if err := s.linkOutings(r.Context(), d.Rows); err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Sorties annulées")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, http.StatusOK, "annulations", p)
}

// linkOutings points each dated cancelled outing at the calendar: its page
// when one event has its title and Paris day, else its day view (lot 8
// part 2). It reads the events of the range, without participants.
func (s *Server) linkOutings(ctx context.Context, rows []outingView) error {
	var first, last time.Time
	for _, o := range rows {
		if o.Starts.IsZero() {
			continue
		}
		if first.IsZero() || o.Starts.Before(first) {
			first = o.Starts
		}
		if o.Starts.After(last) {
			last = o.Starts
		}
	}
	if first.IsZero() {
		return nil
	}
	events, err := s.calendar.Range(ctx, midnight(first, s.paris), midnight(last, s.paris).AddDate(0, 0, 1), false)
	if err != nil {
		return err
	}
	for i, o := range rows {
		if o.Starts.IsZero() {
			continue
		}
		day, title := parisDay(o.Starts, s.paris), normTitle(o.Title)
		rows[i].Link = calendarLink(viewDay, midnight(o.Starts, s.paris))
		var id string
		matches := 0
		for _, ev := range events {
			if parisDay(ev.Start, s.paris) == day && normTitle(ev.Title) == title {
				id = ev.ID
				matches++
			}
		}
		if matches == 1 {
			rows[i].Link = "/calendrier/" + id
		}
	}
	return nil
}
