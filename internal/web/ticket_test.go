package web

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

var versionField = regexp.MustCompile(`name="version" value="(\d+)"`)

func bob() admins.Account {
	return admins.Account{Username: "bob", Name: "Bob", Role: "Trésorier", PasswordHash: testHash()}
}

// addBob adds a second committee account; alice's sessions stay valid.
func (e *testEnv) addBob(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(e.adminsPath, []byte(accountsFile(alice(), bob())), 0o600))
	_, err := e.deps.Admins.Reload()
	require.NoError(t, err)
}

// shownTicket is a request page as a committee member saw it.
type shownTicket struct {
	body    string // unescaped
	csrf    string
	version string
}

func (e *testEnv) openTicket(t *testing.T, cookie *http.Cookie, id int64) shownTicket {
	t.Helper()
	rec := e.do(t, http.MethodGet, adminHost, "/demandes/"+strconv.FormatInt(id, 10), nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.String()
	csrf := csrfPattern.FindStringSubmatch(raw)
	require.NotNil(t, csrf)
	version := versionField.FindStringSubmatch(raw)
	require.NotNil(t, version)
	return shownTicket{body: html.UnescapeString(raw), csrf: csrf[1], version: version[1]}
}

// act posts an action from a page as it was shown, with its version.
func (e *testEnv) act(t *testing.T, cookie *http.Cookie, id int64, page shownTicket, v url.Values) *httptest.ResponseRecorder {
	t.Helper()
	v.Set("csrf", page.csrf)
	v.Set("version", page.version)
	return e.do(t, http.MethodPost, adminHost, "/demandes/"+strconv.FormatInt(id, 10)+"/actions", formBody(v), formType, withCookie(cookie))
}

func TestRequestPageBlocks(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	first := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	other := e.submitTicket(t, "lea.martin@example.org")
	page := e.openTicket(t, cookie, first.ID)
	id := strconv.FormatInt(first.ID, 10)
	for _, want := range []string{
		first.Ref, "Léa Martin", "lea.martin@example.org", "Copier le nom", "31/12/2026", other.Ref,
		"https://vpdive.example.org/app/admin/vpdive/%2Ff%2Fpayment%2Findex?route=/f/payment/index",
		"Date de création", "01/01/2026 au 02/09/2026", "Je prends", "Prends la demande pour répondre",
		"/demandes/" + id + "/captures/", "Supprimer cette demande", "/effacement?demande=" + id,
		`data-ticket-page="` + id + `"`, "Demande envoyée", "Mon carnet affiche un montant",
	} {
		assert.Contains(t, page.body, want)
	}
	assert.NotContains(t, page.body, "/suivi/", "the secret link never reaches the committee")
	assert.NotContains(t, page.body, "/w/f-a-q", "member links stay off the request page")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, "/demandes/9999", nil, withCookie(cookie)).Code)
}

func TestTakeAssignsAndMailsTheMember(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	page := e.openTicket(t, cookie, tk.ID)
	before := len(e.mails(t))

	rec := e.act(t, cookie, tk.ID, page, url.Values{"action": {"take"}})
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	assert.Equal(t, "/demandes/"+strconv.FormatInt(tk.ID, 10), rec.Header().Get("Location"))
	d := e.detail(t, tk.ID)
	assert.Equal(t, tickets.StatusInProgress, d.Status)
	assert.Equal(t, "alice", d.Assignee)
	taken := 0
	for _, m := range e.mails(t)[before:] {
		if m.To == "lea.martin@example.org" {
			assert.Contains(t, m.Text, "s'occupe de ta demande")
			taken++
		}
	}
	assert.Equal(t, 1, taken)
	after := e.openTicket(t, cookie, tk.ID)
	assert.Contains(t, after.body, "Répondre à l'adhérent")
	assert.Contains(t, after.body, "Note interne")
}

