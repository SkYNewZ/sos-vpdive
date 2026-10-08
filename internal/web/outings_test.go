package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

func outingOn(id, title string, start time.Time) calendar.Participation {
	return calendar.Participation{Event: calendar.Event{ID: id, Title: title, Start: start, End: start.Add(3 * time.Hour)}}
}

// A title with a tab, double spaces or capitals still matches. A VPDive line
// naming another outing stays out even on a day the requester has one outing:
// they may have left that other outing.
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
		outingOn("double", "Sortie Porquerolles", at(27, 9)),
		outingOn("double", "Sortie Porquerolles", at(27, 9)),
	}
	lines := []payments.Line{
		{Product: "sortie du soir", Starts: at(6, 0)},
		{Product: "Autre intitulé", Starts: at(6, 0)},
		{Product: "Baptêmes", Starts: at(13, 0)},
		{Product: "SORTIE ANNULÉE - Levant", Starts: at(20, 0)},
		{Product: "Carte 10 plongées"},
		{Product: "Sortie du matin", Starts: at(7, 0)},
		{Product: "Sortie Porquerolles", Starts: at(27, 0)},
	}
	mollie := []payments.CollectedLine{
		{Service: "Sortie du matin", Starts: at(6, 0)},
		{Service: "Supplément distance", Starts: at(20, 0)},
		{Service: "Supplément distance", Starts: at(27, 0)},
	}

	got := attach(ps, lines, mollie, paris)
	products := make([][]string, 0, len(got))
	for _, o := range got {
		p := make([]string, 0, len(o.Lines))
		for _, l := range o.Lines {
			p = append(p, l.Product)
		}
		products = append(products, p)
	}
	assert.Equal(t, [][]string{{}, {"sortie du soir"}, {}, {}, {}, {"Sortie Porquerolles"}, {}}, products,
		"by title only: same title twice or none, another outing's title on a day alone, undated or no outing that day: none; one outing with two seats: its first")
	mollieCount := make([]int, 0, len(got))
	for _, o := range got {
		mollieCount = append(mollieCount, len(o.Mollie))
	}
	assert.Equal(t, []int{1, 0, 0, 0, 1, 1, 0}, mollieCount, "a Mollie line by its outing's title, or to the only outing that day whatever the title")
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
		assert.Equal(t, c.want, c.o.Signals(), name)
	}
}

// Lot 8 part 2: the « Sorties VPDive » block of a request page, in each of
// its cases, and never on the member's tracking page.
func TestRequestPageOutingsBlock(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	lea := e.submitTicket(t, "lea.martin@example.org")
	ines := e.submitTicket(t, "ines.leroy@example.org")
	noe := e.submitTicket(t, "noe.durand@example.org")

	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, "Aucun calendrier reçu pour l'instant.")

	e.importCalendar(t, "calendar_view.json")
	e.importPayments(t)
	e.importMollie(t)
	page := e.openTicket(t, cookie, hugo.ID).body
	for _, want := range []string{
		"Sorties VPDive",
		"Calendrier reçu le 02/09/2026 à 12:00 : vérifie dans VPDive avant d'agir.",
		"sam. 15/08/2026, 8 h 30", `href="/calendrier/evt-porquerolles"`, "Sortie Porquerolles",
		"Inscrit · Pilote (Bateau A)", "Panier : partiel, 40,00\u00a0€ sur 60,00\u00a0€",
		"Mollie : Calendrier, 40,00\u00a0€, soldé dans VPDive",
		"Non inscrit · Pilote (Bateau A, proposé)", "Panier : aucun",
		"Liste d'attente, 2 personnes",
		"Voir les 2 sorties plus anciennes",
		"VPDive : SORTIE ANNULÉE - Île du Levant, Payé, Prépayé, 30,00\u00a0€",
		signalCarnet,
	} {
		assert.Contains(t, page, want)
	}
	assert.NotContains(t, page, signalMoney, "the night dive was renamed: its line names no outing of Hugo's and stays out")
	assert.Less(t, strings.Index(page, "Séjour Corse"), strings.Index(page, "Sortie Porquerolles"), "newest first")

	assert.Contains(t, e.openTicket(t, cookie, lea.ID).body, "Plusieurs membres portent ce nom : aucune sortie n'est affichée.")
	assert.Contains(t, e.openTicket(t, cookie, ines.ID).body, "Aucune sortie dans le calendrier pour ce membre.")
	e.importMembers(t, "members_minimal.xlsx")
	assert.Contains(t, e.openTicket(t, cookie, noe.ID).body, "Le demandeur n'est pas dans la liste des membres : aucune sortie ne peut lui être rattachée.")

	_, tracking := e.tracking(t, hugo.Token)
	assert.NotContains(t, tracking, "Sorties VPDive")
}

