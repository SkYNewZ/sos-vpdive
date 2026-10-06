package web

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
