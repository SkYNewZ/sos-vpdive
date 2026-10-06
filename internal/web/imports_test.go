package web

import (
	"bytes"
	"context"
	"html"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

var previewPattern = regexp.MustCompile(`name="apercu" value="([^"]+)"`)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(memberstest.FixturePath(name))
	require.NoError(t, err)
	return data
}

// upload sends a members export.
func (e *testEnv) upload(t *testing.T, cookie *http.Cookie, csrf string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	return e.uploadAs(t, cookie, csrf, "membres", data)
}

// uploadAs sends an export of kind (membres, paiements), fields in the
// order the page writes them.
func (e *testEnv) uploadAs(t *testing.T, cookie *http.Cookie, csrf, kind string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("csrf", csrf))
	require.NoError(t, mw.WriteField("type", kind))
	fw, err := mw.CreateFormFile("file", "export.xlsx")
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return e.do(t, http.MethodPost, adminHost, "/imports", &buf,
		func(r *http.Request) { r.Header.Set("Content-Type", mw.FormDataContentType()) }, withCookie(cookie))
}

// confirm confirms a members preview.
func (e *testEnv) confirm(t *testing.T, cookie *http.Cookie, csrf, preview string, half bool) *httptest.ResponseRecorder {
	t.Helper()
	return e.confirmAs(t, cookie, csrf, "membres", preview, half)
}

func (e *testEnv) confirmAs(t *testing.T, cookie *http.Cookie, csrf, kind, preview string, half bool) *httptest.ResponseRecorder {
	t.Helper()
	v := url.Values{"csrf": {csrf}, "type": {kind}, "apercu": {preview}}
	if half {
		v.Set("confirmer_moitie", "oui")
	}
	return e.do(t, http.MethodPost, adminHost, "/imports/confirmer", formBody(v), formType, withCookie(cookie))
}

func previewID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := previewPattern.FindStringSubmatch(rec.Body.String())
	require.NotNil(t, m, "no preview in page: %s", rec.Body.String())
	return m[1]
}

func TestImportsPageShowsWhereToFindTheExport(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	rec := e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "https://vpdive.example.org/app/admin/vpdive/%2Ff%2Fuser%2Findex?route=/f/user/index")
	assert.Contains(t, body, "« Télécharger »")
	assert.Contains(t, body, "Aucune liste importée")
	assert.Contains(t, body, `enctype="multipart/form-data"`)

	assert.Contains(t, body, "https://vpdive.example.org/app/admin/vpdive/%2Ff%2Fpayment%2Findex?route=/f/payment/index")
	assert.Contains(t, body, "« Télécharger Excel »")
	assert.Contains(t, body, "Des 24 derniers mois à aujourd'hui")
	assert.Contains(t, body, "Aucun export des paiements importé")
}

