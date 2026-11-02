package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/ffessm"
)

// fakeRegister answers by lowercased query and counts the calls.
type fakeRegister struct {
	mu    sync.Mutex
	rows  map[string][]ffessm.Licence
	err   error
	calls []string
}

func (f *fakeRegister) Search(_ context.Context, q string) ([]ffessm.Licence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)
	if f.err != nil {
		return nil, f.err
	}
	if len([]rune(q)) < 3 {
		return nil, ffessm.ErrQueryTooShort
	}
	return f.rows[strings.ToLower(q)], nil
}

var (
	goodLicence = ffessm.Licence{Number: "A-13-000001", LastName: "BERNARD", FirstName: "HUGO", BirthDate: "1990-02-03", ValidUntil: "2099-12-31", Active: true, Club: "CLUB DU PRADET"}
	oldLicence  = ffessm.Licence{Number: "A-13-000002", LastName: "BERNARD", FirstName: "HUGO", BirthDate: "1990-02-03", ValidUntil: "2000-01-01", Active: true}
)

func registerEnv(t *testing.T, rows map[string][]ffessm.Licence) (*testEnv, *fakeRegister) {
	t.Helper()
	f := &fakeRegister{rows: rows}
	return newTestEnv(t, func(d *Deps) { d.Licences = f }), f
}

func TestLicenceSearchPage(t *testing.T) {
	e, _ := registerEnv(t, map[string][]ffessm.Licence{
		"hugo":    {goodLicence},
		"vieux":   {oldLicence},
		"homonym": {goodLicence, {LastName: "BERNARD", FirstName: "HUGO", BirthDate: "1980-01-01", ValidUntil: "2000-01-01", Active: true}},
	})
	cookie := e.login(t)
	get := func(q string) string {
		rec := e.postAs(t, cookie, "/licences", url.Values{"csrf": {e.csrf(t, cookie, "/licences")}, "q": {q}})
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}
	body := get("hugo")
	assert.Contains(t, body, "Licence valide")
	assert.Contains(t, body, "31/12/2099")
	for _, shown := range []string{"BERNARD Hugo", "03/02/1990", "CLUB DU PRADET"} {
		assert.Contains(t, body, shown, "homonyms are many: every line says who it is")
	}
	assert.NotContains(t, body, "A-13-000001")
	assert.Contains(t, get("vieux"), "Licence non valide")
	assert.Contains(t, get("vieux"), "01/01/2000")
	homonym := get("homonym")
	for _, shown := range []string{"BERNARD Hugo", "03/02/1990", "01/01/1980", "CLUB DU PRADET"} {
		assert.Contains(t, homonym, shown, "homonyms are told apart")
	}
	assert.Contains(t, get("inconnu"), "Aucune licence trouvée")
	assert.Contains(t, get("ab"), "au moins 3 caractères")
}

func TestLicenceName(t *testing.T) {
	assert.Equal(t, "LEMAIRE Quentin", licenceName("Lemaire", "QUENTIN"))
	assert.Equal(t, "LEMAIRE BLONDEL Marie-Christine", licenceName("lemaire blondel", "MARIE-CHRISTINE"))
	assert.Equal(t, "LEMAIRE Jean Clément", licenceName("LEMAIRE", "jean CLÉMENT"))
}

func TestLicenceSearchOffWithoutRegister(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, "/licences", nil, withCookie(cookie)).Code)
}

func TestLicenceTile(t *testing.T) {
	lea := goodLicence
	lea.LastName, lea.FirstName = "MARTIN", "LEA" // the requester, as newSubmission types her
	e, f := registerEnv(t, map[string][]ffessm.Licence{"hugo.bernard@example.org": {lea}, "famille@example.org": {goodLicence}})
	cookie := e.login(t)
	other := e.submitTicket(t, "famille@example.org")
	otherFrag := e.do(t, http.MethodGet, adminHost, "/demandes/"+strconv.FormatInt(other.ID, 10)+"/licence", nil, withCookie(cookie)).Body.String()
	assert.NotContains(t, otherFrag, "A-13-000001", "a relative's licence on a shared address")
	assert.Contains(t, otherFrag, "Aucune licence à ce nom")
	assert.Contains(t, otherFrag, `name="csrf"`)
	f.calls = nil
	tk := e.submitTicket(t, "hugo.bernard@example.org")
	path := "/demandes/" + strconv.FormatInt(tk.ID, 10)
	frag := e.do(t, http.MethodGet, adminHost, path+"/licence", nil, withCookie(cookie)).Body.String()
	assert.Contains(t, frag, "A-13-000001")
	assert.Contains(t, frag, "31/12/2099")
	assert.Contains(t, e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie)).Body.String(), `data-licence="`+path+`/licence"`)
	assert.Len(t, f.calls, 1)

	f.err = http.ErrAbortHandler
	e.srv.licenceMemo = licenceCache{}
	assert.Contains(t, e.do(t, http.MethodGet, adminHost, path+"/licence", nil, withCookie(cookie)).Body.String(), "indisponible")
}

func TestLicenceTileOffWithoutRegister(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "hugo.bernard@example.org")
	path := "/demandes/" + strconv.FormatInt(tk.ID, 10)
	assert.NotContains(t, e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie)).Body.String(), "data-licence")
}

