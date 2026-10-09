package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

const importToken = "test-import-token-of-at-least-32-chars"

func withImportToken(d *Deps) { d.Config.ImportToken = importToken }

// push sends a workbook to the pushed-import route as the script does: no
// cookie, no Origin.
func (e *testEnv) push(t *testing.T, kind string, data []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, adminHost, "/api/imports/"+kind, bytes.NewReader(data), func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	})
}

type pushAnswer struct {
	Result  string `json:"result"`
	Read    int    `json:"read"`
	Kept    int    `json:"kept"`
	Skipped int    `json:"skipped"`
	ToCheck int    `json:"to_check"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

func answer(t *testing.T, rec *httptest.ResponseRecorder) pushAnswer {
	t.Helper()
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var a pushAnswer
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &a), rec.Body.String())
	return a
}

// clubMails returns the mails to the club inbox about a refused pushed import.
func (e *testEnv) clubMails(t *testing.T) []mail.Message {
	t.Helper()
	var out []mail.Message
	for _, m := range e.mails(t) {
		if m.To == clubEmail && strings.Contains(m.Subject, "Import automatique refusé") {
			out = append(out, m)
		}
	}
	return out
}

// Spec §7.6, §13: without IMPORT_TOKEN the route does not exist; with it, a
// wrong token is refused with 401 and logged, the token never.
func TestPushedImportRouteNeedsItsToken(t *testing.T) {
	off := newTestEnv(t)
	assert.Equal(t, http.StatusNotFound, off.push(t, "members", fixtureBytes(t, "members_valid.xlsx"), importToken).Code)

	e := newTestEnv(t, withImportToken)
	data := fixtureBytes(t, "members_valid.xlsx")
	for name, token := range map[string]string{"no token": "", "wrong token": "not-the-" + importToken} {
		rec := e.push(t, "members", data, token)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, "unauthorized", answer(t, rec).Error, name)
	}
	assert.Contains(t, e.logs.String(), `"msg":"import token refused"`)
	assert.NotContains(t, e.logs.String(), importToken)
	assert.NotContains(t, e.logs.String(), "not-the-")
	assert.Zero(t, e.count(t, "members"))
	assert.Empty(t, e.clubMails(t), "a bad token mails nobody")

	unknown := e.push(t, "contacts", data, importToken)
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, "unknown_type", answer(t, unknown).Error)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, publicHost, "/api/imports/members", bytes.NewReader(data),
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+importToken) }).Code, "committee host only")
}

// Spec §7.6: a valid file replaces the list in place without preview, as
// « script », and the answer counts its lines.
func TestPushedImportReplacesAndCounts(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	members := e.push(t, "members", fixtureBytes(t, "members_valid.xlsx"), importToken)
	require.Equal(t, http.StatusOK, members.Code, members.Body.String())
	assert.Equal(t, pushAnswer{Result: "imported", Read: 7, Kept: 6, Skipped: 1}, answer(t, members))

	mollie := e.push(t, "vpaydive", fixtureBytes(t, "vpaydive_valid.xlsx"), importToken)
	require.Equal(t, http.StatusOK, mollie.Code, mollie.Body.String())
	assert.Equal(t, pushAnswer{Result: "imported", Read: 17, Kept: 16, Skipped: 1, ToCheck: 3}, answer(t, mollie))
	payments := e.push(t, "payments", fixtureBytes(t, "payments_valid.xlsx"), importToken)
	require.Equal(t, http.StatusOK, payments.Code, payments.Body.String())
	assert.Equal(t, pushAnswer{Result: "imported", Read: 21, Kept: 20, Skipped: 1, ToCheck: 1}, answer(t, payments))

	var by string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT imported_by FROM imports WHERE kind = 'vpaydive'`).Scan(&by))
	assert.Equal(t, "script", by)
	assert.Equal(t, 16, e.count(t, "online_payment_lines"))
	assert.Equal(t, 20, e.count(t, "payment_lines"))
	assert.Equal(t, 6, e.count(t, "members"))
	report, err := e.deps.Mollie.Report(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, report.Ambiguous.Lines, "the homonyms' lines stay unattributed")
}

// mollieWorkbook builds a VPayDive export of n lines with the required
// columns and « Payé » set to settled, declaring the creation date created.
func mollieWorkbook(t *testing.T, created time.Time, n int, settled string) []byte {
	t.Helper()
	sheet := make(xlsxtest.Sheet, 0, 1+n)
	sheet = append(sheet, []any{"Nom", "Prénom", "Type Panier", "Montant Panier", "Payé", "Date paiement"})
	for range n {
		sheet = append(sheet, []any{"Bernard", "Hugo", "Calendrier", 40, settled, "12/08/2026 14:05"})
	}
	return xlsxtest.BuildCreated(t, created, sheet)
}

