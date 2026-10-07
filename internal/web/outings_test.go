package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

func outingOn(id, title string, start time.Time) calendar.Participation {
	return calendar.Participation{Event: calendar.Event{ID: id, Title: title, Start: start, End: start.Add(3 * time.Hour)}}
}

// Review focus: a title with a tab, double spaces or capitals still matches.
func TestAttachLinesToOutings(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	at := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, paris) }
	ps := []calendar.Participation{
		outingOn("matin", "Sortie du matin", at(6, 8)),
		outingOn("soir", "Sortie  du SOIR\t", at(6, 20)),
		outingOn("bapteme-a", "Baptêmes", at(13, 9)),
		outingOn("bapteme-b", "Baptêmes", at(13, 14)),
		outingOn("seule", "Sortie Levant", at(20, 9)),
	}
	lines := []payments.Line{
		{Product: "sortie du soir", Starts: at(6, 0)},
		{Product: "Autre intitulé", Starts: at(6, 0)},
		{Product: "Baptêmes", Starts: at(13, 0)},
		{Product: "SORTIE ANNULÉE - Levant", Starts: at(20, 0)},
		{Product: "Carte 10 plongées"},
		{Product: "Sortie du matin", Starts: at(7, 0)},
	}
	mollie := []payments.CollectedLine{{Service: "Sortie du matin", Starts: at(6, 0)}}

	got := attach(ps, lines, mollie, paris)
	products := make([][]string, 0, len(got))
	for _, o := range got {
		p := make([]string, 0, len(o.Lines))
		for _, l := range o.Lines {
			p = append(p, l.Product)
		}
		products = append(products, p)
	}
	assert.Equal(t, [][]string{{}, {"sortie du soir"}, {}, {}, {"SORTIE ANNULÉE - Levant"}}, products,
		"two outings that day: by title; same title twice or none: no outing; alone that day: whatever the title; undated or no outing that day: none")
	assert.Len(t, got[0].Mollie, 1, "a Mollie line by its outing's title")
	assert.Empty(t, got[1].Mollie)
}

func TestOutingSignals(t *testing.T) {
	cancelled := calendar.Participation{Event: calendar.Event{Title: "SORTIE ANNULÉE - Levant"}}
	held := calendar.Participation{Event: calendar.Event{Title: "Sortie Levant"}}
	prepaid := payments.Line{State: payments.StatePaid, Method: payments.MethodPrepaid, UnitPrice: 3000}
	money := payments.Line{State: payments.StatePaid, Method: payments.MethodVPayDive, Paid: 3500}
	free := payments.Line{State: payments.StatePaid, Method: "Autre"}
	void := payments.Line{State: payments.StateCancelled, Method: payments.MethodPrepaid, UnitPrice: 3000}
	unsettled := payments.CollectedLine{Settled: payments.SettledNo}
	checked := unsettled
	checked.Dismissal = &payments.Dismissal{}

	for name, c := range map[string]struct {
		o    outing
		want []string
	}{
		"carnet, once for two":      {outing{Participation: cancelled, Lines: []payments.Line{prepaid, prepaid}}, []string{signalCarnet}},
		"real money":                {outing{Participation: cancelled, Lines: []payments.Line{money}}, []string{signalMoney}},
		"both":                      {outing{Participation: cancelled, Lines: []payments.Line{money, prepaid}}, []string{signalCarnet, signalMoney}},
		"nothing paid":              {outing{Participation: cancelled, Lines: []payments.Line{free}}, nil},
		"outing held":               {outing{Participation: held, Lines: []payments.Line{prepaid}}, nil},
		"line cancelled":            {outing{Participation: cancelled, Lines: []payments.Line{void}}, nil},
		"Mollie not settled":        {outing{Participation: held, Mollie: []payments.CollectedLine{unsettled}}, []string{signalMollie}},
		"Mollie checked by someone": {outing{Participation: held, Mollie: []payments.CollectedLine{checked}}, nil},
	} {
		assert.Equal(t, c.want, signals(c.o), name)
	}
}
