package web

import (
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

// The signals of an outing (spec §7.4, §7.7 as amended): each has a clear
// rule and asks for a check in VPDive.
const (
	signalCarnet = "Sortie annulée, carnet encore débité : il sera recrédité à la suppression de la sortie."
	signalMoney  = "Sortie annulée, réglée en argent réel : le trésorier rembourse."
	signalMollie = "Encaissé par Mollie, non soldé dans VPDive : à vérifier."
)

// outing is one of the requester's outings on a request page: their own
// participation, the lines of theirs that attach to it, and its signals.
type outing struct {
	calendar.Participation

	Lines   []payments.Line
	Mollie  []payments.CollectedLine
	Signals []string
}

// attach builds the requester's outings (lot 8 part 2): a dated line goes to
// their outing of the same Paris day, by normalised title when they have
// several that day. Only the day of a line's date counts. A line no outing
// takes stays in the payments blocks only.
func attach(ps []calendar.Participation, lines []payments.Line, mollie []payments.CollectedLine, paris *time.Location) []outing {
	out := make([]outing, len(ps))
	for i, p := range ps {
		out[i].Participation = p
	}
	for _, l := range lines {
		if i := pickOuting(ps, l.Product, l.Starts, paris); i >= 0 {
			out[i].Lines = append(out[i].Lines, l)
		}
	}
	for _, l := range mollie {
		if i := pickOuting(ps, l.Service, l.Starts, paris); i >= 0 {
			out[i].Mollie = append(out[i].Mollie, l)
		}
	}
	for i := range out {
		out[i].Signals = signals(out[i])
	}
	return out
}

// pickOuting returns the index of the outing a line of title dated starts
// goes to, or -1.
func pickOuting(ps []calendar.Participation, title string, starts time.Time, paris *time.Location) int {
	if starts.IsZero() {
		return -1
	}
	day := parisDay(starts, paris)
	sameDay := make([]int, 0, len(ps))
	sameTitle := make([]int, 0, len(ps))
	for i, p := range ps {
		if parisDay(p.Event.Start, paris) != day {
			continue
		}
		sameDay = append(sameDay, i)
		if normTitle(p.Event.Title) == normTitle(title) {
			sameTitle = append(sameTitle, i)
		}
	}
	switch {
	case len(sameDay) == 1:
		return sameDay[0]
	case len(sameTitle) == 1:
		return sameTitle[0]
	}
	return -1
}

// parisDay is the Paris date of t, as YYYY-MM-DD.
func parisDay(t time.Time, paris *time.Location) string { return t.In(paris).Format(time.DateOnly) }

// normTitle is a title as compared: lower case, white space runs as one
// space, trimmed.
func normTitle(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// signals lists what the lines of an outing tell, each once.
func signals(o outing) []string {
	var carnet, money, mollie bool
	if o.Event.Cancelled() {
		for _, l := range o.Lines {
			if l.State != payments.StatePaid {
				continue
			}
			switch {
			case l.Prepaid():
				carnet = true
			case l.Paid > 0:
				money = true
			}
		}
	}
	for _, l := range o.Mollie {
		if l.Unsettled() && l.Dismissal == nil {
			mollie = true
		}
	}
	var out []string
	if carnet {
		out = append(out, signalCarnet)
	}
	if money {
		out = append(out, signalMoney)
	}
	if mollie {
		out = append(out, signalMollie)
	}
	return out
}
