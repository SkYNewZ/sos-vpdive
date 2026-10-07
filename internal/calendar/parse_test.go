package calendar

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	return loc
}

// testNow is the clock of every calendar test.
var testNow = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

// doc is a calendar as the script pushes it.
type doc struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Events []Event `json:"events"`
}

func (d doc) bytes(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(d)
	require.NoError(t, err)
	return data
}

// window is the window the script pushes daily: 90 days back, 12 months ahead.
func window(events ...Event) doc { return doc{From: "2026-06-04", To: "2027-09-02", Events: events} }

// event starts at start (RFC 3339) and lasts 3 hours.
func event(t *testing.T, id, start string, ps ...Participant) Event {
	t.Helper()
	s, err := time.Parse(time.RFC3339, start)
	require.NoError(t, err)
	return Event{ID: id, Title: "Sortie " + id, StartsAt: start, EndsAt: s.Add(3 * time.Hour).Format(time.RFC3339), Participants: ps}
}

func registered(id int64, last, first string) Participant {
	return Participant{VPDiveID: id, Name: last + " " + first, LastName: last, FirstName: first, Registered: true, People: 1}
}

// unregistered is a pilot or payer without registration: a full name only.
func unregistered(id int64, name string) Participant { return Participant{VPDiveID: id, Name: name} }

func TestParseReadsTheContract(t *testing.T) {
	body := `{"from": "2026-06-04", "to": "2027-09-02", "added_later": true, "events": [{
		"id": "evt-0001", "url": "https://club.example/agenda/evt-0001", "title": "Sortie épave",
		"description": "", "starts_at": "2026-10-11T08:00:00+02:00", "ends_at": "2026-10-11T12:00:00+02:00",
		"all_day": false, "category": "diving leisure", "color": "#0505f0", "text_color": "#ffffff",
		"activity": "outing", "environment": "natural sea", "location": "Port du club",
		"max_participants": 24, "boats": ["Bateau A"],
		"participants": [{"vpdive_id": 101, "name": "MARTIN Léa", "last_name": "Martin", "first_name": "Léa",
			"registered": true, "waiting_list": false, "status": "accepted", "people": 1, "guest": false,
			"tariff": "Plongée unitaire", "qualifications": ["N3"],
			"roles": [{"name": "Directeur de plongée", "confirmed": true}, {"name": "Pilote", "boat": "Bateau A", "confirmed": false}],
			"payment": {"status": "partial", "due_cents": 6000, "paid_cents": 4000}}]}]}`
	exp, err := Parse([]byte(body), paris(t), testNow)
	require.NoError(t, err)

	assert.Equal(t, time.Date(2026, 6, 4, 0, 0, 0, 0, paris(t)), exp.From)
	assert.Equal(t, time.Date(2027, 9, 2, 0, 0, 0, 0, paris(t)), exp.To)
	assert.Zero(t, exp.Skipped)
	require.Len(t, exp.Events, 1)
	ev := exp.Events[0]
	assert.Equal(t, "evt-0001", ev.ID)
	assert.Equal(t, "diving leisure", ev.Category)
	assert.Equal(t, 24, *ev.MaxParticipants)
	assert.Equal(t, []string{"Bateau A"}, ev.Boats)
	assert.Equal(t, time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC), ev.start.UTC())
	require.Len(t, ev.Participants, 1)
	p := ev.Participants[0]
	assert.Equal(t, int64(101), p.VPDiveID)
	assert.True(t, p.Registered)
	assert.Equal(t, []Role{{Name: "Directeur de plongée", Confirmed: true}, {Name: "Pilote", Boat: "Bateau A"}}, p.Roles)
	assert.Equal(t, &Payment{Status: "partial", DueCents: 6000, PaidCents: 4000}, p.Payment)

	// A typo in a JSON tag drops a field silently: the body's event and the
	// parsed one, both as generic JSON, must be the same.
	var sent struct {
		Events []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &sent))
	require.Len(t, sent.Events, 1)
	kept, err := json.Marshal(ev)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(kept, &got))
	assert.Equal(t, sent.Events[0], got)
}

