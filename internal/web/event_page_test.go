package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlainText(t *testing.T) {
	assert.Equal(t, "Rendez-vous à 7\u00a0h.\nPrévois ta lampe.",
		plainText("<p>Rendez-vous à 7&nbsp;h.<br>Prévois ta lampe.</p><p></p><script>x()</script>"))
	assert.Equal(t, "A\n\nB", plainText("<p>A</p><p></p><p>B</p>"), "empty paragraphs give one blank line")
	assert.Empty(t, plainText(""))
}

func TestEventPage(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	e.importCalendar(t, "calendar_view.json")
	cookie := e.login(t)

	code, page := e.page(t, cookie, "/calendrier/evt-cap")
	require.Equal(t, http.StatusOK, code)
	for _, want := range []string{
		"Sortie Cap Garonne", "dimanche 6 septembre 2026, 8 h – 12 h",
		`href="https://club.example/agenda/evt-cap"`, "Ouvrir dans VPDive",
		`href="/calendrier?vue=semaine&date=2026-09-06"`,
		"DP : MARTIN Léa", "Pilote : BERNARD Hugo (Bateau A, proposé), GARNIER Paul (Bateau B)",
		"1 inscrit sur 12", "Rendez-vous au port à 7\u00a0h\u00a030.",
		"MARTIN Léa · Homonyme, non rapproché",
		"BERNARD Hugo · Membre", "GARNIER Paul · Non rapproché",
	} {
		assert.Contains(t, page, want)
	}
	assert.NotContains(t, page, "x()", "markup and scripts of the description are dropped")

	_, porquerolles := e.page(t, cookie, "/calendrier/evt-porquerolles")
	assert.Contains(t, porquerolles, "PETIT Chloé · Membre")
	assert.Contains(t, porquerolles, "invité")
	assert.Contains(t, porquerolles, "Famille Roux · Non rapproché", "null roles and qualifications render")
	assert.Contains(t, porquerolles, "Panier : payé, 70,00\u00a0€")

	_, inverted := e.page(t, cookie, "/calendrier/evt-inverse")
	assert.Contains(t, inverted, "Heure de fin incohérente dans VPDive.")
	_, cancelled := e.page(t, cookie, "/calendrier/evt-levant")
	assert.Contains(t, cancelled, `href="/annulations"`)
	_, meeting := e.page(t, cookie, "/calendrier/evt-ag")
	assert.Contains(t, meeting, "0 inscrit", "null boats and limit render")

	code, missing := e.page(t, cookie, "/calendrier/evt-nope")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, missing, "La sortie a été supprimée dans VPDive, ou elle a commencé il y a plus de 12 mois.")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/calendrier/evt-cap", nil).Code)
}
