package web

import (
	"html"
	"testing"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/carnets"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"

	"github.com/stretchr/testify/assert"
)

// A visual summary must not bypass the existing identity gates, replace
// the detailed evidence, or reach the public tracking page.
func TestRequestVisualsKeepEvidenceAndIdentityGates(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	e.importCalendar(t, "calendar_view.json")
	e.importPayments(t)
	e.importMollie(t)
	e.importCarnets(t)
	e.importCampaign(t)
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	page := e.openTicket(t, cookie, hugo.ID).body
	for _, want := range []string{"viz-block", "Inscriptions au fil des mois", "Inscriptions passées", "Situation des paiements", "Avoir restant", "Participation, paiements et désinscriptions", "Sortie Porquerolles", signalCarnet} {
		assert.Contains(t, page, want)
	}
	lea := e.submitTicket(t, "lea.martin@example.org")
	ambiguous := e.openTicket(t, cookie, lea.ID).body
	assert.NotContains(t, ambiguous, "Inscriptions au fil des mois")
	assert.NotContains(t, ambiguous, "Situation des paiements")
	assert.NotContains(t, ambiguous, "Avoir restant")
	_, public := e.tracking(t, hugo.Token)
	assert.NotContains(t, public, "viz-block")
}

// Guests count as seats, not additional member registrations; cancelled,
// waiting, future and out-of-window events must not inflate the past chart.
func TestOutingVisualCountsKnownRegistrations(t *testing.T) {
	e := newTestEnv(t)
	at := func(date string) time.Time { v, _ := time.Parse(time.RFC3339, date); return v }
	registered := &calendar.Participant{Registered: true, People: 4}
	b := outingsBlock{State: outingsList, Received: e.clock.now()}
	cases := []struct {
		title, start string
		participant  *calendar.Participant
		left         bool
	}{
		{"Inscription A", "2026-08-31T22:30:00Z", registered, false},
		{"Inscription B", "2026-09-01T10:00:00Z", registered, false},
		{"Annulée", "2026-09-01T10:00:00Z", registered, false},
		{"Attente", "2026-09-01T10:00:00Z", &calendar.Participant{Registered: true, WaitingList: true}, false},
		{"Future", "2026-09-03T10:00:00Z", registered, false},
		{"Trop ancienne", "2025-09-30T10:00:00Z", registered, false},
		{"Désinscrite", "2026-09-01T10:00:00Z", nil, true},
	}
	for _, tc := range cases {
		o := outing{
			Event:       calendar.Event{ID: tc.title, Title: tc.title, Start: at(tc.start)},
			Participant: tc.participant}
		if tc.left {
			o.Unregistrations = []calendar.Unregistration{{}}
		}
		b.Recent = append(b.Recent, o)
	}
	b.Recent = append(b.Recent, b.Recent[0]) // two linked rows still describe one registration
	linked := b.Recent[0]
	linked.Participant = nil
	b.Recent = append([]outing{linked}, b.Recent...) // a linked payer row must not hide registration
	got := html.UnescapeString(string(e.srv.outingsVisual(b)))
	for label, value := range map[string]string{"Inscriptions passées": "2", "Inscriptions à venir connues": "1", "Sorties annulées connues": "1", "Sorties avec désinscription": "1"} {
		assert.Regexp(t, `<dt>`+label+`</dt><dd[^>]*>`+value+`</dd>`, got)
	}
	assert.Contains(t, got, `sept. 26 : 2`)
	assert.Contains(t, got, "pas des plongées réalisées")
}

func TestCardVisualDoesNotPresentPartialNetAsComplete(t *testing.T) {
	e := newTestEnv(t)
	d := ticketData{Carnets: carnets.Block{State: payments.BlockLines, Cards: []carnets.View{
		{Title: "Carte synthétique", Amount: -8000, Unread: 1, Debited: 1200, Debits: 2},
	}}}
	v := e.srv.ticketVisuals(d)
	assert.Len(t, v.Cards, 1)
	got := html.UnescapeString(string(v.Cards[0].Summary))
	assert.Contains(t, got, "Données partielles")
	assert.Contains(t, got, "80,00")
	assert.Contains(t, got, "Non calculé")
	assert.NotContains(t, got, "12,00")
}

func TestPaymentVisualKeepsKnownCreditsSeparate(t *testing.T) {
	e := newTestEnv(t)
	d := ticketData{Payments: payments.Block{State: payments.BlockLines, Balances: []payments.Line{
		{Product: "Carnet synthétique", UnitPrice: -8000},
		{Product: "Formation synthétique", UnitPrice: 0},
	}}}
	got := html.UnescapeString(string(e.srv.ticketVisuals(d).Payments))
	assert.Contains(t, got, "Carnet synthétique")
	assert.Contains(t, got, "80,00")
	assert.Contains(t, got, "Formation synthétique")
	assert.Contains(t, got, "0,00")
}