// The same content downloaded again has other bytes (its creation date): it
// is imported and refreshes the import date, or the staleness alert would
// fire although the script works.
func TestPushedRedownloadIsImported(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	first := e.push(t, "vpaydive", mollieWorkbook(t, time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC), 3, "Oui"), importToken)
	require.Equal(t, http.StatusOK, first.Code)
	again := e.push(t, "vpaydive", mollieWorkbook(t, time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC), 3, "Oui"), importToken)
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, "imported", answer(t, again).Result)
	assert.Equal(t, 2, e.count(t, "imports"))
}

// RFC 7235: the authentication scheme is case-insensitive.
func TestPushedImportTakesALowerCaseScheme(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	rec := e.do(t, http.MethodPost, adminHost, "/api/imports/vpaydive", bytes.NewReader(mollieWorkbook(t, time.Time{}, 1, "Oui")),
		func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Authorization", "bearer "+importToken) })
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A resolver's preview confirmed after the script pushed the same export is
// stale: it never overwrites the newer import.
func TestPushBetweenPreviewAndConfirm(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	rec := e.uploadAs(t, cookie, csrf, "mollie", fixtureBytes(t, "vpaydive_valid.xlsx"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusOK, e.push(t, "vpaydive", mollieWorkbook(t, time.Time{}, 2, "Oui"), importToken).Code)

	stale := e.confirmAs(t, cookie, csrf, "mollie", previewID(t, rec), false)
	assert.Equal(t, http.StatusConflict, stale.Code)
	assert.Contains(t, html.UnescapeString(stale.Body.String()), "Un autre import est passé entre-temps.")
	assert.Equal(t, 2, e.count(t, "online_payment_lines"), "the pushed lines stay")
}

// Spec §13: a file pushed twice in a row makes one journal entry.
func TestPushedTwiceMakesOneJournalEntry(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	data := fixtureBytes(t, "vpaydive_valid.xlsx")
	require.Equal(t, http.StatusOK, e.push(t, "vpaydive", data, importToken).Code)
	again := e.push(t, "vpaydive", data, importToken)
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, pushAnswer{Result: "unchanged", Read: 17, Kept: 16, Skipped: 1, ToCheck: 3}, answer(t, again))
	assert.Equal(t, 1, e.count(t, "imports"))
}

// mollieSnapshot reads the Mollie lines in place, decrypted, in id order.
func (e *testEnv) mollieSnapshot(t *testing.T) []string {
	t.Helper()
	rows, err := e.db.QueryContext(context.Background(), `SELECT name_hash, ambiguous, data FROM online_payment_lines ORDER BY id`)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (string, error) {
		var hash, sealed []byte
		var ambiguous bool
		if err := rows.Scan(&hash, &ambiguous, &sealed); err != nil {
			return "", err
		}
		plain, err := e.deps.Keys.Open(sealed)
		return string(hash) + "|" + map[bool]string{true: "a", false: "-"}[ambiguous] + "|" + string(plain), err
	})
	require.NoError(t, err)
	return out
}

// Spec §13: the same file gives exactly the same list, uploaded by hand or
// pushed by the route.
func TestPushAndUploadGiveTheSameList(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	data := fixtureBytes(t, "vpaydive_valid.xlsx")
	require.Equal(t, http.StatusOK, e.push(t, "vpaydive", data, importToken).Code)
	pushed := e.mollieSnapshot(t)
	require.Len(t, pushed, 16)

	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	rec := e.uploadAs(t, cookie, csrf, "mollie", data)
	require.Equal(t, http.StatusSeeOther, e.confirmAs(t, cookie, csrf, "mollie", previewID(t, rec), false).Code)
	assert.Equal(t, pushed, e.mollieSnapshot(t))
}

// Spec §7.6, §13: a pushed members list under half of the accounts in place
// is refused, the list stays, and the committee gets a mail; so does any
// refused file.
func TestPushedRefusalsKeepTheDataAndMailTheCommittee(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	e.importMembers(t, "members_valid.xlsx")
	e.importPayments(t)

	half := e.push(t, "members", fixtureBytes(t, "members_minimal.xlsx"), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, half.Code)
	a := answer(t, half)
	assert.Equal(t, "too_few", a.Error)
	assert.Contains(t, a.Message, "moins de la moitié")
	assert.Equal(t, 6, e.count(t, "members"), "the list in place stays")

	broken := e.push(t, "payments", fixtureBytes(t, "payments_missing_column.xlsx"), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, broken.Code)
	assert.Equal(t, pushAnswer{Error: "missing_column", Message: "Colonne obligatoire absente : « Montant paiement »."}, answer(t, broken))
	assert.Equal(t, 20, e.count(t, "payment_lines"))

	garbage := e.push(t, "vpaydive", []byte("Nom;Prénom\n"), importToken)
	assert.Equal(t, "invalid_workbook", answer(t, garbage).Error)

	mails := e.clubMails(t)
	require.Len(t, mails, 3)
	assert.Equal(t, "Import automatique refusé : liste des membres", mails[0].Subject)
	assert.Contains(t, mails[0].Text, "moins de la moitié")
	assert.Contains(t, mails[0].Text, "https://"+adminHost+"/imports")
	assert.Equal(t, "Import automatique refusé : export des paiements", mails[1].Subject)
	assert.Contains(t, mails[1].Text, "« Montant paiement »")
	assert.Equal(t, "Import automatique refusé : export VPayDive", mails[2].Subject)
}

// Spec §7.6: 10 calls an hour, counted before the token; a body over 5 MB
// is refused.
func TestPushedImportLimits(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	big := e.push(t, "members", bytes.Repeat([]byte{'x'}, maxUploadBytes+1), importToken)
	assert.Equal(t, http.StatusRequestEntityTooLarge, big.Code)
	assert.Equal(t, "too_large", answer(t, big).Error)
	refused := e.clubMails(t)
	require.Len(t, refused, 1, "an oversized file is a refused import too")
	assert.Equal(t, "Import automatique refusé : liste des membres", refused[0].Subject)
	assert.Contains(t, refused[0].Text, "5 Mo au plus")
	for range 9 {
		assert.Equal(t, http.StatusUnauthorized, e.push(t, "members", nil, "wrong").Code)
	}
	limited := e.push(t, "members", fixtureBytes(t, "members_valid.xlsx"), importToken)
	assert.Equal(t, http.StatusTooManyRequests, limited.Code)
	assert.Equal(t, "rate_limited", answer(t, limited).Error)
	assert.NotEmpty(t, limited.Header().Get("Retry-After"))
	assert.Zero(t, e.count(t, "members"))
}

// Review focus: a pushed members list that reveals homonyms marks the lines
// already in place, as an upload does; otherwise a later list keeping one
// homonym would attribute the other's lines to them.
func TestPushedMembersListMarksHomonymLines(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	require.Equal(t, http.StatusOK, e.push(t, "vpaydive", fixtureBytes(t, "vpaydive_valid.xlsx"), importToken).Code)
	require.Equal(t, http.StatusOK, e.push(t, "payments", fixtureBytes(t, "payments_valid.xlsx"), importToken).Code)
	require.Equal(t, http.StatusOK, e.push(t, "members", fixtureBytes(t, "members_valid.xlsx"), importToken).Code)
	for _, table := range []string{"online_payment_lines", "payment_lines"} {
		var marked int
		require.NoError(t, e.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table+" WHERE ambiguous = 1").Scan(&marked))
		assert.Equal(t, 2, marked, table)
	}
}

