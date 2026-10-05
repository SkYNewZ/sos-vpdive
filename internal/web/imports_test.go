package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
)

var previewPattern = regexp.MustCompile(`name="apercu" value="([^"]+)"`)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(memberstest.FixturePath(name))
	require.NoError(t, err)
	return data
}

func (e *testEnv) upload(t *testing.T, cookie *http.Cookie, csrf string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("csrf", csrf))
	fw, err := mw.CreateFormFile("file", "export.xlsx")
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return e.do(t, http.MethodPost, adminHost, "/imports", &buf,
		func(r *http.Request) { r.Header.Set("Content-Type", mw.FormDataContentType()) }, withCookie(cookie))
}

func (e *testEnv) confirm(t *testing.T, cookie *http.Cookie, csrf, preview string, half bool) *httptest.ResponseRecorder {
	t.Helper()
	v := url.Values{"csrf": {csrf}, "apercu": {preview}}
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
	assert.Equal(t, "/imports?importe=1", done.Header().Get("Location"))

	page := e.do(t, http.MethodGet, adminHost, "/imports?importe=1", nil, withCookie(cookie)).Body.String()
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
