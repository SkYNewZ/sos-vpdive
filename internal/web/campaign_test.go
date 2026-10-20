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

	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

// importCampaign stores campaign_valid.xlsx as the script would push it.
func (e *testEnv) importCampaign(t *testing.T) {
	t.Helper()
	rows, err := xlsx.ReadFirstSheet(fixtureBytes(t, "campaign_valid.xlsx"), imports.Limits())
	require.NoError(t, err)
	exp, err := campaign.Parse(rows, e.srv.paris)
	require.NoError(t, err)
	require.NoError(t, e.deps.Campaign.Import(context.Background(), exp))
}

// oneCampaignPerson is a campaign export of a single person, under half of
// campaign_valid.xlsx.
func oneCampaignPerson(t *testing.T) []byte {
	t.Helper()
	return xlsxtest.Build(t, xlsxtest.Sheet{
		{"Statut", "Nom", "Prénom", "Adhésion", "License"},
		{"En cours", "Bernard", "Hugo", "Adhésion (Réglé) - 50,00 €", "Licence fédérale (Réglé) - 42,00 €"},
	})
}

// Design 2026-10-20 « Import »: the script pushes the campaign export like
// the others; a push under half of the people in place is refused and mailed,
// and an upload with the second confirmation is the way past it.
func TestPushedCampaignImport(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	rec := e.push(t, "campaign", fixtureBytes(t, "campaign_valid.xlsx"), importToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, pushAnswer{Result: "imported", Read: 11, Kept: 10, Skipped: 1}, answer(t, rec))
	assert.Equal(t, 10, e.count(t, "campaign_lines"))

	half := e.push(t, "campaign", oneCampaignPerson(t), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, half.Code)
	a := answer(t, half)
	assert.Equal(t, "too_few", a.Error)
	assert.Contains(t, a.Message, "dépose-le à la main")
	assert.Equal(t, 10, e.count(t, "campaign_lines"), "the people in place stay")
	mails := e.clubMails(t)
	require.Len(t, mails, 1)
	assert.Equal(t, "Import automatique refusé : campagne d'adhésion", mails[0].Subject)

	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	preview := e.uploadAs(t, cookie, csrf, "campagne", oneCampaignPerson(t))
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	assert.Contains(t, html.UnescapeString(preview.Body.String()), "moins de la moitié des 10 personnes en place")
	id := previewID(t, preview)
	assert.Equal(t, http.StatusUnprocessableEntity, e.confirmAs(t, cookie, csrf, "campagne", id, false).Code)
	require.Equal(t, http.StatusSeeOther, e.confirmAs(t, cookie, csrf, "campagne", id, true).Code)
	assert.Equal(t, 1, e.count(t, "campaign_lines"))

	page := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie)).Body.String())
	_, journal, found := strings.Cut(page, `id="campagne-titre"`)
	require.True(t, found)
	for _, want := range []string{"Importé : 1 personne", "Refusé : Ce fichier contient moins de la moitié", "Importé : 10 personnes"} {
		assert.Contains(t, journal, want)
	}
}

// Design 2026-10-20 « Import »: the upload shows a preview, then replaces
// the people in place once confirmed.
func TestCampaignImportPreviewThenConfirm(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	page := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie)).Body.String())
	for _, want := range []string{`id="campagne-titre"`, "Campagne d'adhésion", "« Télécharger »", "Aucune campagne importée"} {
		assert.Contains(t, page, want)
	}

	csrf := e.csrf(t, cookie, "/imports")
	data := fixtureBytes(t, "campaign_valid.xlsx")
	rec := e.uploadAs(t, cookie, csrf, "campagne", data)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := html.UnescapeString(rec.Body.String())
	for _, want := range []string{
		"Aperçu avant remplacement", "21/10/2026 à 09:15 (indicatif)",
		"Personnes lues</dt><dd class=\"mb-2 sm:mb-0\">10",
		"Lignes sans nom écartées</dt><dd class=\"mb-2 sm:mb-0\">1",
		"Personnes en place</dt><dd class=\"mb-2 sm:mb-0\">0",
		"Remplacer la campagne",
	} {
		assert.Contains(t, body, want)
	}
	assert.Zero(t, e.count(t, "campaign_lines"), "nothing changes before the confirmation")

	done := e.confirmAs(t, cookie, csrf, "campagne", previewID(t, rec), false)
	require.Equal(t, http.StatusSeeOther, done.Code, done.Body.String())
	assert.Equal(t, "/imports?importe=campagne", done.Header().Get("Location"))
	page = html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports?importe=campagne", nil, withCookie(cookie)).Body.String())
	for _, want := range []string{"Campagne d'adhésion importée.", "Personnes</dt><dd class=\"mb-2 sm:mb-0\">10"} {
		assert.Contains(t, page, want)
	}
	assert.Equal(t, 10, e.count(t, "campaign_lines"))
	assert.Equal(t, e.deps.Keys.Hash(string(data)), e.fileHash(t, "campaign"), "the upload journals the file hash")
}

