package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// section returns the part of page between the headings of id and of next
// ("" for the end of the page).
func section(t *testing.T, page, id, next string) string {
	t.Helper()
	_, rest, found := strings.Cut(page, `id="`+id+`"`)
	require.True(t, found, id)
	if next != "" {
		rest, _, found = strings.Cut(rest, `id="`+next+`"`)
		require.True(t, found, next)
	}
	return rest
}

// Design 2026-10-20 « Page Suivi des adhésions »: the whole committee sees
// the campaign in three sections sorted by name, then a count of the others.
func TestMembershipsPage(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	code, page := e.page(t, cookie, "/adhesions")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, `href="/adhesions"`, "in the committee nav")
	assert.Contains(t, page, `<use href="#i-id-card">`)
	assert.Contains(t, page, `<symbol id="i-id-card"`)
	assert.Contains(t, page, "Aucune campagne d'adhésion importée.")
	assert.Contains(t, page, `href="/imports#campagne-titre"`)

	e.importCampaign(t)
	_, page = e.page(t, cookie, "/adhesions")
	assert.Contains(t, page, "Export du fichier créé le 21/10/2026 (indicatif)")
	assert.Contains(t, page, "Campagne d'adhésion</a>", "the VPDive link")
	assert.Contains(t, page, "/f/campaign/users/OO2mtT1O0mAa_yIwb9uHGKUjEFuDTUbL_mVKy_l3KUk")

	pending := section(t, page, "a-valider", "valides")
	assert.Contains(t, pending, "Bernard Hugo")
	assert.Contains(t, pending, "vpaydive (12/10/2026)")
	assert.Contains(t, pending, "Inconnu Paul")
	assert.Less(t, strings.Index(pending, "Bernard Hugo"), strings.Index(pending, "Inconnu Paul"), "sorted by name")
	assert.Equal(t, 2, strings.Count(pending, "Homonyme"), "Inconnu Paul is twice in the export: table and card")
	assert.NotContains(t, pending, "Petit")

	validated := section(t, page, "valides", "incomplets")
	for _, want := range []string{
		"Martin Léa", "Petit Chloé", "Roux Max",
		"Validé le 14/10/2026 à 18:32 par Hugo Bernard",
		"Virements (03/10/2026)",
		"état inconnu : Remboursé",
		"Validé, validation illisible",
		"Adhésion réglée en espèces", "à vérifier",
		"1\u00a0042,50\u00a0€",
		"Adhésion ou licence non réglée",
		"montant ≠ 50 €",
	} {
		assert.Contains(t, validated, want)
	}
	assert.Less(t, strings.Index(validated, "Martin Léa"), strings.Index(validated, "Petit Chloé"))
	assert.Less(t, strings.Index(validated, "Petit Chloé"), strings.Index(validated, "Roux Max"))
	_, chloe, _ := strings.Cut(validated, "Petit Chloé")
	chloe, _, _ = strings.Cut(chloe, "Roux Max")
	assert.NotContains(t, chloe, "non réglée", "both paid")
	assert.NotContains(t, chloe, "montant ≠", "50 €")

	incomplete := section(t, page, "incomplets", "")
	assert.Contains(t, incomplete, "Durand Noé")
	assert.Contains(t, incomplete, "Au panier")
	assert.Contains(t, incomplete, "Il manque la licence.")
	assert.Contains(t, incomplete, "4 personnes invitées sans adhésion ni licence.")
	for _, absent := range []string{"Leroy", "MARTIN Lea", "invite@example.org"} {
		assert.NotContains(t, page, absent, "the others are only counted")
	}

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/adhesions", nil).Code)
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/adhesions", nil).Code, "session required")
	_, plus := e.page(t, cookie, "/plus")
	assert.Contains(t, plus, `href="/adhesions"`, "in « Plus »")
	sidebar, dock, _ := strings.Cut(page, `aria-label="Onglets du comité"`)
	assert.Contains(t, sidebar, `href="/adhesions"`)
	assert.NotContains(t, dock, `href="/adhesions"`, "not a phone tab")
}

// Spec §8.3: after 90 days without an import the people are gone and the
// page says so.
func TestMembershipsPageAfterThePurge(t *testing.T) {
	e := newTestEnv(t)
	e.importCampaign(t)
	e.clock.advance(91 * 24 * time.Hour)
	require.NoError(t, e.deps.Campaign.Purge(context.Background()))
	cookie := e.login(t)
	_, page := e.page(t, cookie, "/adhesions")
	assert.Contains(t, page, "Plus de 90 jours sans import : la campagne d'adhésion a été effacée.")
	assert.NotContains(t, page, "Aucune campagne d'adhésion importée.")
}
