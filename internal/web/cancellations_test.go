package web

import (
	"context"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/payments/paymentstest"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

func TestCancellationsPage(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	page := func() string {
		t.Helper()
		rec := e.do(t, http.MethodGet, adminHost, "/annulations", nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		return html.UnescapeString(rec.Body.String())
	}
	body := page()
	assert.Contains(t, body, `href="/annulations"`, "in the committee nav")
	assert.Contains(t, body, "Aucun paiement importé.")

	e.importPayments(t)
	body = page()
	for _, want := range []string{
		"2 sorties, 5 lignes, 4 inscriptions.",
		"Ces données datent de l'import : vérifie dans VPDive avant d'agir.",
		"90,00\u00a0€", "35,00\u00a0€",
		`<span class="text-error font-bold">116 j</span>`,
	} {
		assert.Contains(t, body, want)
	}
	assert.Less(t, strings.Index(body, "SORTIE ANNULÉE - Île du Levant"), strings.Index(body, "Plongée de nuit (annulée, météo)"),
		"newest first")
	for _, name := range []string{"Bernard", "Durand", "Martin", "MARTIN"} {
		assert.NotContains(t, body, name, "no names on this page")
	}

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/annulations", nil).Code)
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/annulations", nil).Code, "session required")
}

// An outing cancelled ahead of time is « à venir », not « < 1 h ».
func TestCancellationsPageFutureOuting(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	sheet := xlsxtest.Sheet{
		{"Nom", "Prénom", "Prix unitaire", "Quantité", "Montant paiement", "Montant réduc.", "État",
			"Methode de paiement", "Produit/Événement", "Du", "Créé le"},
		{"Bernard", "Hugo", 30, 1, 0, 0, "Payé", "Prépayé", "Sortie annulée (météo)", "20/09/2026 09:00", "01/09/2026 10:00:00"},
	}
	paymentstest.ImportBytes(t, e.deps.Payments, xlsxtest.Build(t, sheet))

	rec := e.do(t, http.MethodGet, adminHost, "/annulations", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, "à venir")
	assert.NotContains(t, body, "< 1 h")
}

// Spec §8.3: after 90 days without an import the lines are gone; both pages
// say so instead of « Aucun paiement importé ».
func TestPagesAfterThePaymentsPurge(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	e.importPayments(t)
	e.clock.advance(91 * 24 * time.Hour)
	require.NoError(t, e.deps.Payments.Purge(context.Background()))
	cookie := e.login(t) // sessions last 30 days

	want := "Plus de 90 jours sans import : les lignes de paiement ont été effacées."
	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, want)
	rec := e.do(t, http.MethodGet, adminHost, "/annulations", nil, withCookie(cookie))
	assert.Contains(t, html.UnescapeString(rec.Body.String()), want)
}

// Codex review: an export with no row is a recent import, not a purge.
func TestEmptyPaymentsImportIsNotAPurge(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	paymentstest.ImportBytes(t, e.deps.Payments, paymentsSheet(t, 0))

	ticket := e.openTicket(t, cookie, hugo.ID).body
	assert.Contains(t, ticket, "Aucune ligne de paiement en place pour ce membre.", "true after an erasure too")
	for _, path := range []string{"/annulations", "/imports"} {
		rec := e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, html.UnescapeString(rec.Body.String()), "Plus de 90 jours", path)
	}
	assert.NotContains(t, ticket, "Plus de 90 jours")
	rec := e.do(t, http.MethodGet, adminHost, "/annulations", nil, withCookie(cookie))
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Aucune sortie annulée en attente de suppression.")
}

// Lot 8 part 2: a cancelled outing points at its calendar page when title
// and day find one event, else at the day view.
func TestCancellationsLinkToTheCalendar(t *testing.T) {
	e := newTestEnv(t)
	e.importPayments(t)
	cookie := e.login(t)

	_, before := e.page(t, cookie, "/annulations")
	assert.Contains(t, before, `href="/calendrier?vue=jour&date=2026-05-10"`, "no calendar yet: the day view")

	e.importCalendar(t, "calendar_view.json")
	_, page := e.page(t, cookie, "/annulations")
	assert.Contains(t, page, `href="/calendrier/evt-levant"`, "same title and day")
	assert.Regexp(t, `class="[^"]*\bmin-h-11\b[^"]*" href="/calendrier/evt-levant"`, page, "a 44 px touch target")
	assert.Contains(t, page, `href="/calendrier?vue=jour&date=2026-05-08"`, "renamed in the calendar: the day view")
}
