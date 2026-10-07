package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spec §7.3 and §13: the « Paiements VPDive » block of a request page, in
// each of its cases, and never on the member's tracking page.
func TestRequestPagePaymentsBlock(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	lea := e.submitTicket(t, "lea.martin@example.org")
	noe := e.submitTicket(t, "noe.durand@example.org")

	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, "Aucun paiement importé.")

	e.importPayments(t)
	page := e.openTicket(t, cookie, hugo.ID).body
	for _, want := range []string{
		"Paiements VPDive",
		"Export du fichier créé le 01/09/2026 (indicatif), importé le 02/09/2026 par Alice",
		"Ces données datent de l'import : vérifie dans VPDive avant d'agir.",
		"Ligne créée le", "180,00\u00a0€", "05/01/2026", "60,00\u00a0€", "02/02/2026",
		"Formation RIFAP", "10,50\u00a0€",
		"le carnet sera recrédité à la suppression de la sortie",
		"payé en argent réel : remboursement par le trésorier",
		"remboursement probable",
		"Mollie (VPayDive)", "Paiement partiel : à vérifier.",
	} {
		assert.Contains(t, page, want)
	}

	assert.Contains(t, e.openTicket(t, cookie, lea.ID).body, "Plusieurs membres portent ce nom : aucune ligne n'est affichée.")
	e.importMembers(t, "members_minimal.xlsx")
	assert.Contains(t, e.openTicket(t, cookie, noe.ID).body,
		"Le demandeur n'est pas dans la liste des membres : ses paiements ne peuvent pas être rattachés.")

	_, tracking := e.tracking(t, hugo.Token)
	assert.NotContains(t, tracking, "Paiements VPDive")
	assert.NotContains(t, tracking, "180,00")
}

// Spec §7.5, §7.7 and §13: the « Encaissements Mollie » block of a request
// page, its payments newest first, and never on the member's tracking page.
func TestRequestPageMollieBlock(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	lea := e.submitTicket(t, "lea.martin@example.org")

	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, "Aucun encaissement Mollie importé.")

	e.importMollie(t)
	page := e.openTicket(t, cookie, hugo.ID).body
	for _, want := range []string{
		"Encaissements Mollie",
		"Export du fichier créé le 01/09/2026 (indicatif), importé le 02/09/2026 par Alice",
		"Ce que Mollie a encaissé, vu par VPayDive, et si VPDive l'a soldé.",
		"12/08/2026 à 14:05", "Carte de crédit", "total 53,00 €", "Sortie Porquerolles",
		"Soldé dans VPDive.", "remboursement ou remise", "Pay by Bank",
		"Encaissé par Mollie, non soldé dans VPDive : à vérifier.",
		"Une tentative échouée ou expirée n'apparaît pas ici : elle se voit dans VPDive.",
		"/f/mollie/index",
	} {
		assert.Contains(t, page, want)
	}
	assert.Less(t, strings.Index(page, "12/08/2026 à 14:05"), strings.Index(page, "03/07/2026 à 18:30"), "newest first")
	assert.Contains(t, e.openTicket(t, cookie, lea.ID).body, "Plusieurs membres portent ce nom : aucun encaissement n'est affiché.")

	checks := e.do(t, http.MethodGet, adminHost, "/anomalies", nil, withCookie(cookie)).Body.String()
	csrf := csrfPattern.FindStringSubmatch(checks)[1]
	for _, m := range fingerprintPattern.FindAllStringSubmatch(checks, -1) {
		v := url.Values{"csrf": {csrf}, "empreinte": {m[1]}}
		e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(v), formType, withCookie(cookie))
	}
	page = e.openTicket(t, cookie, hugo.ID).body
	assert.NotContains(t, page, "Encaissé par Mollie, non soldé dans VPDive : à vérifier.")
	assert.Contains(t, page, "Vérifié par Alice (Présidente) le 02/09/2026.")

	_, tracking := e.tracking(t, hugo.Token)
	assert.NotContains(t, tracking, "Encaissements Mollie")
	assert.NotContains(t, tracking, "53,00")
}

// An export cell holding a date and no time reads as midnight: it shows as
// a date, never « à 00:00 ».
func TestMidnightShowsAsADate(t *testing.T) {
	e := newTestEnv(t)
	assert.Equal(t, "12/08/2026", e.srv.formatTime(time.Date(2026, 8, 12, 0, 0, 0, 0, e.srv.paris)))
	assert.Equal(t, "12/08/2026 à 00:01", e.srv.formatTime(time.Date(2026, 8, 12, 0, 1, 0, 0, e.srv.paris)))
}

// An empty « Payé » cell is named as such, not quoted as an empty value.
func TestRequestPageNamesAnEmptySettledCell(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	wb := xlsxtest.Build(t, xlsxtest.Sheet{
		{"Nom", "Prénom", "Type Panier", "Montant Panier", "Payé", "Date paiement"},
		{"Bernard", "Hugo", "Calendrier", 40, "", "12/08/2026 14:05"},
	})
	require.Equal(t, http.StatusOK, e.push(t, "vpaydive", wb, importToken).Code)

	page := e.openTicket(t, cookie, hugo.ID).body
	assert.Contains(t, page, "« Payé » est vide dans l'export.")
	assert.NotContains(t, page, "« Payé » vaut")
}
