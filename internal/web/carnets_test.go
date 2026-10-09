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

	"github.com/SkYNewZ/sos-vpdive/internal/carnets"
)

// importCarnets stores the cards fixture as the script would push it; the
// members list must be in place for the holders to resolve.
func (e *testEnv) importCarnets(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	exp, err := carnets.Parse(fixtureBytes(t, "carnets_valid.json"), e.srv.paris)
	require.NoError(t, err)
	require.NoError(t, e.deps.Carnets.Resolve(ctx, exp))
	require.NoError(t, e.deps.Carnets.Import(ctx, exp))
}

// Design 2026-10-09 §2: the script pushes the cards as JSON; a purchase is
// skipped, a card of no member or of two is to check, and the same bytes
// again change nothing.
func TestPushedCarnetsImportsThenUnchanged(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	data := fixtureBytes(t, "carnets_valid.json")
	rec := e.push(t, "carnets", data, importToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, pushAnswer{Result: "imported", Read: 7, Kept: 4, Skipped: 1, ToCheck: 2}, answer(t, rec))
	assert.Equal(t, 4, e.count(t, "carnets"))
	var by string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT imported_by FROM imports WHERE kind = 'carnets'`).Scan(&by))
	assert.Equal(t, "script", by)

	again := e.push(t, "carnets", data, importToken)
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, pushAnswer{Result: "unchanged", Read: 7, Kept: 4, Skipped: 1, ToCheck: 2}, answer(t, again))
	var pushes int
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM imports WHERE kind = 'carnets'`).Scan(&pushes))
	assert.Equal(t, 1, pushes)
	for _, secret := range []string{"BERNARD", "Épave", "trésorier"} {
		assert.NotContains(t, e.logs.String(), secret)
	}
}

// A refused push keeps the cards in place and mails the committee without
// the « upload it by hand » advice: there is no manual upload.
func TestPushedCarnetsRefusals(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	require.Equal(t, http.StatusOK, e.push(t, "carnets", fixtureBytes(t, "carnets_valid.json"), importToken).Code)

	broken := e.push(t, "carnets", []byte(`{"from": "2024-09-02"`), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, broken.Code)
	assert.Equal(t, pushAnswer{Error: "invalid_carnets", Message: "Cette liste de cartes n'est pas un JSON lisible."}, answer(t, broken))

	empty := e.push(t, "carnets", []byte(`{"from": "2024-09-02", "to": "2026-09-02", "carts": [
		{"member": "BERNARD Hugo", "title": "Carte 10", "status": "", "method": "", "amount": "-75", "entries": []}]}`), importToken)
	assert.Equal(t, pushAnswer{Error: "invalid_carnets", Message: "Panier n° 1 : historique absent, ligne sans action ou date illisible."}, answer(t, empty))

	few := e.push(t, "carnets", []byte(`{"from": "2024-09-02", "to": "2026-09-02", "carts": [
		{"member": "BERNARD Hugo", "title": "Carte 10", "status": "", "method": "", "amount": "-75",
		 "entries": [{"action": "Ajout au panier", "at": "2026-05-01T09:00:00+02:00", "by": "", "detail": ""}]}]}`), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, few.Code)
	a := answer(t, few)
	assert.Equal(t, "too_few", a.Error)
	assert.Contains(t, a.Message, "moins de la moitié des cartes en place")
	assert.Contains(t, a.Message, "page Paiements de VPDive")
	assert.Equal(t, 4, e.count(t, "carnets"), "the cards in place stay")

	mails := e.clubMails(t)
	require.Len(t, mails, 3)
	for _, m := range mails {
		assert.Equal(t, "Import automatique refusé : cartes VPDive", m.Subject)
		assert.NotContains(t, m.Text, "à la main")
		assert.Contains(t, m.Text, "Dernières cartes reçues")
	}
}

// The imports page shows the latest cards received; nothing to upload.
func TestImportsPageShowsTheCards(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	page := func() string {
		t.Helper()
		rec := e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		return html.UnescapeString(rec.Body.String())
	}
	assert.Contains(t, page(), "Aucune carte reçue pour l'instant.")
	require.Equal(t, http.StatusOK, e.push(t, "carnets", fixtureBytes(t, "carnets_valid.json"), importToken).Code)
	_, rest, found := strings.Cut(page(), `id="cartes-titre"`)
	require.True(t, found, "the cards section is on the page")
	section, _, found := strings.Cut(rest, "</section>")
	require.True(t, found)
	for _, want := range []string{"du 02/09/2024 au 02/09/2026", `sm:mb-0">script</dd>`, `sm:mb-0">4</dd>`} {
		assert.Contains(t, section, want)
	}
	assert.NotContains(t, section, "<form", "no upload form for the cards")
}