func TestImportPreviewThenConfirm(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")

	rec := e.upload(t, cookie, csrf, fixtureBytes(t, "members_valid.xlsx"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	for _, want := range []string{"Aperçu avant remplacement", `<dd class="mb-2 sm:mb-0">6</dd>`, "01/09/2026 à 08:15", "1 groupe, 2 comptes", "Remplacer la liste"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "confirmer_moitie")

	done := e.confirm(t, cookie, csrf, previewID(t, rec), false)
	require.Equal(t, http.StatusSeeOther, done.Code, done.Body.String())
	assert.Equal(t, "/imports?importe=membres", done.Header().Get("Location"))

	page := e.do(t, http.MethodGet, adminHost, "/imports?importe=membres", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, page, "Liste des membres importée.")
	assert.Contains(t, page, "Alice (Présidente)")
	assert.NotContains(t, page, "Le formulaire est fermé")

	ok, err := e.deps.Members.Lookup(context.Background(), "lea.martin@example.org")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestImportRefusalsKeepTheListAndExplain(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	tests := []struct {
		name   string
		data   []byte
		status int
		want   string
	}{
		{"missing Email", fixtureBytes(t, "members_missing_email.xlsx"), http.StatusUnprocessableEntity, "Colonne « Email » introuvable"},
		{"missing Prénom", fixtureBytes(t, "members_missing_first_name.xlsx"), http.StatusUnprocessableEntity, "Colonne obligatoire absente : « Prénom »"},
		{"duplicates", fixtureBytes(t, "members_duplicate_email.xlsx"), http.StatusUnprocessableEntity, "lignes 5, 7"},
		{"space", fixtureBytes(t, "members_email_space.xlsx"), http.StatusUnprocessableEntity, "ligne 6"},
		{"not a workbook", []byte("Nom;Prénom;Email\n"), http.StatusUnprocessableEntity, "pas un classeur Excel"},
		{"empty", nil, http.StatusUnprocessableEntity, "Choisis le fichier"},
		{"too big", bytes.Repeat([]byte{'x'}, maxUploadBytes+1), http.StatusRequestEntityTooLarge, "Fichier trop volumineux"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.upload(t, cookie, csrf, tt.data)
			assert.Equal(t, tt.status, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.want)
		})
	}
	has, err := e.deps.Members.HasList(context.Background())
	require.NoError(t, err)
	assert.False(t, has)
}

func TestImportErrorSitsAfterTheFileField(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	body := e.upload(t, cookie, csrf, fixtureBytes(t, "members_missing_email.xlsx")).Body.String()
	label := strings.Index(body, `for="file"`)
	msg := strings.Index(body, "Colonne « Email » introuvable")
	require.NotEqual(t, -1, label)
	require.NotEqual(t, -1, msg)
	assert.Greater(t, msg, label)
}

func TestImportNeedsSessionAndCSRF(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	assert.Equal(t, http.StatusForbidden, e.upload(t, cookie, "forged", fixtureBytes(t, "members_valid.xlsx")).Code)
	assert.Equal(t, http.StatusForbidden, e.confirm(t, cookie, "forged", "x", false).Code)
	anonymous := &http.Cookie{Name: "__Host-session", Value: "nope"}
	assert.Equal(t, http.StatusForbidden, e.upload(t, anonymous, "x", fixtureBytes(t, "members_valid.xlsx")).Code)
}

func TestImportStalePreviewAndDoubleSubmit(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	a := previewID(t, e.upload(t, cookie, csrf, fixtureBytes(t, "members_valid.xlsx")))
	b := previewID(t, e.upload(t, cookie, csrf, fixtureBytes(t, "members_valid.xlsx")))

	require.Equal(t, http.StatusSeeOther, e.confirm(t, cookie, csrf, a, false).Code)
	again := e.confirm(t, cookie, csrf, a, false)
	assert.Equal(t, http.StatusConflict, again.Code)
	assert.Contains(t, again.Body.String(), "a expiré ou a déjà servi")

	stale := e.confirm(t, cookie, csrf, b, false)
	assert.Equal(t, http.StatusConflict, stale.Code)
	assert.Contains(t, stale.Body.String(), "Un autre import est passé entre-temps")
}

func TestImportBelowHalfNeedsSecondConfirmation(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")

	rec := e.upload(t, cookie, csrf, fixtureBytes(t, "members_minimal.xlsx"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "moins de la moitié des 6 comptes actuels")
	id := previewID(t, rec)

	refused := e.confirm(t, cookie, csrf, id, false)
	assert.Equal(t, http.StatusUnprocessableEntity, refused.Code)
	assert.Contains(t, refused.Body.String(), "Coche la seconde confirmation")
	assert.Equal(t, id, previewID(t, refused), "the same preview is shown again")

	assert.Equal(t, http.StatusSeeOther, e.confirm(t, cookie, csrf, id, true).Code)
}

// Spec §13: logs and spans carry no email, name, token or password.
func TestImportFlowLeaksNothingToLogsOrSpans(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	id := previewID(t, e.upload(t, cookie, csrf, fixtureBytes(t, "members_valid.xlsx")))
	require.Equal(t, http.StatusSeeOther, e.confirm(t, cookie, csrf, id, false).Code)

	ended := spans.Ended()
	names := make([]string, 0, len(ended))
	var dump strings.Builder
	for _, s := range ended {
		names = append(names, s.Name())
		dump.WriteString(s.Name())
		for _, a := range s.Attributes() {
			dump.WriteString(" " + a.Value.String())
		}
	}
	for _, want := range []string{"import.read", "import.validate", "import.replace", "db members.replace", "POST /imports"} {
		assert.Contains(t, names, want)
	}
	logs := e.logs.String()
	for _, secret := range []string{"lea.martin@example.org", "Martin", "Léa", testPassword, cookie.Value, csrf, id} {
		assert.NotContains(t, logs, secret)
		assert.NotContains(t, dump.String(), secret)
	}
}

// paymentsSheet is a small payments export: the required columns only.
func paymentsSheet(t *testing.T, lines int) []byte {
	t.Helper()
	sheet := make(xlsxtest.Sheet, 0, 1+lines)
	sheet = append(sheet, []any{"Nom", "Prénom", "Prix unitaire", "Quantité", "Montant paiement", "Montant réduc.",
		"État", "Produit/Événement", "Créé le"})
	for range lines {
		sheet = append(sheet, []any{"Bernard", "Hugo", 30, 1, 30, 0, "Payé", "Baptême", "05/01/2026 10:12:00"})
	}
	return xlsxtest.Build(t, sheet)
}

func TestPaymentsImportPreviewThenConfirm(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")

	rec := e.uploadAs(t, cookie, csrf, "paiements", fixtureBytes(t, "payments_valid.xlsx"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := html.UnescapeString(rec.Body.String())
	for _, want := range []string{
		"Aperçu avant remplacement", "01/09/2026 à 12:50 (indicatif)", "du 05/01/2026 au 20/06/2026",
		"Moins de 12 mois", `<dd class="mb-2 sm:mb-0">20</dd>`, "« En attente » : 1 ligne", "Remplacer les paiements",
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "confirmer_moitie")
	assert.NotContains(t, body, "Colonnes absentes", "the fixture has every optional column")

	done := e.confirmAs(t, cookie, csrf, "paiements", previewID(t, rec), false)
	require.Equal(t, http.StatusSeeOther, done.Code, done.Body.String())
	assert.Equal(t, "/imports?importe=paiements", done.Header().Get("Location"))

	page := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports?importe=paiements", nil, withCookie(cookie)).Body.String())
	for _, want := range []string{
		"Paiements importés.", "Alice (Présidente)", "du 05/01/2026 au 20/06/2026",
		"Rattachées à un membre</dt><dd class=\"mb-2 sm:mb-0\">17 lignes, 4 payeurs",
		"Ambiguës (homonymes)</dt><dd class=\"mb-2 sm:mb-0\">2 lignes, 1 payeur",
		"Sans membre correspondant</dt><dd class=\"mb-2 sm:mb-0\">1 ligne, 1 payeur",
		"Lignes sans nom écartées</dt><dd class=\"mb-2 sm:mb-0\">1",
	} {
		assert.Contains(t, page, want)
	}
	assert.Equal(t, 20, e.count(t, "payment_lines"))
}

func TestPaymentsImportRefusalsKeepTheLinesAndExplain(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"missing column": {fixtureBytes(t, "payments_missing_column.xlsx"), "Colonne obligatoire absente : « Montant paiement »"},
		"members export": {fixtureBytes(t, "members_valid.xlsx"), "Colonne « Créé le » introuvable"},
		"not a workbook": {[]byte("Nom;Prénom\n"), "Dépose l'export « Télécharger Excel » de la page des paiements"},
	} {
		rec := e.uploadAs(t, cookie, csrf, "paiements", c.data)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		body := html.UnescapeString(rec.Body.String())
		assert.Contains(t, body, c.want, name)
		label, msg := strings.Index(body, `for="file-paiements"`), strings.Index(body, c.want)
		assert.Greater(t, msg, label, "%s: the error sits after the payments file field", name)
	}
	assert.Zero(t, e.count(t, "payment_lines"))

	bad := e.uploadAs(t, cookie, csrf, "autre", paymentsSheet(t, 1))
	assert.Equal(t, http.StatusBadRequest, bad.Code)
}

func TestPaymentsImportBelowHalfNeedsSecondConfirmation(t *testing.T) {
	e := newTestEnv(t)
	e.importPayments(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")

	rec := e.uploadAs(t, cookie, csrf, "paiements", paymentsSheet(t, 3))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, "moins de la moitié des 20 lignes en place")
	// The sheet has the required columns only: spec §7.3 makes the others
	// optional, so the preview names what each absence costs instead of refusing.
	for _, want := range []string{
		"Colonnes absentes", "« Methode de paiement » : toute sortie annulée passera pour réglée en argent réel",
		"« Date paiement »", "second « Materiel » : pas de montant de location",
	} {
		assert.Contains(t, body, want)
	}
	id := previewID(t, rec)

	refused := e.confirmAs(t, cookie, csrf, "paiements", id, false)
	assert.Equal(t, http.StatusUnprocessableEntity, refused.Code)
	assert.Contains(t, html.UnescapeString(refused.Body.String()), "Coche la seconde confirmation : ce fichier contient moins de la moitié des lignes en place.")
	assert.Equal(t, id, previewID(t, refused), "the same preview is shown again")
	assert.Equal(t, http.StatusConflict, e.confirmAs(t, cookie, csrf, "membres", id, true).Code, "a payments preview is not a members one")

	assert.Equal(t, http.StatusSeeOther, e.confirmAs(t, cookie, csrf, "paiements", id, true).Code)
	assert.Equal(t, 3, e.count(t, "payment_lines"))
}

// Spec §7.3: past PAYMENTS_MAX_AGE, a banner asks to import again; nothing
// before the first import.
func TestPaymentsReminderBanner(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	home := func() string {
		t.Helper()
		return html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	}
	assert.NotContains(t, home(), "Les paiements datent")
	e.importPayments(t)
	e.clock.advance(7 * 24 * time.Hour)
	assert.NotContains(t, home(), "Les paiements datent")
	e.clock.advance(2 * time.Hour)
	assert.Contains(t, home(), "Les paiements datent du 02/09/2026. Pense à refaire l'import.")
}

func TestPaymentsImportLeaksNothingToLogsOrSpans(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	id := previewID(t, e.uploadAs(t, cookie, csrf, "paiements", fixtureBytes(t, "payments_valid.xlsx")))
	require.Equal(t, http.StatusSeeOther, e.confirmAs(t, cookie, csrf, "paiements", id, false).Code)

	ended := spans.Ended()
	names := make([]string, 0, len(ended))
	var dump strings.Builder
	for _, s := range ended {
		names = append(names, s.Name())
		dump.WriteString(s.Name())
		for _, a := range s.Attributes() {
			dump.WriteString(" " + a.Value.String())
		}
	}
	for _, want := range []string{"import.read", "import.validate", "import.replace", "db payments.replace"} {
		assert.Contains(t, names, want)
	}
	for _, secret := range []string{"Bernard", "Hugo", "Porquerolles", id} {
		assert.NotContains(t, e.logs.String(), secret)
		assert.NotContains(t, dump.String(), secret)
	}
}
