package web

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
)

func TestFrenchDates(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	at := func(m time.Month, d, h, mi int) time.Time { return time.Date(2026, m, d, h, mi, 0, 0, paris) }

	assert.Equal(t, "8 h", frClock(at(10, 10, 8, 0)))
	assert.Equal(t, "14 h 05", frClock(at(10, 10, 14, 5)))
	assert.Equal(t, "sam. 10 oct.", frShortDay(at(10, 10, 0, 0)))
	assert.Equal(t, "samedi 10 octobre 2026", frLongDay(at(10, 10, 0, 0)))
	assert.Equal(t, "octobre 2026", frMonth(at(10, 10, 0, 0)))
	assert.Equal(t, "Semaine du 5 au 11 octobre 2026", frWeek(at(10, 5, 0, 0)))
	assert.Equal(t, "Semaine du 28 septembre au 4 octobre 2026", frWeek(at(9, 28, 0, 0)))
	assert.Equal(t, "Semaine du 28 décembre 2026 au 3 janvier 2027", frWeek(at(12, 28, 0, 0)))

	ev := calendar.Event{Start: at(10, 10, 8, 30), End: at(10, 10, 12, 0)}
	assert.Equal(t, "sam. 10/10/2026, 8 h 30", eventWhen(ev, paris))
	assert.Equal(t, "8 h 30 – 12 h", eventHours(ev, paris))
	ev.End = at(10, 10, 7, 0)
	assert.Equal(t, "8 h 30", eventHours(ev, paris), "an end before the start: the start alone")
	ev.End = at(10, 14, 18, 0)
	assert.Equal(t, "8 h 30, jusqu'au 14/10", eventHours(ev, paris))
	allDay := calendar.Event{Start: at(10, 10, 0, 0), End: at(10, 10, 23, 59), AllDay: true}
	assert.Equal(t, "sam. 10/10/2026, journée", eventWhen(allDay, paris))
	assert.Equal(t, "Journée", eventHours(allDay, paris))
	allDay.End = at(10, 11, 0, 0)
	assert.Equal(t, "Journée", eventHours(allDay, paris), "an end at midnight closes the day before")
	late := calendar.Event{Start: at(10, 10, 21, 0), End: at(10, 11, 0, 0)}
	assert.Equal(t, "21 h – 0 h", eventHours(late, paris))

	assert.Equal(t, slices.Concat(frWeekdaysShort[1:], frWeekdaysShort[:1]), frWeekdaysMonday, "the month grid heads: monday first")

	assert.Equal(t, "aucun", cartText(nil))
	assert.Equal(t, "payé, 60,00\u00a0€", cartText(&calendar.Payment{Status: calendar.PaymentPaid, DueCents: 6000, PaidCents: 6000}))
	assert.Equal(t, "partiel, 40,00\u00a0€ sur 60,00\u00a0€", cartText(&calendar.Payment{Status: calendar.PaymentPartial, DueCents: 6000, PaidCents: 4000}))
	assert.Equal(t, "à payer, 60,00\u00a0€", cartText(&calendar.Payment{Status: calendar.PaymentUnpaid, DueCents: 6000}))
	assert.Equal(t, "refunded, 0,00\u00a0€ sur 60,00\u00a0€", cartText(&calendar.Payment{Status: "refunded", DueCents: 6000}), "unknown: as received")
}
