package web

import (
	"net/http"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
)

// calendarView is a layout of /calendrier, as its query names it.
type calendarView string

const (
	viewMonth calendarView = "mois"
	viewWeek  calendarView = "semaine"
	viewDay   calendarView = "jour"
)

// eventView is an event in a list, with its counts and its staff when its
// participants were read.
type eventView struct {
	calendar.Event

	Booked, Queued int // people registered, people on the waiting list
	Staff          []roleGroup
	Counted        bool // participants were read: Booked, Queued and Staff hold
	Detailed       bool // the day view: activity, place, boats and staff
}

// Max is the event's limit, 0 without one.
func (v eventView) Max() int {
	if v.MaxParticipants == nil {
		return 0
	}
	return *v.MaxParticipants
}

func (s *Server) eventView(ev calendar.Event, counted bool) eventView {
	v := eventView{Event: ev, Counted: counted, Staff: s.labels.staff(ev.Participants)}
	for _, p := range ev.Participants {
		switch {
		case !p.Registered:
		case p.WaitingList:
			v.Queued += p.People
		default:
			v.Booked += p.People
		}
	}
	return v
}

// calendarSpan lays out a view: the days it shows, the events it reads
// between from and to, and the days its arrows go to.
type calendarSpan struct {
	days       []time.Time
	from, to   time.Time
	prev, next time.Time
}

// spanOf lays out view around day, a Paris midnight. The month grid runs
// from the Monday before its first day to the Sunday after its last.
func spanOf(view calendarView, day time.Time) calendarSpan {
	switch view {
	case viewDay:
		return calendarSpan{days: []time.Time{day}, from: day, to: day.AddDate(0, 0, 1),
			prev: day.AddDate(0, 0, -1), next: day.AddDate(0, 0, 1)}
	case viewMonth:
		first := day.AddDate(0, 0, 1-day.Day())
		end := first.AddDate(0, 1, 0)
		sp := calendarSpan{from: first, to: end, prev: first.AddDate(0, -1, 0), next: end}
		for d := monday(first); d.Before(end) || d.Weekday() != time.Monday; d = d.AddDate(0, 0, 1) {
			sp.days = append(sp.days, d)
		}
		return sp
	case viewWeek: // below, as any other value
	}
	mon := monday(day)
	sp := calendarSpan{from: mon, to: mon.AddDate(0, 0, 7), prev: mon.AddDate(0, 0, -7), next: mon.AddDate(0, 0, 7)}
	for i := range 7 {
		sp.days = append(sp.days, mon.AddDate(0, 0, i))
	}
	return sp
}

// monday is the Monday of d's week.
func monday(d time.Time) time.Time { return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7)) }

// onDay returns the events of evs that overlap day, as Store.Range does.
func onDay(evs []eventView, day time.Time) []eventView {
	end := day.AddDate(0, 0, 1)
	var out []eventView
	for _, ev := range evs {
		if ev.Start.Before(end) && (!ev.Start.Before(day) || ev.Until().After(day)) {
			out = append(out, ev)
		}
	}
	return out
}

// calendarLink is the address of view around day.
func calendarLink(view calendarView, day time.Time) string {
	return "/calendrier?vue=" + string(view) + "&date=" + day.Format(time.DateOnly)
}

// dayView is a day of a view.
type dayView struct {
	Date   time.Time
	Link   string // its day view
	InSpan bool   // false for the grid's days of another month
	Today  bool
	Events []eventView
	Shown  []eventView // the first three, for a month cell
	More   int         // the others
}

// viewLink is a button of the view switch.
type viewLink struct {
	Label, Link string
	Current     bool
}

// calendarData is a page of /calendrier.
type calendarData struct {
	View              calendarView
	Title             string
	Prev, Next, Today string
	Views             []viewLink
	Weekdays          []string // the month grid's column heads
	Days              []dayView
	Empty             bool
}

// calendarPage shows the month, week or day around ?date=: the current
// week without parameters, and on any unreadable view or date, never a mix
// of a valid one and the default of the other.
func (s *Server) calendarPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	today := midnight(s.now(), s.paris)
	view, day := viewWeek, today
	var err error
	if v := q.Get("vue"); v != "" {
		view = calendarView(v)
	}
	if v := q.Get("date"); v != "" {
		day, err = time.ParseInLocation(time.DateOnly, v, s.paris)
	}
	if err != nil || (view != viewMonth && view != viewWeek && view != viewDay) {
		view, day = viewWeek, today
	}
	sp := spanOf(view, day)
	counted := view != viewMonth // the month decrypts no participant
	events, err := s.calendar.Range(r.Context(), sp.from, sp.to, counted)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	evs := make([]eventView, len(events))
	for i, ev := range events {
		evs[i] = s.eventView(ev, counted)
		evs[i].Detailed = view == viewDay
	}
	d := calendarData{
		View: view, Prev: calendarLink(view, sp.prev), Next: calendarLink(view, sp.next), Today: calendarLink(view, today),
		Weekdays: frWeekdaysMonday, Empty: len(evs) == 0,
	}
	switch view {
	case viewMonth:
		d.Title = frMonth(sp.from)
	case viewDay:
		d.Title = frLongDay(sp.from)
	case viewWeek:
		d.Title = frWeek(sp.from)
	}
	for _, v := range []struct {
		view  calendarView
		label string
	}{{viewMonth, "Mois"}, {viewWeek, "Semaine"}, {viewDay, "Jour"}} {
		d.Views = append(d.Views, viewLink{Label: v.label, Link: calendarLink(v.view, day), Current: v.view == view})
	}
	for _, dd := range sp.days {
		dv := dayView{Date: dd, Link: calendarLink(viewDay, dd), Today: dd.Equal(today), InSpan: !dd.Before(sp.from) && dd.Before(sp.to)}
		if dv.InSpan {
			dv.Events = onDay(evs, dd)
		}
		dv.Shown = dv.Events[:min(3, len(dv.Events))]
		dv.More = len(dv.Events) - len(dv.Shown)
		d.Days = append(d.Days, dv)
	}
	p, err := s.adminPage(r, "Calendrier")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, http.StatusOK, "calendrier", p)
}