func TestTakeRaceNamesWhoTookIt(t *testing.T) {
	e := newTestEnv(t)
	e.addBob(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	page := e.openTicket(t, cookie, tk.ID)
	require.NoError(t, e.deps.Tickets.Apply(context.Background(),
		tickets.Command{Action: tickets.ActionTake, TicketID: tk.ID, Version: e.detail(t, tk.ID).Version, Actor: "bob"}))

	rec := e.act(t, cookie, tk.ID, page, url.Values{"action": {"take"}})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Bob (Trésorier) a pris cette demande entre-temps")
	assert.Equal(t, "bob", e.detail(t, tk.ID).Assignee)
}

// Review Focus 5: an action from a stale page keeps what was typed.
func TestStaleReplyKeepsTheTypedText(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionTake})
	page := e.openTicket(t, cookie, tk.ID)
	require.NoError(t, e.deps.Tickets.MemberReply(context.Background(), tk.ID, "Je précise ma demande.", nil))

	rec := e.act(t, cookie, tk.ID, page, url.Values{"action": {"reply"}, "message": {"Voici ma réponse détaillée."}})
	assert.Equal(t, http.StatusConflict, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, ">Voici ma réponse détaillée.</textarea>")
	assert.Contains(t, body, "Je précise ma demande.", "the page shows the current state")
	assert.Contains(t, body, "a changé entre-temps")
	for _, m := range e.detail(t, tk.ID).Messages {
		assert.True(t, m.FromMember, "nothing from the stale page was stored")
	}
}

func TestReplyWaitAndNote(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionTake})
	before := len(e.mails(t))

	page := e.openTicket(t, cookie, tk.ID)
	rec := e.act(t, cookie, tk.ID, page, url.Values{"action": {"reply_wait"}, "message": {"Peux-tu m'envoyer une capture de ton panier ?"}})
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	d := e.detail(t, tk.ID)
	assert.Equal(t, tickets.StatusWaiting, d.Status)
	sent := e.mails(t)[before:]
	require.Len(t, sent, 1, "a reply and a status change make one mail")
	assert.Equal(t, "lea.martin@example.org", sent[0].To)

	page = e.openTicket(t, cookie, tk.ID)
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, tk.ID, page, url.Values{"action": {"note"}, "note": {"Note secrète du comité"}}).Code)
	assert.Len(t, e.mails(t), before+1, "a note sends no mail")
	_, tracking := e.tracking(t, tk.Token)
	assert.Contains(t, tracking, "Peux-tu m'envoyer une capture de ton panier ?")
	assert.NotContains(t, tracking, "Note secrète")
	shown := e.openTicket(t, cookie, tk.ID)
	assert.Contains(t, shown.body, "Note secrète du comité")
	assert.Contains(t, shown.body, "Note interne, visible du comité seulement")
}

func TestReclassifyBothWays(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org", func(s *tickets.Submission) {
		s.Fields.Values["montant"] = "-180,00 €"
	})
	journal := len(e.detail(t, tk.ID).Events)
	page := e.openTicket(t, cookie, tk.ID)
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, tk.ID, page, url.Values{"action": {"category"}, "categorie": {"bug"}}).Code)
	d := e.detail(t, tk.ID)
	assert.Equal(t, "bug", d.Category)
	assert.Equal(t, "-180,00 €", d.Fields.Values["montant"], "typed fields stay")

	page = e.openTicket(t, cookie, tk.ID)
	assert.Contains(t, page.body, "Bug VPDive")
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, tk.ID, page, url.Values{"action": {"category"}, "categorie": {"carnet"}}).Code)
	d = e.detail(t, tk.ID)
	assert.Equal(t, "carnet", d.Category)
	assert.Len(t, d.Events, journal+2, "both reclassifications are in the journal")
}

