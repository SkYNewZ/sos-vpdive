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

	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
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
	assert.Less(t, strings.Index(body, "Plongée de nuit (annulée, météo)"), strings.Index(body, "SORTIE ANNULÉE - Île du Levant"),
		"oldest first")
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
	rows, err := xlsx.ReadFirstSheet(xlsxtest.Build(t, sheet), payments.ImportLimits())
	require.NoError(t, err)
	exp, err := payments.Parse(rows, time.Time{}, time.UTC)
	require.NoError(t, err)
	ctx := context.Background()
	p, err := e.deps.Payments.NewPreview(ctx, "alice", exp)
	require.NoError(t, err)
	require.NoError(t, e.deps.Payments.Confirm(ctx, p.ID, "alice", true))

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