// reportFailure posts a run failure as the script does.
func (e *testEnv) reportFailure(t *testing.T, kind, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, adminHost, "/api/imports/"+kind+"/failure", strings.NewReader(body), func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	})
}

// Design 2026-10-09: every run goes to the journal, the script's failures
// included, and the imports page lists them with the chart of the month.
func TestImportRunsAreJournaled(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/imports")
	done := e.confirm(t, cookie, csrf, previewID(t, e.upload(t, cookie, csrf, fixtureBytes(t, "members_valid.xlsx"))), false)
	require.Equal(t, http.StatusSeeOther, done.Code, done.Body.String())

	e.clock.advance(time.Hour)
	same := e.push(t, "members", fixtureBytes(t, "members_valid.xlsx"), importToken)
	require.Equal(t, http.StatusOK, same.Code, same.Body.String())
	assert.Equal(t, "unchanged", answer(t, same).Result)

	e.clock.advance(time.Hour)
	half := e.push(t, "members", fixtureBytes(t, "members_minimal.xlsx"), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, half.Code)
	assert.Contains(t, half.Body.String(), `"recorded":true`, "the script learns the refusal is journaled")

	e.clock.advance(time.Hour)
	reported := e.reportFailure(t, "members", `{"code":"vpdive_failed","detail":"bridge: HTTP 403"}`, importToken)
	require.Equal(t, http.StatusOK, reported.Code, reported.Body.String())
	assert.JSONEq(t, `{"result":"recorded"}`, reported.Body.String())
	long := e.reportFailure(t, "payments", `{"code":"push_failed","detail":"`+strings.Repeat("é", 300)+`"}`, importToken)
	require.Equal(t, http.StatusOK, long.Code, long.Body.String())

	runs, err := imports.Runs(context.Background(), e.db, imports.Members, time.Time{})
	require.NoError(t, err)
	require.Len(t, runs, 4)
	assert.Equal(t, []imports.Result{imports.Failed, imports.Refused, imports.Unchanged, imports.Imported},
		[]imports.Result{runs[0].Result, runs[1].Result, runs[2].Result, runs[3].Result}, "the latest first")
	assert.Equal(t, imports.Run{Kind: imports.Members, At: e.clock.now(), By: "script", Result: imports.Failed, Code: "vpdive_failed", Detail: "bridge: HTTP 403"}, runs[0])
	assert.Equal(t, "too_few", runs[1].Code)
	assert.Contains(t, runs[1].Detail, "moins de la moitié")
	assert.Equal(t, 6, runs[2].Rows, "an unchanged push keeps the count read")
	assert.Equal(t, imports.Run{Kind: imports.Members, At: e.clock.now().Add(-3 * time.Hour), By: "alice", Result: imports.Imported, Rows: 6, Skipped: 1}, runs[3])
	payments, err := imports.Runs(context.Background(), e.db, imports.Payments, time.Time{})
	require.NoError(t, err)
	require.Len(t, payments, 1)
	assert.Len(t, []rune(payments[0].Detail), imports.MaxDetail, "the detail is cut")
	assert.True(t, strings.HasSuffix(payments[0].Detail, "…"))

	page := e.do(t, http.MethodGet, adminHost, "/imports", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, page, "Derniers passages")
	for _, want := range []string{
		"Échec : lecture de VPDive en échec (bridge: HTTP 403)", "Refusé : Ce fichier contient moins de la moitié",
		"Fichier identique au précédent", "Importé : 6 comptes", "Alice (Présidente)",
		"4 passages ces 30 derniers jours, dont 2 échecs", "Comptes du dernier passage réussi du jour", `class="fill-error"`,
		"status-success", "status-neutral", "status-error",
		"Aucun passage ces 30 derniers jours.", // the calendar and the cards
	} {
		assert.Contains(t, page, want)
	}
	assert.Equal(t, 1, strings.Count(page, "Échec : envoi à l&#39;outil en échec"), "the payments section has its own journal")

	e.clock.advance(imports.RunsRetention + time.Hour)
	require.NoError(t, e.srv.Purge(context.Background()))
	runs, err = imports.Runs(context.Background(), e.db, imports.Members, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, runs, "runs are purged after 90 days")
	assert.Contains(t, e.logs.String(), `"code":"vpdive_failed"`)
	assert.NotContains(t, e.logs.String(), "HTTP 403", "the script's detail stays out of the logs")
}