func TestDeleteRequestEndsTheTrackingLink(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	require.Equal(t, 1, e.objects(t))
	page := e.openTicket(t, cookie, tk.ID)

	rec := e.act(t, cookie, tk.ID, page, url.Values{"action": {"delete"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/?supprimee=1", rec.Header().Get("Location"))
	code, _ := e.tracking(t, tk.Token)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Zero(t, e.count(t, "tickets"))
	assert.Zero(t, e.objects(t))
	board := e.do(t, http.MethodGet, adminHost, "/?supprimee=1", nil, withCookie(cookie))
	assert.Contains(t, board.Body.String(), "Demande supprimée.")
}

func TestDeleteMessageAndCapture(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionTake})
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionReply, Body: "Réponse à effacer."})
	d := e.detail(t, tk.ID)
	require.Len(t, d.Messages, 1)
	require.Len(t, d.Captures, 1)

	page := e.openTicket(t, cookie, tk.ID)
	rec := e.act(t, cookie, tk.ID, page, url.Values{"action": {"delete_message"}, "message_id": {strconv.FormatInt(d.Messages[0].ID, 10)}})
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	page = e.openTicket(t, cookie, tk.ID)
	rec = e.act(t, cookie, tk.ID, page, url.Values{"action": {"delete_capture"}, "capture": {strconv.FormatInt(d.Captures[0].ID, 10)}})
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())

	after := e.detail(t, tk.ID)
	assert.Empty(t, after.Messages)
	assert.Empty(t, after.Captures)
	assert.Zero(t, e.objects(t))
	assert.NotContains(t, e.openTicket(t, cookie, tk.ID).body, "Réponse à effacer.")
}

func TestActionRefusals(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	path := "/demandes/" + strconv.FormatInt(tk.ID, 10)
	page := e.openTicket(t, cookie, tk.ID)

	forged := url.Values{"csrf": {"forged"}, "version": {page.version}, "action": {"take"}}
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodPost, adminHost, path+"/actions", formBody(forged), formType, withCookie(cookie)).Code)
	publicOrigin := func(r *http.Request) { r.Header.Set("Origin", "https://"+publicHost) }
	ok := url.Values{"csrf": {page.csrf}, "version": {page.version}, "action": {"take"}}
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodPost, adminHost, path+"/actions", formBody(ok), formType, withCookie(cookie), publicOrigin).Code)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, path, nil).Code)

	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionTake})
	page = e.openTicket(t, cookie, tk.ID)
	empty := e.act(t, cookie, tk.ID, page, url.Values{"action": {"reply"}, "message": {"  "}})
	assert.Equal(t, http.StatusUnprocessableEntity, empty.Code)
	assert.Contains(t, html.UnescapeString(empty.Body.String()), "Écris ta réponse")
	refused := e.act(t, cookie, tk.ID, page, url.Values{"action": {"take"}})
	assert.Equal(t, http.StatusUnprocessableEntity, refused.Code, "take on a request in progress")
	assert.Contains(t, html.UnescapeString(refused.Body.String()), "Cette action n'est pas possible")
}

func TestCommitteeCaptureRoute(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	other := e.submitTicket(t, "hugo.bernard@example.org")
	capture := strconv.FormatInt(e.detail(t, tk.ID).Captures[0].ID, 10)

	rec := e.do(t, http.MethodGet, adminHost, "/demandes/"+strconv.FormatInt(tk.ID, 10)+"/captures/"+capture, nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, pngBytes(t), rec.Body.Bytes())
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost,
		"/demandes/"+strconv.FormatInt(other.ID, 10)+"/captures/"+capture, nil, withCookie(cookie)).Code)
	anonymous := e.do(t, http.MethodGet, adminHost, "/demandes/"+strconv.FormatInt(tk.ID, 10)+"/captures/"+capture, nil)
	assert.Equal(t, http.StatusSeeOther, anonymous.Code)
}

// Review Focus 2, request page side.
func TestRequestPageShowsRemovedCategory(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org", func(s *tickets.Submission) {
		s.Fields.Values["montant"] = "-180,00 €"
	})
	reduced, err := tickets.LoadCatalog(fstest.MapFS{
		"config/categories.yaml": {Data: []byte("categories:\n  - id: autre\n    label: Autre\n")},
		"config/products.yaml":   {Data: []byte("products:\n  - id: autre\n    label: Autre\n")},
	})
	require.NoError(t, err)
	narrow := e.withServer(t, func(d *Deps) { d.Catalog = reduced })
	page := narrow.openTicket(t, cookie, tk.ID)
	assert.Contains(t, page.body, "carnet (retiré)")
	assert.Contains(t, page.body, "-180,00 €")
	assert.Contains(t, page.body, "retiré")
}
