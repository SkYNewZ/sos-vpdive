package web

import (
	"context"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitteeSeesSummaryProcedureAndFiches(t *testing.T) {
	e, m := modelEnv(t, 200)
	m.set(`{"fiches": ["inscription-impossible"], "resume": "N'arrive pas à s'inscrire à la sortie de samedi."}`)
	_, token := e.screen2(t, validRequest(e.formKey(t)))
	require.Equal(t, http.StatusSeeOther, e.draftAction(t, "/demandes/confirmer", token).Code)
	m.set(choice)
	_, other := e.screen2(t, validRequest(e.formKey(t)))
	require.Equal(t, http.StatusSeeOther, e.draftAction(t, "/demandes/abandonner", other).Code)

	cookie := e.login(t)
	board := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, board, "N'arrive pas à s'inscrire à la sortie de samedi.")
	assert.Contains(t, board, "Résumé automatique, à vérifier")
	assert.NotContains(t, board, "Je ne vois plus mes réservations", "the summary replaces the excerpt")

	page := e.openTicket(t, cookie, e.firstID(t)).body
	assert.Contains(t, page, "Résumé automatique, à vérifier")
	assert.Contains(t, page, "Procédure suggérée")
	assert.Contains(t, page, "Je n'arrive pas à m'inscrire à une sortie")
	assert.Contains(t, page, "vérifier la date du certificat médical", "the resolver procedure")
	assert.Contains(t, page, `href="/fiches"`)
	membres := strings.Index(page, "Liste des membres, et export des membres")
	paiements := strings.Index(page, "Paiements, et export des paiements")
	assert.True(t, membres >= 0 && membres < paiements, "the fiche's link comes first (spec §7)")

	fiches := e.do(t, http.MethodGet, adminHost, "/fiches", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, fiches.Code)
	body := html.UnescapeString(fiches.Body.String())
	for _, f := range e.deps.KB.Fiches {
		assert.Contains(t, body, f.Title)
		assert.Contains(t, body, `id="`+f.ID+`"`)
	}
	_, carnet, found := strings.Cut(body, `id="carnet-solde-negatif"`)
	require.True(t, found)
	carnet, _, _ = strings.Cut(carnet, "</article>")
	assert.Contains(t, carnet, "Demandes évitées où elle était proposée : 1")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/fiches", nil).Code, "committee only")
}

// firstID returns the id of the first request filed, CPP-0001.
func (e *testEnv) firstID(t *testing.T) int64 {
	t.Helper()
	var id int64
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT id FROM tickets WHERE ref = 'CPP-0001'`).Scan(&id))
	return id
}

func TestTicketPageWithoutFiche(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	require.Equal(t, http.StatusSeeOther, e.sendRequest(t, validRequest(e.formKey(t))).Code)
	cookie := e.login(t)
	page := e.openTicket(t, cookie, e.firstID(t)).body
	assert.Contains(t, page, "Aucune fiche proposée à l'envoi.")
	assert.NotContains(t, page, "Résumé automatique", "no model, no summary")
	board := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, board, "Je ne vois plus mes réservations", "without a summary the board shows the excerpt")
	assert.NotContains(t, board, "Résumé automatique")
}

func TestRemovedFicheIsNamed(t *testing.T) {
	e, m := modelEnv(t, 200)
	m.set(`{"fiches": ["caci-redemande"], "resume": "CACI redemandé."}`)
	_, token := e.screen2(t, validRequest(e.formKey(t)))
	require.Equal(t, http.StatusSeeOther, e.draftAction(t, "/demandes/confirmer", token).Code)
	_, err := e.db.ExecContext(context.Background(), `UPDATE tickets SET kb_ids = '["fiche-supprimee"]'`)
	require.NoError(t, err)
	cookie := e.login(t)
	page := e.openTicket(t, cookie, e.firstID(t)).body
	assert.Contains(t, page, "Fiche retirée depuis l'envoi : fiche-supprimee")
	assert.NotContains(t, page, "Aucune fiche proposée")
}

func TestFicheTablesAndDiagram(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	body := e.do(t, http.MethodGet, adminHost, "/fiches", nil, withCookie(cookie)).Body.String()
	_, fiche, found := strings.Cut(body, `id="tarification"`)
	require.True(t, found)
	assert.Contains(t, fiche, `<div class="overflow-x-auto"><table class="table table-sm">`)
	assert.Contains(t, fiche, `<th>Ton cas</th>`)
	assert.Contains(t, fiche, `<img src="/kb/tarification.svg" alt="Les plongées à la carte`)

	for _, host := range []string{publicHost, adminHost} {
		rec := e.do(t, http.MethodGet, host, "/kb/tarification.svg", nil)
		require.Equal(t, http.StatusOK, rec.Code, host)
		assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"), host)
		assert.Equal(t, "default-src 'none'; style-src 'unsafe-inline'", rec.Header().Get("Content-Security-Policy"), host)
		assert.Contains(t, rec.Body.String(), "<!-- mermaid sha256:", host)
		for _, path := range []string{"/kb/tarification.md", "/kb/carnet-solde-negatif.svg", "/kb/absente.svg"} {
			assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, host, path, nil).Code, host+path)
		}
	}
}
