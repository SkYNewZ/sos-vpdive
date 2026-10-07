package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// calendarWith is the calendar fixture keeping the events of ids only.
func calendarWith(t *testing.T, ids ...string) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(fixtureBytes(t, "calendar_valid.json"), &doc))
	var kept []any
	for _, ev := range doc["events"].([]any) {
		if slices.Contains(ids, ev.(map[string]any)["id"].(string)) {
			kept = append(kept, ev)
		}
	}
	doc["events"] = kept
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	return data
}

// Lot 8: the script pushes the calendar like an export; events past the
// 12-month retention are counted as skipped, and the same bytes again change
// nothing.
func TestPushedCalendarImportsThenUnchanged(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	data := fixtureBytes(t, "calendar_valid.json")
	rec := e.push(t, "calendar", data, importToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, pushAnswer{Result: "imported", Read: 4, Kept: 3, Skipped: 1}, answer(t, rec))
	assert.Equal(t, 3, e.count(t, "calendar_events"))
	assert.Equal(t, 5, e.count(t, "calendar_participants"))
	var by string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT imported_by FROM imports WHERE kind = 'calendar'`).Scan(&by))
	assert.Equal(t, "script", by)

	again := e.push(t, "calendar", data, importToken)
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, pushAnswer{Result: "unchanged", Read: 4, Kept: 3, Skipped: 1}, answer(t, again))
	assert.Equal(t, 1, e.count(t, "imports"))

	fewer := e.push(t, "calendar", calendarWith(t, "evt-0002", "evt-0003"), importToken)
	require.Equal(t, http.StatusOK, fewer.Code, "2 of 3 in the window is not under half")
	assert.Equal(t, 2, e.count(t, "calendar_events"), "the event missing from the push went")
}

// Names, titles and places are sealed, and no log line carries them.
func TestPushedCalendarLeaksNothing(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	require.Equal(t, http.StatusOK, e.push(t, "calendar", fixtureBytes(t, "calendar_valid.json"), importToken).Code)
	for _, table := range []string{"calendar_events", "calendar_participants"} {
		rows, err := e.db.QueryContext(context.Background(), "SELECT data FROM "+table)
		blobs, err := store.Collect(rows, err, func(rows *sql.Rows) (b []byte, err error) {
			err = rows.Scan(&b)
			return b, err
		})
		require.NoError(t, err)
		for _, b := range blobs {
			for _, secret := range []string{"Martin", "Garnier", "Sortie", "Piscine"} {
				assert.False(t, bytes.Contains(b, []byte(secret)), "%s holds %q in clear", table, secret)
			}
		}
	}
	for _, secret := range []string{"Martin", "Garnier", "Sortie", "evt-0002"} {
		assert.NotContains(t, e.logs.String(), secret)
	}
}

// A refused calendar keeps the events in place and mails the committee,
// without the « upload it by hand » advice: there is no manual upload.
func TestPushedCalendarRefusals(t *testing.T) {
	e := newTestEnv(t, withImportToken)
	require.Equal(t, http.StatusOK, e.push(t, "calendar", fixtureBytes(t, "calendar_valid.json"), importToken).Code)

	broken := e.push(t, "calendar", []byte(`{"from": "2026-06-04"`), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, broken.Code)
	assert.Equal(t, pushAnswer{Error: "invalid_calendar", Message: "Ce calendrier n'est pas un JSON lisible."}, answer(t, broken))

	few := e.push(t, "calendar", calendarWith(t, "evt-0004"), importToken)
	assert.Equal(t, http.StatusUnprocessableEntity, few.Code)
	a := answer(t, few)
	assert.Equal(t, "too_few", a.Error)
	assert.Contains(t, a.Message, "moins de la moitié des événements")
	assert.Equal(t, 3, e.count(t, "calendar_events"), "the events in place stay")

	mails := e.clubMails(t)
	require.Len(t, mails, 2)
	for _, m := range mails {
		assert.Equal(t, "Import automatique refusé : calendrier", m.Subject)
		assert.NotContains(t, m.Text, "à la main")
		assert.Contains(t, m.Text, "script d'import est peut-être en panne")
	}
}
