package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fingerprintPattern = regexp.MustCompile(`name="empreinte" value="([0-9a-f]{64})"`)

// Spec §7.7, §13: « Paiements à vérifier » lists the Mollie lines not settled
// in VPDive and the partial payments, oldest first, and a resolver masks a
// line once checked.
func TestChecksPage(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	page := func() string {
		t.Helper()
		rec := e.do(t, http.MethodGet, adminHost, "/anomalies", nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		return html.UnescapeString(rec.Body.String())
	}
	body := page()
	assert.Contains(t, body, `href="/anomalies"`, "in the committee nav")
	assert.Contains(t, body, "<title>Paiements à vérifier")
	assert.Contains(t, body, "Aucun export de paiements ni d'encaissements Mollie importé.")

	e.importMembers(t, "members_valid.xlsx")
	e.importPayments(t)
	e.importMollie(t)
	body = page()
	for _, want := range []string{
		"4 lignes à vérifier : 3 encaissées par Mollie et non soldées dans VPDive, 1 paiement partiel.",
		"L'outil ne corrige rien : vérifie dans VPDive, puis masque la ligne.",
		"Hugo Bernard", "Noé Durand", "Formation RIFAP", "60,00 € payés, prix 120,00 €", "300,00 €",
		"encaissé par Mollie, non soldé dans VPDive", "paiement partiel",
		`<span class="text-error font-bold">90 j</span>`, // 04/06 to 02/09
		"/f/mollie/index", "/f/payment/index",
	} {
		assert.Contains(t, body, want)
	}
	assert.Less(t, strings.Index(body, "Formation RIFAP"), strings.Index(body, "Baptême"), "oldest first")
	assert.NotContains(t, body, "Masquées", "nothing masked yet")

	prints := fingerprintPattern.FindAllStringSubmatch(body, -1)
	require.Len(t, prints, 4)
	durand := prints[3][1]
	csrf := csrfPattern.FindStringSubmatch(body)[1]
	dismiss := func(fingerprint string, mutators ...func(*http.Request)) int {
		t.Helper()
		v := url.Values{"csrf": {csrf}, "empreinte": {fingerprint}}
		return e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(v),
			append([]func(*http.Request){formType, withCookie(cookie)}, mutators...)...).Code
	}
	assert.Equal(t, http.StatusSeeOther, dismiss(durand))
	body = page()
	assert.Contains(t, body, "3 lignes à vérifier")
	assert.Contains(t, body, "Masquées (1)")
	assert.Contains(t, body, "masquée par Alice (Présidente) le 02/09/2026")
	assert.Contains(t, e.logs.String(), `"msg":"check dismissed"`)
	assert.NotContains(t, e.logs.String(), "Durand")

	e.importMollie(t)
	assert.Contains(t, page(), "Masquées (1)", "still masked after a new import of the same file")

	assert.Equal(t, http.StatusNotFound, dismiss(strings.Repeat("0", 64)), "no line has it")
	assert.Equal(t, http.StatusForbidden, dismiss(durand, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.org") }))
	v := url.Values{"csrf": {"forged"}, "empreinte": {durand}}
	assert.Equal(t, http.StatusForbidden,
		e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(v), formType, withCookie(cookie)).Code)
	assert.Equal(t, http.StatusForbidden,
		e.do(t, http.MethodPost, adminHost, "/anomalies/masquer", formBody(v), formType).Code, "session required")
	assert.Equal(t, http.StatusSeeOther, e.do(t, http.MethodGet, adminHost, "/anomalies", nil).Code, "session required")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/anomalies", nil).Code)
}
