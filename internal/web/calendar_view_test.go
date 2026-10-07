package web

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// page gets a committee page as cookie's session, unescaped.
func (e *testEnv) page(t *testing.T, cookie *http.Cookie, target string) (int, string) {
	t.Helper()
	rec := e.do(t, http.MethodGet, adminHost, target, nil, withCookie(cookie))
	return rec.Code, html.UnescapeString(rec.Body.String())
}

// Review focus: a week across the October change of time keeps seven
// midnights; a month starting on a Saturday needs six weeks.
func TestSpanOf(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, paris) }

	dst := spanOf(viewWeek, day(10, 25))
	require.Len(t, dst.days, 7)
	assert.Equal(t, day(10, 19), dst.from)
	assert.Equal(t, day(10, 26), dst.to)
	for _, d := range dst.days {
		h, m, _ := d.Clock()
		assert.Zero(t, h*60+m, "midnight in Paris: %s", d)
	}
	assert.Equal(t, day(10, 12), dst.prev)

	month := spanOf(viewMonth, day(8, 20))
	assert.Equal(t, day(7, 27), month.days[0])
	assert.Len(t, month.days, 42)
	assert.Equal(t, day(8, 1), month.from)
	assert.Equal(t, day(9, 1), month.to)
	assert.Equal(t, day(7, 1), month.prev)
	assert.Equal(t, day(9, 1), month.next)

	one := spanOf(viewDay, day(9, 22))
	assert.Equal(t, []time.Time{day(9, 22)}, one.days)
	assert.Equal(t, day(9, 21), one.prev)

	late := eventView{Start: day(9, 11).Add(21 * time.Hour), End: day(9, 12)}
	assert.Len(t, onDay([]eventView{late}, day(9, 11)), 1)
	assert.Empty(t, onDay([]eventView{late}, day(9, 12)), "ending at midnight: not on the next day")
}

func TestCalendarViews(t *testing.T) {
	e := newTestEnv(t)
	e.importCalendar(t, "calendar_view.json")
	cookie := e.login(t)

	code, week := e.page(t, cookie, "/calendrier")
	require.Equal(t, http.StatusOK, code)
	for _, want := range []string{
		"Semaine du 31 août au 6 septembre 2026", "sam. 5 sept.",
		"Sortie fin aberrante", `href="/calendrier/evt-cap"`, "1 inscrit sur 12",
		`href="/calendrier?vue=semaine&date=2026-08-24"`, `href="/calendrier?vue=mois&date=2026-09-02"`,
	} {
		assert.Contains(t, week, want)
	}
	assert.NotContains(t, week, "Sortie Porquerolles", "another week")
	titles := regexp.MustCompile(`class="[^"]*\bmin-h-11\b[^"]*" href="/calendrier/evt-cap"`).FindAllString(week, -1)
	assert.Len(t, titles, strings.Count(week, `href="/calendrier/evt-cap"`), "every title link is a 44 px touch target")

	_, month := e.page(t, cookie, "/calendrier?vue=mois&date=2026-05-20")
	assert.Contains(t, month, "mai 2026")
	assert.Contains(t, month, "Annulée · ")
	assert.Contains(t, month, "SORTIE ANNULÉE - Île du Levant")
	assert.NotContains(t, month, "Hugo", "the month reads no participant")

	_, day := e.page(t, cookie, "/calendrier?vue=jour&date=2026-09-22")
	for _, want := range []string{"mardi 22 septembre 2026", "Séjour Corse", "8 h, jusqu'au 24/09", "1 inscrit sur 8 · 2 en attente", "Plongée loisir · Sortie · Mer · Ajaccio"} {
		assert.Contains(t, day, want)
	}

	_, capDay := e.page(t, cookie, "/calendrier?vue=jour&date=2026-09-06")
	assert.Contains(t, capDay, "DP : MARTIN Léa")
	assert.Contains(t, capDay, "Bateaux : Bateau A, Bateau B")

	for _, target := range []string{"/calendrier?vue=annee&date=2026-05-20", "/calendrier?vue=mois&date=hier"} {
		_, odd := e.page(t, cookie, target)
		assert.Contains(t, odd, "Semaine du 31 août au 6 septembre 2026", "%s: an unreadable value gives this week", target)
	}
	_, monthOnly := e.page(t, cookie, "/calendrier?vue=mois")
	assert.Contains(t, monthOnly, `href="/calendrier?vue=jour&date=2026-09-30"`, "no date: today's month")

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/calendrier", nil).Code)
	_, plus := e.page(t, cookie, "/plus")
	assert.Contains(t, plus, `href="/calendrier"`)
}