func TestImportFailureReportIsGuarded(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	assert.Equal(t, http.StatusUnauthorized, e.reportFailure(t, "members", `{"code":"push_failed"}`, "wrong").Code)
	assert.Equal(t, http.StatusNotFound, e.reportFailure(t, "cartes", `{"code":"push_failed"}`, importToken).Code)
	for _, body := range []string{``, `{"code":""}`, `{"code":"Push Failed"}`, `{"code":"` + strings.Repeat("a", 65) + `"}`, `[]`} {
		rec := e.reportFailure(t, "members", body, importToken)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Equal(t, "invalid_failure", answer(t, rec).Error)
	}
	runs, err := imports.Runs(context.Background(), e.db, imports.Members, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, runs)
	assert.Empty(t, e.clubMails(t), "a reported failure mails nobody")

	for range apiImportLimit - 7 {
		require.Equal(t, http.StatusOK, e.reportFailure(t, "members", `{"code":"push_failed"}`, importToken).Code)
	}
	limited := e.reportFailure(t, "members", `{"code":"push_failed"}`, importToken)
	assert.Equal(t, http.StatusTooManyRequests, limited.Code)
	assert.Equal(t, http.StatusOK, e.push(t, "members", fixtureBytes(t, "members_valid.xlsx"), importToken).Code, "its own counter")
}