func TestCampaignImportRefusalsKeepThePeopleAndExplain(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"missing column": {fixtureBytes(t, "campaign_missing_column.xlsx"), "Colonne obligatoire absente : « License »"},
		"members export": {fixtureBytes(t, "members_valid.xlsx"), "Colonnes « Statut » et « Adhésion » introuvables"},
		"not a workbook": {[]byte("Nom;Prénom\n"), "Dépose l'export « Télécharger » de la page de la campagne d'adhésion"},
	} {
		rec := e.uploadAs(t, cookie, csrf, "campagne", c.data)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		body := html.UnescapeString(rec.Body.String())
		assert.Contains(t, body, c.want, name)
		label, msg := strings.Index(body, `for="file-campagne"`), strings.Index(body, c.want)
		assert.Greater(t, msg, label, "%s: the error sits after the campaign file field", name)
	}
	assert.Zero(t, e.count(t, "campaign_lines"))
}

// Design 2026-10-20 « Import »: past CAMPAIGN_MAX_AGE, a banner asks to
// import again and the club inbox gets one mail.
func TestCampaignReminder(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	home := func() string {
		t.Helper()
		return html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	}
	e.importCampaign(t)
	e.clock.advance(48 * time.Hour)
	assert.NotContains(t, home(), "La campagne d'adhésion date")
	assert.Empty(t, e.staleMails(t))
	e.clock.advance(time.Hour)
	assert.Contains(t, home(), "La campagne d'adhésion date du 02/09/2026. Pense à refaire l'import.")
	mails := e.staleMails(t)
	require.Len(t, mails, 1)
	assert.Equal(t, "Import ancien : campagne d'adhésion", mails[0].Subject)
}

// Design 2026-10-20 « Bloc Campagne d'adhésion »: the block of a request
// page in each of its states, through the members list, then through the
// typed name for a requester outside it, and never on the tracking page.
func TestRequestPageCampaignBlock(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	typed := func(first, last string) func(*tickets.Submission) {
		return func(s *tickets.Submission) { s.FirstName, s.LastName = first, last }
	}
	hugo := e.submitTicket(t, "hugo.bernard@example.org") // typed « Léa Martin »: the members list wins
	chloe := e.submitTicket(t, "chloe.petit@example.org")
	lea := e.submitTicket(t, "lea.martin@example.org")
	newcomer := e.submitTicket(t, "nouveau@example.org", typed("Hugo", "Bernard"))
	reversed := e.submitTicket(t, "inverse@example.org", typed("Bernard", "Hugo"))
	namesake := e.submitTicket(t, "paul@example.org", typed("Paul", "Inconnu"))
	stranger := e.submitTicket(t, "zoe@example.org", typed("Zoé", "Absente"))
	block := func(id int64) string {
		t.Helper()
		page := e.openTicket(t, cookie, id).body
		start, end := strings.Index(page, `id="campagne"`), strings.Index(page, `id="sorties"`)
		require.True(t, start >= 0 && start < end, "before the outings block")
		return page[start:end]
	}
	assert.Contains(t, block(hugo.ID), "Aucune campagne d'adhésion importée.")

	rows, err := xlsx.ReadFirstSheet(oneCampaignPerson(t), imports.Limits())
	require.NoError(t, err)
	exp, err := campaign.Parse(rows, e.srv.paris)
	require.NoError(t, err)
	require.NoError(t, e.deps.Campaign.Import(context.Background(), exp))
	assert.Contains(t, block(chloe.ID), "Ce membre n'est pas dans la campagne.")

	e.importCampaign(t)
	got := block(hugo.ID)
	for _, want := range []string{
		"Campagne d'adhésion", "Export du fichier créé le 21/10/2026 (indicatif)",
		"Statut", "En cours", "Pas encore validé", "Réglé · ", "50,00\u00a0€", "vpaydive (12/10/2026)", "Tout réglé",
	} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "rapproché par le nom tapé")
	assert.Contains(t, block(chloe.ID), "Validé le 14/10/2026 à 18:32 par Hugo Bernard")
	assert.Contains(t, block(lea.ID), "Plusieurs personnes portent ce nom : aucune ligne n'est affichée. Cherche dans VPDive.")

	for _, id := range []int64{newcomer.ID, reversed.ID} {
		got := block(id)
		assert.Contains(t, got, "rapproché par le nom tapé, à confirmer")
		assert.Contains(t, got, "vpaydive (12/10/2026)")
	}
	assert.Contains(t, block(namesake.ID), "Plusieurs personnes portent ce nom : aucune ligne n'est affichée.")
	assert.NotContains(t, block(namesake.ID), "rapproché par le nom tapé")
	assert.Contains(t, block(stranger.ID), "Le demandeur n'est pas dans la liste des membres, et le nom tapé n'est pas dans la campagne.")

	_, tracking := e.tracking(t, newcomer.Token)
	assert.NotContains(t, tracking, "Campagne d'adhésion")
	assert.NotContains(t, tracking, "rapproché")

	e.clock.advance(91 * 24 * time.Hour)
	require.NoError(t, e.deps.Campaign.Purge(context.Background()))
	cookie = e.login(t) // logins expire after 30 days of test clock
	assert.Contains(t, block(hugo.ID), "Plus de 90 jours sans import : la campagne d'adhésion a été effacée.")
}
