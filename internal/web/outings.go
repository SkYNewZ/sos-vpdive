package web

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
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

	Lines  []payments.Line
	Mollie []payments.CollectedLine
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
	return out
}

// pickOuting returns the index of the outing a line of title dated starts
// goes to, or -1. An outing the requester holds two seats in counts once,
// at its first.
func pickOuting(ps []calendar.Participation, title string, starts time.Time, paris *time.Location) int {
	if starts.IsZero() {
		return -1
	}
	day := parisDay(starts, paris)
	sameDay := make([]int, 0, len(ps))
	sameTitle := make([]int, 0, len(ps))
	for i, p := range ps {
		if parisDay(p.Event.Start, paris) != day ||
			slices.ContainsFunc(sameDay, func(j int) bool { return ps[j].Event.ID == p.Event.ID }) {
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

// Signals lists what the lines of an outing tell, each once.
func (o outing) Signals() []string {
	var out []string
	if o.Event.Cancelled() && slices.ContainsFunc(o.Lines, func(l payments.Line) bool { return l.State == payments.StatePaid && l.Prepaid() }) {
		out = append(out, signalCarnet)
	}
	if o.Event.Cancelled() && slices.ContainsFunc(o.Lines, func(l payments.Line) bool { return l.State == payments.StatePaid && !l.Prepaid() && l.Paid > 0 }) {
		out = append(out, signalMoney)
	}
	if slices.ContainsFunc(o.Mollie, func(l payments.CollectedLine) bool { return l.Unsettled() && l.Dismissal == nil }) {
		out = append(out, signalMollie)
	}
	return out
}

// outingsState says what the « Sorties VPDive » block shows, checked in
// this order.
type outingsState string

const (
	outingsNoCalendar outingsState = "no_calendar"
	outingsNoMember   outingsState = "no_member"
	outingsAmbiguous  outingsState = "ambiguous"
	outingsEmpty      outingsState = "empty"
	outingsList       outingsState = "list"
)

// outingsWindow is how far before a request its outings show unfolded.
const outingsWindow = 90 * 24 * time.Hour

// outingsBlock is the « Sorties VPDive » block of a request page (lot 8
// part 2): the requester's outings with their lines, never shown to members.
type outingsBlock struct {
	State    outingsState
	Received time.Time // when the calendar in place was pushed
	Since    time.Time // the request's submission less outingsWindow
	Recent   []outing  // ending since Since, upcoming ones included, newest first
	Older    []outing
}

// outingsBlock follows the requester's member (found by address, never by
// the typed name) to their participations, then attaches their lines.
func (s *Server) outingsBlock(ctx context.Context, submitted time.Time, profile members.Profile, found bool,
	pay payments.Block, mollie payments.MollieBlock) (outingsBlock, error) {
	info, received, err := imports.Last(ctx, s.db, imports.Calendar)
	if err != nil || !received {
		return outingsBlock{State: outingsNoCalendar}, err
	}
	b := outingsBlock{Received: info.ImportedAt, Since: submitted.Add(-outingsWindow)}
	if !found {
		b.State = outingsNoMember
		return b, nil
	}
	n, err := s.members.NameCount(ctx, profile.NameHash)
	if err != nil {
		return outingsBlock{}, err
	}
	if n > 1 {
		b.State = outingsAmbiguous
		return b, nil
	}
	ps, err := s.calendar.Participations(ctx, profile.NameHash)
	if err != nil {
		return outingsBlock{}, err
	}
	if len(ps) == 0 {
		b.State = outingsEmpty
		return b, nil
	}
	var collected []payments.CollectedLine
	for _, p := range mollie.Payments {
		collected = append(collected, p.Lines...)
	}
	b.State = outingsList
	for _, o := range attach(ps, pay.Lines, collected, s.paris) {
		if o.Event.Until().Before(b.Since) {
			b.Older = append(b.Older, o)
		} else {
			b.Recent = append(b.Recent, o)
		}
	}
	return b, nil
}
