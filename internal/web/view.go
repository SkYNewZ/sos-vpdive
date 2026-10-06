package web

import (
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// ageLevel colors the age of an open request (spec §4.2): neutral, then the
// warning color from AGE_WARN_AFTER, then the error color in bold from
// AGE_ALERT_AFTER. The text always carries the age; the color only adds to it.
type ageLevel string

const (
	ageNeutral ageLevel = "neutral"
	ageWarn    ageLevel = "warn"
	ageAlert   ageLevel = "alert"
)

type ageView struct {
	Text  string
	Level ageLevel
}

// age is the time since submission of an open request, or the processing
// time of a done one (closed is non-zero), which is never colored. A date
// still ahead reads « à venir ».
func (s *Server) age(submitted, closed time.Time) ageView {
	if !closed.IsZero() {
		return ageView{Text: elapsed(closed.Sub(submitted)), Level: ageNeutral}
	}
	d := s.now().Sub(submitted)
	if d < 0 { // an outing cancelled ahead of its date (spec §7.4)
		return ageView{Text: "à venir", Level: ageNeutral}
	}
	v := ageView{Text: elapsed(d), Level: ageNeutral}
	if d >= s.cfg.AgeAlertAfter {
		v.Level = ageAlert
	} else if d >= s.cfg.AgeWarnAfter {
		v.Level = ageWarn
	}
	return v
}

// elapsed writes a duration as « < 1 h », « 5 h » or « 12 j », rounded down.
func elapsed(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d < time.Hour:
		return "< 1 h"
	case d < day:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	default:
		return strconv.Itoa(int(d/day)) + " j"
	}
}

// accountOf returns the committee account of username, nil when there is
// none or when it left the accounts file.
func (s *Server) accountOf(username string) *admins.Account {
	a, ok := s.admins.Get(username)
	if !ok {
		return nil
	}
	return &a
}

// actorName names the author of a journal entry (tickets.Event.Actor).
func (s *Server) actorName(actor string) string {
	switch actor {
	case tickets.ActorMember:
		return "l'adhérent"
	case tickets.ActorSystem:
		return "automatique"
	default:
		return s.tickets.AccountName(actor)
	}
}

// isoDate turns a stored YYYY-MM-DD date into DD/MM/YYYY; anything else is
// shown as is.
func isoDate(v string) string {
	d, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return v
	}
	return d.Format(dateFormat)
}

// methodLabel names a payment method of the payments export: « vpaydive » is
// Mollie, seen through VPayDive (owner decision, lot 7).
func methodLabel(method string) string {
	if method == payments.MethodVPayDive {
		return "Mollie (VPayDive)"
	}
	return method
}
