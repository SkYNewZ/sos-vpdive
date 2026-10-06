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
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// fileHash reads the file hash journaled by the latest import of kind.
func (e *testEnv) fileHash(t *testing.T, kind string) []byte {
	t.Helper()
	var h []byte
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT file_hash FROM imports WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind).Scan(&h))
	return h
}

func TestImportsPageShowsWhereToFindTheVPayDiveExport(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	body := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie)).Body.String())
	for _, want := range []string{
		`id="encaissements-titre"`, "Encaissements Mollie (export VPayDive)",
		"https://vpdive.example.org/app/admin/vpdive/%2Ff%2Fmollie%2Findex?route=/f/mollie/index",
		"« Exporter (Excel) »", "Aucun export VPayDive importé",
	} {
		assert.Contains(t, body, want)
	}
}

// Spec §7.5: the VPayDive export follows the rules of §7.2, preview then
// confirmation, and its report gives the lines to check (§7.7).
func TestMollieImportPreviewThenConfirm(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	data := fixtureBytes(t, "vpaydive_valid.xlsx")

	rec := e.uploadAs(t, cookie, csrf, "mollie", data)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := html.UnescapeString(rec.Body.String())
	for _, want := range []string{
		"Aperçu avant remplacement", "01/09/2026 à 18:24 (indicatif)", "du 02/04/2026 au 12/08/2026",
		"Lignes dans le fichier</dt><dd class=\"mb-2 sm:mb-0\">16",
		"Lignes à vérifier</dt><dd class=\"mb-2 sm:mb-0\">3",
		"« En attente » : 1 ligne", "Remplacer les encaissements",
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "Moins de 12 mois", "VPayDive started in 2026: no short-period warning")

	done := e.confirmAs(t, cookie, csrf, "mollie", previewID(t, rec), false)
	require.Equal(t, http.StatusSeeOther, done.Code, done.Body.String())
	assert.Equal(t, "/imports?importe=mollie", done.Header().Get("Location"))

	page := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports?importe=mollie", nil, withCookie(cookie)).Body.String())
	for _, want := range []string{
		"Encaissements Mollie importés.", "du 02/04/2026 au 12/08/2026",
		"Rattachées à un membre</dt><dd class=\"mb-2 sm:mb-0\">12 lignes, 4 payeurs",
		"Ambiguës (homonymes)</dt><dd class=\"mb-2 sm:mb-0\">2 lignes, 1 payeur",
		"Sans membre correspondant</dt><dd class=\"mb-2 sm:mb-0\">2 lignes, 1 payeur",
		"Lignes à vérifier</dt><dd class=\"mb-2 sm:mb-0\">3",
	} {
		assert.Contains(t, page, want)
	}
	assert.Equal(t, 16, e.count(t, "online_payment_lines"))
	assert.Equal(t, e.deps.Keys.Hash(string(data)), e.fileHash(t, "vpaydive"), "the upload journals the file hash")
}

func TestMollieImportRefusalsKeepTheLinesAndExplain(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"missing column":  {fixtureBytes(t, "vpaydive_missing_column.xlsx"), "Colonne obligatoire absente : « Payé »"},
		"payments export": {fixtureBytes(t, "payments_valid.xlsx"), "Colonne « Montant Panier » introuvable"},
		"not a workbook":  {[]byte("Nom;Prénom\n"), "Dépose l'export « Exporter (Excel) » de la page VPayDive"},
	} {
		rec := e.uploadAs(t, cookie, csrf, "mollie", c.data)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		body := html.UnescapeString(rec.Body.String())
		assert.Contains(t, body, c.want, name)
		label, msg := strings.Index(body, `for="file-mollie"`), strings.Index(body, c.want)
		assert.Greater(t, msg, label, "%s: the error sits after the Mollie file field", name)
	}
	assert.Zero(t, e.count(t, "online_payment_lines"))
}

// Spec §7.7: every import report gives its lines to check; every upload
// journals the hash of its file (spec §7.6, one code path).
func TestPaymentsAndMembersUploadsReportTheirChecksAndHashes(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	for kind, fixture := range map[string]string{"membres": "members_valid.xlsx", "paiements": "payments_valid.xlsx"} {
		data := fixtureBytes(t, fixture)
		rec := e.uploadAs(t, cookie, csrf, kind, data)
		require.Equal(t, http.StatusOK, rec.Code, kind)
		require.Equal(t, http.StatusSeeOther, e.confirmAs(t, cookie, csrf, kind, previewID(t, rec), true).Code, kind)
		journal := map[string]string{"membres": "members", "paiements": "payments"}[kind]
		assert.Equal(t, e.deps.Keys.Hash(string(data)), e.fileHash(t, journal), kind)
	}
	page := html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie)).Body.String())
	assert.Contains(t, page, "Lignes à vérifier</dt><dd class=\"mb-2 sm:mb-0\">1", "the partial payment")
}

// Spec §7.5: past VPAYDIVE_MAX_AGE, a banner asks to import again.
func TestMollieReminderBanner(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	home := func() string {
		t.Helper()
		return html.UnescapeString(e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie)).Body.String())
	}
	assert.NotContains(t, home(), "Les encaissements Mollie datent")
	e.importMollie(t)
	e.clock.advance(7 * 24 * time.Hour)
	assert.NotContains(t, home(), "Les encaissements Mollie datent")
	e.clock.advance(2 * time.Hour)
	assert.Contains(t, home(), "Les encaissements Mollie datent du 02/09/2026. Pense à refaire l'import.")
}

func TestMollieImportLeaksNothingToLogsOrSpans(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	id := previewID(t, e.uploadAs(t, cookie, csrf, "mollie", fixtureBytes(t, "vpaydive_valid.xlsx")))
	require.Equal(t, http.StatusSeeOther, e.confirmAs(t, cookie, csrf, "mollie", id, false).Code)

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
	for _, want := range []string{"import.read", "import.validate", "import.replace", "db mollie.replace"} {
		assert.Contains(t, names, want)
	}
	for _, secret := range []string{"Bernard", "Hugo", "Porquerolles", id} {
		assert.NotContains(t, e.logs.String(), secret)
		assert.NotContains(t, dump.String(), secret)
	}
}
