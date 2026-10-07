package web

import (
	"fmt"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

var (
	frWeekdays      = [...]string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}
	frWeekdaysShort = [...]string{"dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."}
	frMonths        = [...]string{"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août",
		"septembre", "octobre", "novembre", "décembre"}
	frMonthsShort = [...]string{"janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août",
		"sept.", "oct.", "nov.", "déc."}
)

// frWeekdaysMonday heads the columns of the month grid.
var frWeekdaysMonday = []string{"lun.", "mar.", "mer.", "jeu.", "ven.", "sam.", "dim."}

// frClock writes « 8 h 30 », or « 8 h » on the hour.
func frClock(t time.Time) string {
	if t.Minute() == 0 {
		return strconv.Itoa(t.Hour()) + " h"
	}
	return fmt.Sprintf("%d h %02d", t.Hour(), t.Minute())
}

// frShortDay writes « sam. 10 oct. ».
func frShortDay(t time.Time) string {
	return fmt.Sprintf("%s %d %s", frWeekdaysShort[t.Weekday()], t.Day(), frMonthsShort[t.Month()-1])
}

// frLongDay writes « samedi 10 octobre 2026 ».
func frLongDay(t time.Time) string {
	return fmt.Sprintf("%s %d %s %d", frWeekdays[t.Weekday()], t.Day(), frMonths[t.Month()-1], t.Year())
}

// frMonth writes « octobre 2026 ».
func frMonth(t time.Time) string { return frMonths[t.Month()-1] + " " + strconv.Itoa(t.Year()) }

// frWeek writes the week starting on monday: « Semaine du 5 au 11 octobre
// 2026 », naming the first month or year only when it differs.
func frWeek(monday time.Time) string {
	sunday := monday.AddDate(0, 0, 6)
	from := strconv.Itoa(monday.Day())
	switch {
	case monday.Year() != sunday.Year():
		from += " " + frMonths[monday.Month()-1] + " " + strconv.Itoa(monday.Year())
	case monday.Month() != sunday.Month():
		from += " " + frMonths[monday.Month()-1]
	}
	return "Semaine du " + from + " au " + strconv.Itoa(sunday.Day()) + " " + frMonths[sunday.Month()-1] + " " + strconv.Itoa(sunday.Year())
}

// eventWhen writes when an event starts: « sam. 10/10/2026, 8 h 30 », or
// « …, journée ».
func eventWhen(ev calendar.Event, paris *time.Location) string {
	start := ev.Start.In(paris)
	day := frWeekdaysShort[start.Weekday()] + " " + start.Format(dateFormat)
	if ev.AllDay {
		return day + ", journée"
	}
	return day + ", " + frClock(start)
}

// midnight is the start of t's day in loc.
func midnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// eventHours writes the hours of an event in a day list: « 8 h 30 – 12 h »,
// « Journée », the start alone when the end precedes it, and « jusqu'au
// 14/10 » when it ends another day. An end at midnight closes the day
// before, as in Store.Range.
func eventHours(ev calendar.Event, paris *time.Location) string {
	start, until := ev.Start.In(paris), ev.Until().In(paris)
	last := until
	if until.After(start) && until.Equal(midnight(until, paris)) {
		last = until.Add(-time.Nanosecond)
	}
	sameDay := start.Format(time.DateOnly) == last.Format(time.DateOnly)
	s := frClock(start)
	switch {
	case ev.AllDay:
		s = "Journée"
	case sameDay && until.After(start):
		s += " – " + frClock(until)
	}
	if !sameDay {
		s += ", jusqu'au " + last.Format("02/01")
	}
	return s
}

// cartText writes a participant's cart, after « Panier : ».
func cartText(p *calendar.Payment) string {
	if p == nil {
		return "aucun"
	}
	due, paid := payments.Amount(p.DueCents).Euros(), payments.Amount(p.PaidCents).Euros()
	switch p.Status {
	case calendar.PaymentPaid:
		return "payé, " + paid
	case calendar.PaymentPartial:
		return "partiel, " + paid + " sur " + due
	case calendar.PaymentUnpaid:
		return "à payer, " + due
	}
	return string(p.Status) + ", " + paid + " sur " + due
}
