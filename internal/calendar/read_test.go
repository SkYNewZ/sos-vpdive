package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

func ids(events []Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.ID
	}
	return out
}

func TestEventEndsAndCancellation(t *testing.T) {
	start := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	ev := Event{Start: start, End: start.Add(-2 * time.Hour)}
	assert.True(t, ev.EndInverted())
	assert.Equal(t, start, ev.Until(), "an end before the start reads as the start")
	ev.End = start.Add(3 * time.Hour)
	assert.False(t, ev.EndInverted())
	assert.Equal(t, start.Add(3*time.Hour), ev.Until())

	for title, want := range map[string]bool{
		"SORTIE ANNULÉE - Île du Levant":   true,
		"Plongée de nuit (annulée, météo)": true,
		"Annulation : sortie reportée":     true,
		"Sortie épave":                     false,
	} {
		assert.Equal(t, want, Event{Title: title}.Cancelled(), title)
	}
}

// An event that ends at midnight or starts at the next one belongs to its
// own day only.
func TestRangeOverlapsTheSpan(t *testing.T) {
	f := newFixture(t)
	multi := event(t, "evt-multi", "2026-10-10T18:00:00+02:00", registered(101, "Martin", "Léa"))
	multi.EndsAt = "2026-10-12T12:00:00+02:00"
	inverted := event(t, "evt-inverted", "2026-10-11T14:00:00+02:00")
	inverted.EndsAt = "2026-10-11T10:00:00+02:00"
	eve := event(t, "evt-eve", "2026-10-10T21:00:00+02:00")
	eve.EndsAt = "2026-10-11T00:00:00+02:00"
	require.NoError(t, f.push(t, window(
		event(t, "evt-before", "2026-10-10T08:00:00+02:00"),
		multi, eve,
		event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(202, "Bernard", "Hugo"), unregistered(303, "Paul Garnier")),
		inverted,
		event(t, "evt-after", "2026-10-12T00:00:00+02:00"),
	)))
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, paris(t))
	ctx := context.Background()

	events, err := f.store.Range(ctx, day, day.AddDate(0, 0, 1), false)
	require.NoError(t, err)
	assert.Equal(t, []string{"evt-multi", "evt-a", "evt-inverted"}, ids(events),
		"overlapping, by start; ending or starting at a midnight stays on its day")
	assert.Nil(t, events[1].Participants, "no participant unless asked")
	assert.True(t, events[2].EndInverted())
	assert.Equal(t, time.Date(2026, 10, 12, 12, 0, 0, 0, paris(t)).Unix(), events[0].Until().Unix())
	assert.Equal(t, "Sortie evt-a", events[1].Title)

	events, err = f.store.Range(ctx, day, day.AddDate(0, 0, 1), true)
	require.NoError(t, err)
	assert.Equal(t, []Participant{registered(101, "Martin", "Léa")}, events[0].Participants)
	assert.Equal(t, []Participant{registered(202, "Bernard", "Hugo"), unregistered(303, "Paul Garnier")}, events[1].Participants)
	assert.Empty(t, events[2].Participants)
}

func TestEventMatchesParticipantsWithMembers(t *testing.T) {
	f := newFixture(t)
	memberstest.Import(t, members.NewStore(f.db, f.keys, f.clock.now), "members_valid.xlsx")
	require.NoError(t, f.push(t, window(
		event(t, "evt-a", "2026-10-11T08:00:00+02:00",
			registered(101, "Bernard", "Hugo"), registered(202, "Martin", "Léa"), registered(303, "Petit", "")),
		event(t, "evt-b", "2026-11-15T08:00:00+01:00",
			unregistered(101, "BERNARD Hugo"), unregistered(404, "Paul Garnier"), unregistered(505, "Chloé Petit")),
		event(t, "evt-c", "2026-12-06T08:00:00+01:00", registered(505, "Petit", "Chloé")),
		event(t, "evt-d", "2026-12-13T08:00:00+01:00", registered(505, "Durand", "Noé")),
	)))
	ctx := context.Background()
	membersOf := func(ev Event) map[string]int {
		out := map[string]int{}
		for _, p := range ev.Participants {
			out[p.Name] = p.Members
		}
		return out
	}

	ev, err := f.store.Event(ctx, "evt-a")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 11, 8, 0, 0, 0, paris(t)).Unix(), ev.Start.Unix())
	assert.Equal(t, "UTC", ev.Start.Location().String(), "read times are UTC: a caller converts to Paris, or fails on every machine")
	assert.Equal(t, "UTC", ev.End.Location().String())
	assert.Equal(t, map[string]int{"Bernard Hugo": 1, "Martin Léa": 2, "Petit ": 0}, membersOf(ev),
		"own name hash; homonyms count 2; an empty first name never matches")

	ev, err = f.store.Event(ctx, "evt-b")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"BERNARD Hugo": 1, "Paul Garnier": 0, "Chloé Petit": 0}, membersOf(ev),
		"linked through a registration; never by name; two registered names: no match")

	_, err = f.store.Event(ctx, "evt-missing")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, ErrNotFound, err, "the rollback does not wrap it")
}