// An outing's Mollie line words its « Payé » cell as the Mollie block does:
// an empty one is named as such, never quoted as an empty value, and a
// dismissed unsettled one says who checked it.
func TestOutingsBlockWordsMollieLikeItsBlock(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	e.importCalendar(t, "calendar_view.json")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	data := xlsxtest.BuildCreated(t, time.Time{}, xlsxtest.Sheet{
		{"Nom", "Prénom", "Type Panier", "Prestation", "Date Début", "Montant Panier", "Payé", "Date paiement"},
		{"Bernard", "Hugo", "Calendrier", "Sortie Porquerolles", "15/08/2026", 40, "", "12/08/2026 14:05"},
		{"Bernard", "Hugo", "Calendrier", "Sortie Porquerolles", "15/08/2026", 40, "Non", "12/08/2026 14:06"},
	})
	require.Equal(t, http.StatusOK, e.push(t, "vpaydive", data, importToken).Code)

	page := e.openTicket(t, cookie, hugo.ID).body
	assert.Equal(t, 2, strings.Count(page, "« Payé » est vide dans l'export"), "the Mollie block and the outing")
	assert.NotContains(t, page, "« Payé » vaut")
	assert.Equal(t, 2, strings.Count(page, signalMollie), "the Mollie block and the outing")

	checks := e.do(t, http.MethodGet, adminHost, "/anomalies", nil, withCookie(cookie)).Body.String()
	csrf := csrfPattern.FindStringSubmatch(checks)[1]
	for _, m := range fingerprintPattern.FindAllStringSubmatch(checks, -1) {
		v := url.Values{"csrf": {csrf}, "empreinte": {m[1]}}
		e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(v), formType, withCookie(cookie))
	}
	page = e.openTicket(t, cookie, hugo.ID).body
	assert.NotContains(t, page, signalMollie)
	assert.Equal(t, 1, strings.Count(page, "Vérifié par Alice (Présidente) le 02/09/2026."), "the Mollie block")
	assert.Equal(t, 1, strings.Count(page, ", vérifié par Alice (Présidente) le 02/09/2026,"), "the outing, mid-line")
}

// Lot 8 part 3: an outing the requester left shows when and by whom, takes
// its « Annulé » line by title, and has no cart; one they came back to shows
// « Inscrit » first. Times read in Paris, authors escaped.
func TestRequestPageShowsUnregistrations(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	body := []byte(`{"from": "2026-06-04", "to": "2027-09-02", "events": [
		{"id": "evt-garonne", "title": "Sortie Cap Garonne",
		 "starts_at": "2026-06-07T09:00:00+02:00", "ends_at": "2026-06-07T12:00:00+02:00",
		 "participants": [{"vpdive_id": 101, "name": "MARTIN Léa", "last_name": "Martin", "first_name": "Léa", "registered": true, "people": 1}],
		 "unregistrations": [{"last_name": "BERNARD", "first_name": "Hugo", "at": "2026-06-05T16:42:00Z", "by": "Alice ORGANISATRICE"}]},
		{"id": "evt-corse", "title": "Séjour Corse",
		 "starts_at": "2026-09-20T08:00:00+02:00", "ends_at": "2026-09-27T18:00:00+02:00",
		 "participants": [{"vpdive_id": 102, "name": "BERNARD Hugo", "last_name": "Bernard", "first_name": "Hugo", "registered": true, "people": 1}],
		 "unregistrations": [
			{"last_name": "Bernard", "first_name": "Hugo", "at": "2026-08-02T09:30:00+02:00", "by": "Paul <b>GARNIER</b>"},
			{"last_name": "Bernard", "first_name": "Hugo", "at": "2026-08-01T10:00:00+02:00", "by": ""}]}]}`)
	exp, err := calendar.Parse(body, e.srv.paris, e.clock.now())
	require.NoError(t, err)
	require.NoError(t, e.deps.Calendar.Import(context.Background(), exp))
	e.importPayments(t) // Hugo's « Sortie Cap Garonne » line, « Annulé », is dated 07/06/2026 09:00 (serial 46180.375)

	page := e.openTicket(t, cookie, hugo.ID).body
	start, end := strings.Index(page, `id="sorties"`), strings.Index(page, `id="paiements"`)
	require.True(t, start >= 0 && end > start, "both blocks on the page")
	block := page[start:end]
	at := strings.Index(block, `href="/calendrier/evt-garonne"`)
	require.Positive(t, at, "the left outing is listed, after the newer one")
	corse, garonne := block[:at], block[at:]

	assert.Contains(t, garonne, "Désinscrit(e) le 05/06/2026 à 18:42 par Alice ORGANISATRICE")
	assert.Contains(t, garonne, "VPDive : Sortie Cap Garonne, Annulé", "the « Annulé » line attaches by title")
	assert.NotContains(t, garonne, "Non inscrit")
	assert.NotContains(t, garonne, "Panier :", "no participant row, no cart")

	registered := strings.Index(corse, "Inscrit</p>")
	first := strings.Index(corse, "Désinscrit(e) le 01/08/2026 à 10:00</p>")
	second := strings.Index(corse, "Désinscrit(e) le 02/08/2026 à 09:30 par Paul <b>GARNIER</b></p>")
	require.True(t, registered >= 0 && first >= 0 && second >= 0, "status and both unregistrations shown")
	assert.Less(t, registered, first, "the « Inscrit » line first")
	assert.Less(t, first, second, "oldest unregistration first")
	raw := e.do(t, http.MethodGet, adminHost, "/demandes/"+strconv.FormatInt(hugo.ID, 10), nil, withCookie(cookie)).Body.String()
	assert.Contains(t, raw, "par Paul &lt;b&gt;GARNIER&lt;/b&gt;</p>", "the author is escaped")
}