// Past CARNETS_MAX_AGE, a banner says the script may be broken, and the club
// inbox gets one mail.
func TestCarnetsAges(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	home := func() string {
		t.Helper()
		return html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	}
	require.Equal(t, http.StatusOK, e.push(t, "carnets", fixtureBytes(t, "carnets_valid.json"), importToken).Code)
	e.clock.advance(47 * time.Hour)
	assert.NotContains(t, home(), "Les cartes VPDive datent")
	e.clock.advance(2 * time.Hour)
	body := home()
	assert.Contains(t, body, "Les cartes VPDive datent du 02/09/2026. Le script d'import est peut-être en panne.")
	assert.Contains(t, body, "#cartes-titre")
	assert.Contains(t, body, "Voir les dernières cartes reçues")

	mails := e.staleMails(t)
	require.Len(t, mails, 1)
	assert.Equal(t, "Import ancien : cartes VPDive", mails[0].Subject)
	assert.Contains(t, mails[0].Text, "Dernières cartes reçues")
}

// Design 2026-10-09 §6: the « Cartes VPDive » block of a request page,
// between the payments and Mollie blocks, in each of its states, never on
// the member's tracking page.
func TestRequestPageCarnetsBlock(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	chloe := e.submitTicket(t, "chloe.petit@example.org")
	lea := e.submitTicket(t, "lea.martin@example.org")
	noe := e.submitTicket(t, "noe.durand@example.org")
	block := func(id int64) string {
		t.Helper()
		page := e.openTicket(t, cookie, id).body
		start, end := strings.Index(page, `id="cartes"`), strings.Index(page, `id="encaissements"`)
		require.True(t, strings.Index(page, `id="paiements"`) < start && start < end, "between the payments and Mollie blocks")
		return page[start:end]
	}
	assert.Contains(t, block(hugo.ID), "Aucune carte reçue pour l'instant")

	e.importCarnets(t)
	got := block(hugo.ID)
	for _, want := range []string{
		"Cartes reçues le 02/09/2026 à 12:00 : vérifie dans VPDive avant d'agir.",
		"Carte 10 plongées niveau 1 et 2", "avoir restant 120,00\u00a0€", "4 débits, net 42,00\u00a0€", "montant inhabituel",
		"Sortie Épave (N2) · 29/08/2026", "répartie sur deux cartes, 25,00\u00a0€ en tout",
		"Prix : -300,00\u00a0€ → -270,00\u00a0€", "Commentaire : Débit de la plongée de nuit à revoir avec le trésorier",
		"+30,00\u00a0€", "Ajout au panier", "Léa MARTIN",
		`<tr class="bg-warning/10"><td class="whitespace-nowrap">01/07/2026 à 19:00</td>`,
	} {
		assert.Contains(t, got, want)
	}
	assert.Less(t, strings.Index(got, "Carte 5 plongées"), strings.Index(got, "Carte 10 plongées"), "newest card first")
	assert.Equal(t, 1, strings.Count(got, "bg-warning/10"), "the split dive is not unusual")

	partial := block(chloe.ID)
	assert.Contains(t, partial, "totaux partiels, 2 lignes non lues")
	assert.Contains(t, partial, "non lue</span>")
	assert.NotContains(t, partial, "débits, net")
	assert.Contains(t, block(lea.ID), "Plusieurs membres portent ce nom : aucune carte n'est affichée.")
	assert.Contains(t, block(noe.ID), "Aucune carte pour ce membre sur les 24 derniers mois.")

	e.clock.advance(49 * time.Hour)
	assert.Contains(t, block(hugo.ID), ">périmé</span>")

	e.importMembers(t, "members_minimal.xlsx")
	assert.Contains(t, block(noe.ID), "Le demandeur n'est pas dans la liste des membres : ses cartes ne peuvent pas être rattachées.")

	e.clock.advance(91 * 24 * time.Hour)
	require.NoError(t, e.deps.Carnets.Purge(context.Background()))
	cookie = e.login(t) // logins expire after 30 days of test clock
	assert.Contains(t, block(hugo.ID), "Plus de 90 jours sans envoi du script : les cartes ont été effacées.")

	_, tracking := e.tracking(t, hugo.Token)
	assert.NotContains(t, tracking, "Cartes VPDive")
}
