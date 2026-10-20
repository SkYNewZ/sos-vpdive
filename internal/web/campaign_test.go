package web

import (
	"context"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
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

// Design 2026-10-20 « Assistant »: member_membership gives the campaign line
// of a person as the request block does, a homonym without detail.
func TestToolMemberMembership(t *testing.T) {
	e, tb := toolEnv(t)
	tb.call(t, "find_member", `{"query":"Hugo Bernard"}`)
	got, _ := tb.call(t, "member_membership", `{"ref":"m1"}`)
	assert.Equal(t, "aucun_import", got["etat"])

	e.importCampaign(t)
	got, step := tb.call(t, "member_membership", `{"ref":"m1"}`)
	assert.Equal(t, "Campagne d'adhésion de Hugo Bernard lue", step)
	assert.Equal(t, "ok", got["etat"])
	assert.Equal(t, "21/10/2026 à 09:15", got["export_du"], "the export date is authoritative")
	assert.NotEmpty(t, got["import_campagne"].(map[string]any)["recu"])
	lines := got["lignes"].([]any)
	require.Len(t, lines, 1)
	hugo := mustJSON(t, lines[0])
	for _, want := range []string{`"statut":"En cours"`, `"validation":"Pas encore validé"`, `"etat":"Réglé"`,
		`"montant":"50,00 €"`, `"moyen":"vpaydive"`, `"le":"12/10/2026"`, `"reglement":"Tout réglé"`} {
		assert.Contains(t, hugo, want)
	}
	assert.Equal(t, "Campagne d'adhésion", tb.sources[len(tb.sources)-1].Label)

	tb.call(t, "find_member", `{"query":"Chloé Petit"}`)
	got, _ = tb.call(t, "member_membership", `{"ref":"m2"}`)
	assert.Contains(t, mustJSON(t, got["lignes"]), `"validation":"Validé le 14/10/2026 à 18:32 par Hugo Bernard"`)

	tb.call(t, "find_member", `{"query":"Noé Durand"}`)
	got, _ = tb.call(t, "member_membership", `{"ref":"m3"}`)
	assert.Contains(t, mustJSON(t, got["lignes"]), `"etat":"Au panier"`)

	tb.call(t, "find_member", `{"query":"Max Roux"}`)
	got, _ = tb.call(t, "member_membership", `{"ref":"m4"}`)
	assert.Contains(t, got["erreur"], "trois personnes", "the membership counts among the people read")

	tb.read = nil
	got, _ = tb.call(t, "member_membership", `{"ref":"m4"}`)
	roux := mustJSON(t, got["lignes"])
	assert.Contains(t, roux, `"validation":"Validé, validation illisible"`)
	assert.Contains(t, roux, `"a_verifier":"Adhésion réglée en espèces"`)

	tb.call(t, "find_member", `{"query":"Léa Martin"}`)
	got, _ = tb.call(t, "member_membership", `{"ref":"m5"}`)
	assert.Equal(t, "homonyme", got["etat"])
	assert.Empty(t, got["lignes"], "no detail for a homonym")
}

// Design 2026-10-20 « Assistant »: a new member is in the campaign before
// the members list; find_member offers them, once, without an address.
func TestToolFindMemberInTheCampaign(t *testing.T) {
	e, tb := toolEnv(t)
	e.importCampaign(t)
	got, _ := tb.call(t, "find_member", `{"query":"Max Roux"}`)
	assert.Equal(t, "exacte", got["correspondance"])
	cands := got["candidats"].([]any)
	require.Len(t, cands, 1)
	roux := cands[0].(map[string]any)
	assert.Equal(t, map[string]any{"ref": "m1", "nom": "Max Roux", "source": "campagne"}, roux)
	assert.Empty(t, tb.c.People[0].Email)

	got, _ = tb.call(t, "find_member", `{"query":"roux"}`)
	assert.Equal(t, "m1", got["candidats"].([]any)[0].(map[string]any)["ref"], "one ref per person")

	got, _ = tb.call(t, "find_member", `{"query":"Paul Inconnu"}`)
	cands = got["candidats"].([]any)
	require.Len(t, cands, 1, "two campaign lines of one name: one candidate")
	assert.Equal(t, true, cands[0].(map[string]any)["homonyme"])

	got, _ = tb.call(t, "find_member", `{"query":"Hugo Bernard"}`)
	cands = got["candidats"].([]any)
	require.Len(t, cands, 1, "a member's campaign line is not a second candidate")
	assert.NotContains(t, cands[0], "source")
	assert.Len(t, tb.c.People, 3)
}

// Design 2026-10-20 « Assistant »: a tool that needs an address answers a
// campaign-only person normally; an error would end the whole answer.
func TestToolMemberRequestsOfACampaignPerson(t *testing.T) {
	e, tb := toolEnv(t)
	e.importCampaign(t)
	tb.call(t, "find_member", `{"query":"Max Roux"}`)
	got, _ := tb.call(t, "member_membership", `{"ref":"m1"}`)
	assert.Equal(t, "ok", got["etat"])
	got, step := tb.call(t, "member_requests", `{"ref":"m1"}`)
	assert.Equal(t, "pas d'adresse : personne connue par la campagne seulement", got["note"])
	assert.Empty(t, got["demandes"])
	assert.Equal(t, "Demandes de Max Roux : pas d'adresse", step)
}

// importCampaignSheet stores a campaign export built from rows under the
// usual header.
func (e *testEnv) importCampaignSheet(t *testing.T, rows ...[]any) {
	t.Helper()
	sheet := append(xlsxtest.Sheet{{"Statut", "Nom", "Prénom", "Adhésion", "License"}}, rows...)
	parsed, err := xlsx.ReadFirstSheet(xlsxtest.Build(t, sheet), imports.Limits())
	require.NoError(t, err)
	exp, err := campaign.Parse(parsed, e.srv.paris)
	require.NoError(t, err)
	require.NoError(t, e.deps.Campaign.Import(context.Background(), exp))
}

// Design 2026-10-20 « Une ligne ambiguous n'est jamais attribuée à une
// personne »: a name the campaign holds twice, with no member, is a homonym
// for the payments and the outings too, whose lines are stored by name hash.
func TestToolCampaignHomonymReadsNoLines(t *testing.T) {
	e, tb := toolEnv(t)
	_, err := e.db.ExecContext(context.Background(), `DELETE FROM members WHERE name_hash = ?`,
		e.srv.keys.Hash(secure.NameKey("Durand", "Noé")))
	require.NoError(t, err)
	e.importCampaignSheet(t, []any{"En cours", "Durand", "Noé", "", ""}, []any{"En cours", "Durand", "Noé", "", ""})

	got, _ := tb.call(t, "find_member", `{"query":"Noé Durand"}`)
	assert.Equal(t, true, got["candidats"].([]any)[0].(map[string]any)["homonyme"])
	got, _ = tb.call(t, "member_payments", `{"ref":"m1","du":"2020-01-01"}`)
	for _, k := range []string{"etat", "etat_mollie", "etat_cartes"} {
		assert.Equal(t, "ambigu", got[k], k)
	}
	for _, k := range []string{"soldes", "lignes", "mollie", "cartes"} {
		assert.Empty(t, got[k], k)
	}
	got, _ = tb.call(t, "member_outings", `{"ref":"m1","du":"2020-01-01"}`)
	assert.Equal(t, "ambigu", got["etat"])
	assert.Empty(t, got["sorties"])

	html, err := e.srv.fragment("assistantDossier", dossierView{People: tb.c.People})
	require.NoError(t, err)
	assert.Contains(t, string(html), "Homonyme")
	assert.Contains(t, string(html), "Connu par la campagne d'adhésion seulement")
	assert.NotContains(t, string(html), "Saisons :")
	assert.NotContains(t, string(html), "C'est cette personne", "one ref for both: picking it tells nothing")
}

// members.Search hides partial matches behind an exact one: a campaign
// person holding every word of the query is an exact match too.
func TestToolFindMemberCampaignHidesPartialMembers(t *testing.T) {
	e, tb := toolEnv(t)
	sheet := make(xlsxtest.Sheet, 0, 1+maxCandidates)
	sheet = append(sheet, []any{"Nom", "Prénom", "Email"})
	for i := range maxCandidates {
		last := "Membre" + string(rune('A'+i)) // NameKey drops digits
		sheet = append(sheet, []any{last, "Nicolas", "nicolas" + strconv.Itoa(i) + "@example.org"})
	}
	rows, err := xlsx.ReadFirstSheet(xlsxtest.Build(t, sheet), imports.Limits())
	require.NoError(t, err)
	exp, err := members.Parse(rows, time.UTC)
	require.NoError(t, err)
	require.NoError(t, e.deps.Members.Import(context.Background(), exp))
	e.importCampaignSheet(t, []any{"En cours", "Nouveau", "Nicolas", "", ""})

	got, _ := tb.call(t, "find_member", `{"query":"Nicolas Nouveau"}`)
	assert.Equal(t, "exacte", got["correspondance"])
	cands := got["candidats"].([]any)
	require.Len(t, cands, 1, mustJSON(t, cands))
	assert.Equal(t, map[string]any{"ref": "m1", "nom": "Nicolas Nouveau", "source": "campagne"}, cands[0])
	assert.Len(t, tb.c.People, 1, "the partial members never reach the conversation")
}
