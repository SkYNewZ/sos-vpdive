package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestElapsed(t *testing.T) {
	tests := map[time.Duration]string{
		0:                             "< 1 h",
		59 * time.Minute:              "< 1 h",
		5 * time.Hour:                 "5 h",
		23*time.Hour + 59*time.Minute: "23 h",
		24 * time.Hour:                "1 j",
		12*24*time.Hour + 5*time.Hour: "12 j",
	}
	for d, want := range tests {
		assert.Equal(t, want, elapsed(d), d.String())
	}
}

func TestAgeLevels(t *testing.T) {
	e := newTestEnv(t)
	now := e.clock.now()
	tests := []struct {
		submitted, closed time.Time
		want              ageView
	}{
		{now.Add(-47 * time.Hour), time.Time{}, ageView{"1 j", ageNeutral}},
		{now.Add(-48 * time.Hour), time.Time{}, ageView{"2 j", ageWarn}},
		{now.Add(-167 * time.Hour), time.Time{}, ageView{"6 j", ageWarn}},
		{now.Add(-168 * time.Hour), time.Time{}, ageView{"7 j", ageAlert}},
		// A done request shows its processing time, without color.
		{now.Add(-10 * 24 * time.Hour), now.Add(-24 * time.Hour), ageView{"9 j", ageNeutral}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, e.srv.age(tt.submitted, tt.closed))
	}
}

func TestIsoDate(t *testing.T) {
	assert.Equal(t, "31/12/2026", isoDate("2026-12-31"))
	assert.Empty(t, isoDate(""))
	assert.Equal(t, "bientôt", isoDate("bientôt"))
}

func TestActorName(t *testing.T) {
	e := newTestEnv(t)
	assert.Equal(t, "l'adhérent", e.srv.actorName("member"))
	assert.Equal(t, "automatique", e.srv.actorName("system"))
	assert.Equal(t, "Alice (Présidente)", e.srv.actorName("alice"))
	assert.Equal(t, "gone", e.srv.actorName("gone"))
	assert.Nil(t, e.srv.accountOf(""))
	assert.Equal(t, "Alice", e.srv.accountOf("alice").Name)
}

// An export cell holding a date and no time reads as midnight: it shows as
// a date, never « à 00:00 ».
func TestMidnightShowsAsADate(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	s := &Server{paris: paris}
	assert.Equal(t, "12/08/2026", s.formatTime(time.Date(2026, 8, 12, 0, 0, 0, 0, paris)))
	assert.Equal(t, "12/08/2026 à 00:01", s.formatTime(time.Date(2026, 8, 12, 0, 1, 0, 0, paris)))
}