func TestParseRefusals(t *testing.T) {
	ok := func() doc {
		return window(event(t, "evt-1", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa")))
	}
	for name, tc := range map[string]struct {
		body  []byte
		kind  ProblemKind
		event string
	}{
		"not JSON":     {[]byte("Nom;Prénom\n"), ProblemUnreadable, ""},
		"not a doc":    {[]byte(`[]`), ProblemUnreadable, ""},
		"wrong type":   {[]byte(`{"from": "2026-06-04", "to": "2027-09-02", "events": [{"id": 7}]}`), ProblemUnreadable, ""},
		"no window":    {[]byte(`{"events": []}`), ProblemWindow, ""},
		"bad from":     {func() []byte { d := ok(); d.From = "Martin Léa"; return d.bytes(t) }(), ProblemWindow, ""},
		"reversed":     {func() []byte { d := ok(); d.From, d.To = d.To, d.From; return d.bytes(t) }(), ProblemWindow, ""},
		"no id":        {func() []byte { d := ok(); d.Events[0].ID = ""; return d.bytes(t) }(), ProblemEventID, ""},
		"repeated id":  {func() []byte { d := ok(); d.Events = append(d.Events, d.Events[0]); return d.bytes(t) }(), ProblemEventID, "evt-1"},
		"bad start":    {func() []byte { d := ok(); d.Events[0].StartsAt = "Martin Léa"; return d.bytes(t) }(), ProblemDates, "evt-1"},
		"missing end":  {func() []byte { d := ok(); d.Events[0].EndsAt = ""; return d.bytes(t) }(), ProblemDates, "evt-1"},
		"no person id": {func() []byte { d := ok(); d.Events[0].Participants[0].VPDiveID = 0; return d.bytes(t) }(), ProblemPerson, "evt-1"},
	} {
		_, err := Parse(tc.body, paris(t), testNow)
		var pe *ParseError
		require.ErrorAs(t, err, &pe, name)
		assert.Equal(t, tc.kind, pe.Kind, name)
		assert.Equal(t, tc.event, pe.Event, name)
		assert.NotContains(t, err.Error(), "Martin", name+": a refusal never quotes the body")
	}
}

// The script keeps an end before the start on purpose: deletions key on the
// start alone. Refusing it would block the whole calendar.
func TestParseKeepsAnEndBeforeTheStart(t *testing.T) {
	d := window(event(t, "evt-1", "2026-10-11T08:00:00+02:00"))
	d.Events[0].EndsAt = "2026-10-11T07:00:00+02:00"
	exp, err := Parse(d.bytes(t), paris(t), testNow)
	require.NoError(t, err)
	require.Len(t, exp.Events, 1)
	assert.True(t, exp.Events[0].end.Before(exp.Events[0].start))
}

// The script's first push covers 24 months: events that started more than
// 12 months ago are counted, never stored, and the window starts at the
// retention cutoff.
func TestParseDropsEventsPastRetention(t *testing.T) {
	d := doc{From: "2024-09-02", To: "2027-09-02", Events: []Event{
		event(t, "evt-old", "2025-06-14T09:00:00+02:00"),
		event(t, "evt-new", "2026-08-29T08:00:00+02:00"),
	}}
	exp, err := Parse(d.bytes(t), paris(t), testNow)
	require.NoError(t, err)
	assert.Equal(t, 1, exp.Skipped)
	require.Len(t, exp.Events, 1)
	assert.Equal(t, "evt-new", exp.Events[0].ID)
	start, _ := exp.window()
	assert.Equal(t, cutoff(testNow), start)
	assert.Equal(t, time.Date(2024, 9, 2, 0, 0, 0, 0, paris(t)), exp.From, "the journal keeps the pushed window")
}

// Review focus: the window ends at the Paris midnight after its last day,
// a DST change included.
func TestWindowEndsAtTheNextParisMidnight(t *testing.T) {
	d := doc{From: "2026-10-01", To: "2026-10-25", Events: []Event{
		event(t, "evt-in", "2026-10-25T23:30:00+01:00"),
		event(t, "evt-out", "2026-10-26T00:30:00+01:00"),
	}}
	exp, err := Parse(d.bytes(t), paris(t), testNow)
	require.NoError(t, err)
	start, end := exp.window()
	assert.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, paris(t)), start)
	assert.Equal(t, time.Date(2026, 10, 26, 0, 0, 0, 0, paris(t)), end)
	assert.Equal(t, 1, exp.inWindow())
}

// A pilot or payer without registration carries a full name and no split one.
func TestParseKeepsAnUnregisteredPerson(t *testing.T) {
	d := window(event(t, "evt-1", "2026-10-11T08:00:00+02:00", unregistered(202, "DURAND Paul")))
	exp, err := Parse(d.bytes(t), paris(t), testNow)
	require.NoError(t, err)
	require.Len(t, exp.Events, 1)
	require.Len(t, exp.Events[0].Participants, 1)
	p := exp.Events[0].Participants[0]
	assert.Equal(t, int64(202), p.VPDiveID)
	assert.False(t, p.Registered)
	assert.Empty(t, p.LastName)
	assert.Empty(t, p.FirstName)
}
