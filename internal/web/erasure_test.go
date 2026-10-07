package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

// fail makes the fake relay refuse every mail with err, or accept again.
func (f *fakeSender) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func TestErasureOfAPerson(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	e.importPayments(t)
	e.importMollie(t)
	e.importCalendar(t)
	cookie := e.login(t)
	first := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	e.submitTicket(t, "lea.martin@example.org")
	kept := e.submitTicket(t, "hugo.bernard@example.org")

	page := e.do(t, http.MethodGet, adminHost, "/effacement?demande="+strconv.FormatInt(first.ID, 10), nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), `value="lea.martin@example.org"`, "prefilled from the request page link")
	csrf := csrfPattern.FindStringSubmatch(page.Body.String())[1]
	post := func(v url.Values) string {
		t.Helper()
		v.Set("csrf", csrf)
		rec := e.do(t, http.MethodPost, adminHost, "/effacement", formBody(v), formType, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return html.UnescapeString(rec.Body.String())
	}

	preview := post(url.Values{"email": {" Lea.Martin@Example.org "}, "etape": {"apercu"}})
	for _, want := range []string{
		"2 demandes", "une ligne dans la liste des membres", "2 lignes de paiement", "2 lignes d'encaissement Mollie", "ne peut pas être rappelé",
		"3 participations au calendrier, y compris celles d'un homonyme éventuel",
		"les prochains imports de VPDive la réinscrivent",
	} {
		assert.Contains(t, preview, want)
	}
	assert.Equal(t, 3, e.count(t, "tickets"), "a preview erases nothing")

	done := post(url.Values{"email": {"lea.martin@example.org"}, "etape": {"confirmer"}})
	assert.Contains(t, done, "Données effacées : 2 demandes, la ligne de la liste des membres, 2 lignes de paiement, 2 lignes d'encaissement Mollie et 3 participations au calendrier.")
	assert.Equal(t, 2, e.count(t, "calendar_participants"), "Bernard Hugo and the pilot stay")
	assert.Equal(t, 18, e.count(t, "payment_lines"))
	assert.Equal(t, 14, e.count(t, "online_payment_lines"))
	assert.Equal(t, 1, e.count(t, "tickets"))
	assert.Equal(t, kept.ID, e.detail(t, kept.ID).ID)
	known, err := e.deps.Members.Lookup(context.Background(), "lea.martin@example.org")
	require.NoError(t, err)
	assert.False(t, known)
	code, _ := e.tracking(t, first.Token)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Zero(t, e.objects(t))
	assert.NotContains(t, e.logs.String(), "lea.martin@example.org")

	invalid := e.do(t, http.MethodPost, adminHost, "/effacement",
		formBody(url.Values{"csrf": {csrf}, "email": {"pas une adresse"}, "etape": {"apercu"}}), formType, withCookie(cookie))
	assert.Equal(t, http.StatusUnprocessableEntity, invalid.Code)
	forged := e.do(t, http.MethodPost, adminHost, "/effacement",
		formBody(url.Values{"csrf": {"forged"}, "email": {"hugo.bernard@example.org"}, "etape": {"confirmer"}}), formType, withCookie(cookie))
	assert.Equal(t, http.StatusForbidden, forged.Code)
	assert.Equal(t, 1, e.count(t, "tickets"))
}

func TestFailedMailsBannerListAndRetry(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	out, err := e.deps.Tickets.Submit(ctx, newSubmission(t, "lea.martin@example.org"), nil)
	require.NoError(t, err)
	ref := out.Ref
	e.sender.fail(fmt.Errorf("550 mailbox unavailable: %w", mail.ErrPermanent))
	sent, err := e.deps.Outbox.SendDue(ctx, e.sender)
	require.NoError(t, err)
	require.Zero(t, sent)
	e.sender.fail(nil)
	cookie := e.login(t)

	board := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, board, "2 mails n'ont pas pu partir.")
	assert.Contains(t, board, `href="/envois"`)

	list := e.do(t, http.MethodGet, adminHost, "/envois", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, list.Code)
	for _, want := range []string{ref, "lea.martin@example.org", clubEmail, "Relancer"} {
		assert.Contains(t, list.Body.String(), want)
	}
	failed, err := e.deps.Outbox.Failed(ctx)
	require.NoError(t, err)
	require.Len(t, failed, 2)

	csrf := csrfPattern.FindStringSubmatch(list.Body.String())[1]
	retry := e.do(t, http.MethodPost, adminHost, "/envois/"+strconv.FormatInt(failed[0].ID, 10)+"/relancer",
		formBody(url.Values{"csrf": {csrf}}), formType, withCookie(cookie))
	require.Equal(t, http.StatusSeeOther, retry.Code)
	assert.Equal(t, "/envois?relance=1", retry.Header().Get("Location"))
	left, err := e.deps.Outbox.FailedCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, left)
	assert.Len(t, e.mails(t), 1, "the retried mail goes out")

	unknown := e.do(t, http.MethodPost, adminHost, "/envois/9999/relancer", formBody(url.Values{"csrf": {csrf}}), formType, withCookie(cookie))
	assert.Equal(t, http.StatusNotFound, unknown.Code)
}

func TestIdleRequestsBanner(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	e.clock.advance(366 * 24 * time.Hour)
	cookie := e.login(t)
	body := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, body, "sans activité depuis 12 mois")
	assert.Contains(t, body, tk.Ref)
	assert.Contains(t, body, `href="/effacement"`, "the navigation reaches erasure")
}

func TestLotTwoRoutesStayOnTheirDomain(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	id := strconv.FormatInt(tk.ID, 10)
	for _, path := range []string{"/demandes/" + id, "/demandes/" + id + "/captures/1", "/evenements", "/effacement", "/envois"} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, path, nil).Code, "public GET %s", path)
	}
	for _, path := range []string{"/demandes/" + id + "/actions", "/effacement", "/envois/1/relancer"} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, publicHost, path, nil, formType).Code, "public POST %s", path)
	}
	for _, path := range []string{"/suivi/" + tk.Token, "/suivi/" + tk.Token + "/captures/1", "/retrouver", "/demandes/envoyee?ref=" + tk.Ref} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie)).Code, "committee GET %s", path)
	}
	for _, path := range []string{"/demandes", "/suivi/" + tk.Token + "/reponse", "/suivi/" + tk.Token + "/cloture", "/retrouver"} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, adminHost, path, nil, formType, withCookie(cookie)).Code, "committee POST %s", path)
	}
}
