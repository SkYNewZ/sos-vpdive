package web

import (
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
// the cancellation is not in the export. Age is nil when the date is unknown.
type outingView struct {
	payments.Outing

	Age *ageView
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
		switch {
		case o.Starts.After(s.now()): // cancelled ahead of time
			d.Rows[i].Age = &ageView{Text: "à venir", Level: ageNeutral}
		case !o.Starts.IsZero():
			age := s.age(o.Starts, time.Time{})
			d.Rows[i].Age = &age
		}
	}
	p, err := s.adminPage(r, "Sorties annulées")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, http.StatusOK, "annulations", p)
}