func TestLicenceCache(t *testing.T) {
	e, f := registerEnv(t, map[string][]ffessm.Licence{"hugo": {goodLicence}})
	ctx := context.Background()
	for range 2 {
		_, err := e.srv.licenceRows(ctx, "  Hugo ")
		require.NoError(t, err)
	}
	assert.Len(t, f.calls, 1, "second answer comes from memory")
	e.clock.advance(2 * time.Hour)
	_, err := e.srv.licenceRows(ctx, "hugo")
	require.NoError(t, err)
	assert.Len(t, f.calls, 2, "expired after an hour")
	f.err = http.ErrAbortHandler
	_, err = e.srv.licenceRows(ctx, "zzz")
	require.Error(t, err)
	f.err = nil
	_, err = e.srv.licenceRows(ctx, "zzz")
	require.NoError(t, err)
	assert.Len(t, f.calls, 4, "errors are not cached")
}

func TestToolLicenceStatus(t *testing.T) {
	e, tb := toolEnv(t)
	e.srv.licences = &fakeRegister{rows: map[string][]ffessm.Licence{
		"hugo.bernard@example.org": {goodLicence},
		"a-13-000002":              {oldLicence},
	}}
	tb.call(t, "find_member", `{"query":"hugo"}`)
	got, step := tb.call(t, "licence_status", `{"ref":"m1"}`)
	assert.Equal(t, "valide", got["etat"])
	assert.Equal(t, "31/12/2099", got["valable_jusqu_au"])
	assert.Equal(t, "Licence FFESSM de Hugo Bernard lue", step)
	assert.Len(t, got, 2, "verdict and date only")

	got, _ = tb.call(t, "licence_status", `{"numero":"A-13-000002"}`)
	assert.Equal(t, "non valide", got["etat"])
	got, _ = tb.call(t, "licence_status", `{"numero":"A-13-999999"}`)
	assert.Equal(t, "introuvable", got["etat"])
	got, _ = tb.call(t, "licence_status", `{"numero":"Hugo Bernard"}`)
	assert.Contains(t, got["erreur"], "numéro de licence attendu", "no name search through the number")
	assert.Equal(t, "Registre FFESSM", tb.sources[len(tb.sources)-1].Label)
	got, _ = tb.call(t, "licence_status", `{}`)
	assert.Contains(t, got["erreur"], "un seul des deux")

	names := func() []string {
		defs := tb.tools().Defs
		out := make([]string, 0, len(defs))
		for _, d := range defs {
			out = append(out, d.Name)
		}
		return out
	}
	assert.Contains(t, names(), "licence_status")
	e.srv.licences = nil
	assert.NotContains(t, names(), "licence_status")
	got, _ = tb.call(t, "licence_status", `{"numero":"A-13-000002"}`)
	assert.Contains(t, got["erreur"], "non configuré")
}

func TestLicenceValidLastDay(t *testing.T) {
	e := newTestEnv(t)
	today := e.srv.now().In(e.srv.paris)
	assert.True(t, e.srv.licenceValid(licenceRow{Active: true, Until: today}))
	assert.False(t, e.srv.licenceValid(licenceRow{Active: true, Until: today.AddDate(0, 0, -1)}))
	assert.False(t, e.srv.licenceValid(licenceRow{Until: today}), "the register calls it inactive")
	assert.False(t, e.srv.licenceValid(licenceRow{Active: true}), "no last day")
}

func TestLicenceOfChecksTheName(t *testing.T) {
	e, f := registerEnv(t, map[string][]ffessm.Licence{"famille@example.org": {goodLicence}})
	ctx := context.Background()
	rows, err := e.srv.licenceOf(ctx, "famille@example.org", "Hugo Bernard")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	rows, err = e.srv.licenceOf(ctx, "famille@example.org", "Léa Bernard")
	require.NoError(t, err)
	assert.Empty(t, rows, "a relative's licence on a shared address is not the requester's")
	again, err := e.srv.licenceRows(ctx, "famille@example.org")
	require.NoError(t, err)
	assert.Len(t, again, 1, "the cached answer stays whole")
	rows, err = e.srv.licenceOf(ctx, "", "Hugo Bernard")
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Len(t, f.calls, 1, "no address, no search")
}

func TestLicenceCacheEmptied(t *testing.T) {
	e, _ := registerEnv(t, map[string][]ffessm.Licence{"hugo": {goodLicence}})
	_, err := e.srv.licenceRows(context.Background(), "hugo")
	require.NoError(t, err)
	e.srv.licenceMemo.sweep(e.srv.now())
	assert.Len(t, e.srv.licenceMemo.m, 1, "still fresh")
	e.srv.licenceMemo.sweep(e.srv.now().Add(licenceTTL))
	assert.Empty(t, e.srv.licenceMemo.m, "swept without a new search")

	_, err = e.srv.licenceRows(context.Background(), "hugo")
	require.NoError(t, err)
	cookie := e.login(t)
	rec := e.postAs(t, cookie, "/effacement", url.Values{"csrf": {e.csrf(t, cookie, "/effacement")}, "email": {"hugo@example.org"}, "etape": {stepConfirm}})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, e.srv.licenceMemo.m, "an erasure forgets the answers")
}