func TestParticipationsFollowTheNameAndTheLinkedPerson(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.push(t, window(
		event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa"), registered(202, "Bernard", "Hugo")),
		event(t, "evt-b", "2026-11-15T08:00:00+01:00", unregistered(101, "MARTIN Léa")),
		event(t, "evt-c", "2026-12-06T08:00:00+01:00", unregistered(707, "Léa Martin")),
		event(t, "evt-d", "2027-01-10T08:00:00+01:00", registered(505, "Petit", "Chloé")),
		event(t, "evt-e", "2027-02-14T08:00:00+01:00", registered(505, "Durand", "Noé")),
		event(t, "evt-f", "2027-03-21T08:00:00+01:00", unregistered(505, "Chloé Petit")),
	)))
	ctx := context.Background()
	seenFor := func(last, first string) []string {
		t.Helper()
		got, err := f.store.Participations(ctx, f.keys.Hash(secure.NameKey(last, first)))
		require.NoError(t, err)
		seen := make([]string, 0, len(got))
		for _, p := range got {
			seen = append(seen, p.Event.ID+":"+p.Participant.Name)
		}
		return seen
	}

	assert.Equal(t, []string{"evt-b:MARTIN Léa", "evt-a:Martin Léa"}, seenFor("Martin", "Léa"),
		"newest first, the linked pilot included; another account of the same name never")
	assert.Equal(t, []string{"evt-e:Durand Noé"}, seenFor("Durand", "Noé"),
		"an account registered under two names follows neither: not the other name's row, not its unregistered one")
	assert.Equal(t, []string{"evt-d:Petit Chloé"}, seenFor("Petit", "Chloé"))

	got, err := f.store.Participations(ctx, f.keys.Hash(secure.NameKey("Martin", "Léa")))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 11, 15, 8, 0, 0, 0, paris(t)).Unix(), got[0].Event.Start.Unix())

	got, err = f.store.Participations(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, got, "no member, no participation")
}

// A calendar read never waits for a writer: it takes no write lock.
func TestReadsDoNotTakeTheWriteLock(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.push(t, window(event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa")))))
	ctx := context.Background()
	writer, err := f.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE: holds the write lock
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Rollback()) })
	_, err = writer.ExecContext(ctx, `DELETE FROM calendar_participants`)
	require.NoError(t, err)

	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, paris(t))
	events, err := f.store.Range(short, day, day.AddDate(0, 0, 1), true)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Len(t, events[0].Participants, 1, "the read sees the last commit, not the writer's pending delete")
	_, err = f.store.Event(short, "evt-a")
	require.NoError(t, err)
}

// Lot 8 part 3: a person's unregistrations join their participation of the
// same event, or stand alone; each event's are oldest first, time read back.
func TestParticipationsCarryUnregistrations(t *testing.T) {
	f := newFixture(t)
	a := event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa"))
	a.Unregistrations = []Unregistration{
		unreg("MARTIN", "Léa", "2026-10-03T09:00:00+02:00", "Paul GARNIER"),
		unreg("Martin", "Léa", "2026-10-01T18:42:00+02:00", ""),
	}
	b := event(t, "evt-b", "2026-11-15T08:00:00+01:00", unregistered(101, "MARTIN Léa"))
	b.Unregistrations = []Unregistration{unreg("MARTIN", "Léa", "2026-11-01T10:00:00+01:00", "Léa MARTIN")}
	c := event(t, "evt-c", "2026-12-06T08:00:00+01:00", registered(202, "Bernard", "Hugo"))
	c.Unregistrations = []Unregistration{
		unreg("Martin", "Léa", "2026-12-01T10:00:00+01:00", "Hugo BERNARD"),
		unreg("Petit", "Chloé", "2026-12-02T10:00:00+01:00", ""),
	}
	require.NoError(t, f.push(t, window(a, b, c)))
	ctx := context.Background()

	type seen struct {
		event   string
		present bool
		left    []string
	}
	read := func(last, first string) []seen {
		t.Helper()
		got, err := f.store.Participations(ctx, f.keys.Hash(secure.NameKey(last, first)))
		require.NoError(t, err)
		out := make([]seen, 0, len(got))
		for _, p := range got {
			s := seen{event: p.Event.ID, present: p.Participant != nil}
			for _, u := range p.Unregistrations {
				s.left = append(s.left, u.Time.In(paris(t)).Format("02/01 15:04")+"|"+u.By)
			}
			out = append(out, s)
		}
		return out
	}

	assert.Equal(t, []seen{
		{event: "evt-c", present: false, left: []string{"01/12 10:00|Hugo BERNARD"}},
		{event: "evt-b", present: true, left: []string{"01/11 10:00|Léa MARTIN"}},
		{event: "evt-a", present: true, left: []string{"01/10 18:42|", "03/10 09:00|Paul GARNIER"}},
	}, read("Martin", "Léa"), "newest event first; joined to the participation of their event, alone otherwise")
	assert.Equal(t, []seen{{event: "evt-c", present: true}}, read("Bernard", "Hugo"),
		"another person's unregistration stays on its own name")
}

func TestUnregistrations(t *testing.T) {
	f := newFixture(t)
	a := event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa"))
	a.Unregistrations = []Unregistration{
		unreg("MARTIN", "Léa", "2026-10-03T09:00:00+02:00", "Paul GARNIER"),
		unreg("Petit", "Chloé", "2026-10-01T18:42:00+02:00", ""),
	}
	require.NoError(t, f.push(t, window(a, event(t, "evt-b", "2026-11-15T08:00:00+01:00"))))
	ctx := context.Background()

	got, err := f.store.Unregistrations(ctx, "evt-a")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "Petit", got[0].LastName, "oldest first, whatever the pushed order")
	assert.Equal(t, time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC), got[1].Time.UTC(), "time read back")
	assert.Equal(t, "Paul GARNIER", got[1].By)

	for _, id := range []string{"evt-b", "evt-unknown"} {
		got, err = f.store.Unregistrations(ctx, id)
		require.NoError(t, err)
		assert.Empty(t, got, id)
	}
}
