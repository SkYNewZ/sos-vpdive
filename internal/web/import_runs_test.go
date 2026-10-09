package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
)

func TestBuildJournal(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	// Tuesday 27 October 2026, 9:00 in Paris: summer time ended on Sunday 25.
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, paris) }
	runs := []imports.Run{ // the latest first, as imports.Runs returns them
		{Result: imports.Failed, At: at(27, 6), By: "script", Code: "sign_in_capped", Detail: "sign-in skipped: daily attempt limit reached"},
		{Result: imports.Unchanged, At: at(26, 19), By: "script", Rows: 610},
		{Result: imports.Imported, At: at(26, 13), By: "script", Rows: 612, Skipped: 1},
		{Result: imports.Refused, At: at(26, 6), By: "script", Code: "too_few", Detail: "Ce fichier contient moins de la moitié des données en place."},
		{Result: imports.Imported, At: at(25, 6), By: "alice", Rows: 1},
		{Result: imports.Failed, At: at(20, 6), By: "script", Code: "mystery"},
		{Result: imports.Imported, At: time.Date(2026, 9, 27, 22, 30, 0, 0, time.UTC), By: "script", Rows: 999}, // 0:30 on 28 September
	}
	j := buildJournal(imports.Members, runs, now, paris)

	assert.Equal(t, 7, j.Runs)
	assert.Equal(t, 3, j.Failures)
	require.Len(t, j.Recent, journalRecent)
	assert.Equal(t, []string{
		"Échec : connexion à VPDive plafonnée, une tentative par 24 h (sign-in skipped: daily attempt limit reached)",
		"Fichier identique au précédent",
		"Importé : 612 comptes",
		"Refusé : Ce fichier contient moins de la moitié des données en place.",
		"Importé : 1 compte",
	}, []string{j.Recent[0].Outcome, j.Recent[1].Outcome, j.Recent[2].Outcome, j.Recent[3].Outcome, j.Recent[4].Outcome})
	assert.Equal(t, "Échec : mystery", outcome(runs[5], runUnits[imports.Members]), "an unknown class shows as reported")

	ch := j.Chart
	assert.Equal(t, "1000", ch.Top)
	assert.Equal(t, "Comptes", ch.Heading)
	require.Len(t, ch.Columns, journalDays)
	assert.Equal(t, "lun. 28 sept.", ch.Columns[0].Label)
	assert.Equal(t, "mar. 27 oct.", ch.Columns[journalDays-1].Label, "today closes the days")
	day := func(label string) runColumn {
		for _, c := range ch.Columns {
			if c.Label == label {
				return c
			}
		}
		t.Fatalf("no column %q", label)
		return runColumn{}
	}
	first := day("lun. 28 sept.")
	assert.Equal(t, 1, first.Runs, "a run at 0:30 Paris time belongs to its Paris day")
	assert.Equal(t, 999, first.Rows)
	monday := day("lun. 26 oct.")
	assert.Equal(t, 3, monday.Runs)
	assert.Equal(t, 610, monday.Rows, "the day's last successful run sets the column; an unchanged file keeps its count")
	assert.Equal(t, 1, monday.Failures)
	assert.Contains(t, monday.Bar, " 96V", "the bar stops above the failure mark")
	assert.Contains(t, first.Bar, " 100V", "a day without failure reaches the axis")
	today := day("mar. 27 oct.")
	assert.Equal(t, 1, today.Failures)
	assert.Zero(t, today.Rows)
	assert.Empty(t, today.Bar, "no bar without a success")
	assert.Equal(t, (journalDays-1)*slotWidth, today.X)
	assert.Equal(t, "27 oct.", today.Tick)
	assert.Empty(t, day("ven. 23 oct.").Tick)
	assert.Zero(t, day("sam. 24 oct.").Runs)
}

func TestBuildJournalEmpty(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	j := buildJournal(imports.Carnets, nil, time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC), paris)
	assert.Zero(t, j.Runs)
	assert.Empty(t, j.Recent)
	assert.Equal(t, "1", j.Chart.Top)
	assert.Equal(t, "cartes", j.Chart.Many)
}
