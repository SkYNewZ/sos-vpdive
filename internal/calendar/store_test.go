package calendar

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	store *Store
	db    *sql.DB
	keys  *secure.Keys
	clock *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	c := &clock{t: testNow}
	return &fixture{store: NewStore(db, keys, c.now), db: db, keys: keys, clock: c}
}

// push imports d as the pushed route does: parsed at the fixture's clock,
// with the hash of its bytes.
func (f *fixture) push(t *testing.T, d doc) error {
	t.Helper()
	data := d.bytes(t)
	exp, err := Parse(data, paris(t), f.clock.now())
	require.NoError(t, err)
	exp.FileHash = f.keys.Hash(string(data))
	return f.store.Import(context.Background(), exp)
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

// events returns the stored event ids, in start order.
func (f *fixture) events(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(), `SELECT id FROM calendar_events ORDER BY starts_at, id`)
	ids, err := store.Collect(rows, err, func(rows *sql.Rows) (id string, err error) {
		err = rows.Scan(&id)
		return id, err
	})
	require.NoError(t, err)
	return ids
}

// stored decrypts the event data of id.
func (f *fixture) stored(t *testing.T, id string) Event {
	t.Helper()
	var sealed []byte
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT data FROM calendar_events WHERE id = ?`, id).Scan(&sealed))
	plain, err := f.keys.Open(sealed)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(plain, &fields))
	assert.NotContains(t, fields, "participants", "participants live in their own rows")
	assert.NotContains(t, fields, "unregistrations", "unregistrations live in their own rows")
	var ev Event
	require.NoError(t, json.Unmarshal(plain, &ev))
	return ev
}

// people decrypts the participants of event id, in insertion order.
func (f *fixture) people(t *testing.T, id string) []Participant {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(), `SELECT data FROM calendar_participants WHERE event_id = ? ORDER BY id`, id)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (p Participant, err error) {
		var sealed []byte
		if err = rows.Scan(&sealed); err != nil {
			return p, err
		}
		plain, err := f.keys.Open(sealed)
		if err != nil {
			return p, err
		}
		return p, json.Unmarshal(plain, &p)
	})
	require.NoError(t, err)
	return out
}

// unregs decrypts the unregistrations of event id, in insertion order, as
// "Last First by By".
func (f *fixture) unregs(t *testing.T, id string) []string {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT data FROM calendar_unregistrations WHERE event_id = ? ORDER BY id`, id)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (string, error) {
		var sealed []byte
		if err := rows.Scan(&sealed); err != nil {
			return "", err
		}
		u, err := f.store.openUnregistration(sealed)
		return u.LastName + " " + u.FirstName + " by " + u.By, err
	})
	require.NoError(t, err)
	return out
}

func TestImportStoresEventsAndParticipants(t *testing.T) {
	f := newFixture(t)
	nameless := registered(505, "Leroy", "")
	require.NoError(t, f.push(t, window(
		event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa"), unregistered(303, "Paul Garnier"), nameless),
		event(t, "evt-b", "2026-11-15T08:00:00+01:00", unregistered(101, "MARTIN Léa")),
	)))

	var (
		kind, by           string
		from, to, rowCount int64
	)
	require.NoError(t, f.db.QueryRowContext(context.Background(),
		`SELECT kind, imported_by, period_from, period_to, row_count FROM imports`).Scan(&kind, &by, &from, &to, &rowCount))
	assert.Equal(t, "calendar", kind)
	assert.Equal(t, imports.ScriptAuthor, by)
	assert.Equal(t, time.Date(2026, 6, 4, 0, 0, 0, 0, paris(t)).Unix(), from)
	assert.Equal(t, time.Date(2027, 9, 2, 0, 0, 0, 0, paris(t)).Unix(), to)
	assert.Equal(t, int64(2), rowCount)

	assert.Equal(t, []string{"evt-a", "evt-b"}, f.events(t))
	assert.Equal(t, "Sortie evt-a", f.stored(t, "evt-a").Title)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM calendar_participants WHERE name_hash = ?`,
		f.keys.Hash(secure.NameKey("Martin", "Léa"))), "registered with both names: matched by name")
	assert.Equal(t, 3, f.count(t, `SELECT COUNT(*) FROM calendar_participants WHERE name_hash IS NULL`),
		"unregistered ones and an empty first name: never matched by name")
	assert.Equal(t, 2, f.count(t, `SELECT COUNT(*) FROM calendar_participants WHERE person_hash = ?`, f.store.personHash(101)),
		"the same person across events")
	assert.Equal(t, []Participant{registered(101, "Martin", "Léa"), unregistered(303, "Paul Garnier"), nameless}, f.people(t, "evt-a"))
}

// Review focus: each pushed event is stored or updated by id with its new
// participants; only the events that start in the window and are missing
// from the push go.
func TestImportUpsertsAndDeletesOnlyInTheWindow(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.push(t, doc{From: "2026-03-01", To: "2027-09-02", Events: []Event{
		event(t, "evt-old", "2026-05-01T09:00:00+02:00"),
		event(t, "evt-overlap", "2026-06-03T22:00:00+02:00"),
		event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa")),
		event(t, "evt-b", "2026-11-15T08:00:00+01:00"),
	}}))

	overlap := event(t, "evt-overlap", "2026-06-03T22:00:00+02:00")
	overlap.Title = "Plongée de nuit, retour tardif"
	require.NoError(t, f.push(t, window(overlap, event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(202, "Bernard", "Hugo")))))

	assert.Equal(t, []string{"evt-old", "evt-overlap", "evt-a"}, f.events(t), "evt-b went, the history stayed")
	assert.Equal(t, "Plongée de nuit, retour tardif", f.stored(t, "evt-overlap").Title, "an event overlapping from is updated")
	assert.Equal(t, []Participant{registered(202, "Bernard", "Hugo")}, f.people(t, "evt-a"), "the old participants are gone")
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM calendar_participants`))
}

func TestImportUnchanged(t *testing.T) {
	f := newFixture(t)
	d := window(event(t, "evt-a", "2026-10-11T08:00:00+02:00"))
	require.NoError(t, f.push(t, d))
	require.ErrorIs(t, f.push(t, d), imports.ErrUnchanged)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM imports`))
}

// The « under half » guard counts the window only: the history does not
// make a complete push look short.
func TestImportTooFewCountsTheWindowOnly(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.push(t, doc{From: "2026-01-01", To: "2027-09-02", Events: []Event{
		event(t, "evt-h1", "2026-02-01T09:00:00+01:00"), event(t, "evt-h2", "2026-03-01T09:00:00+01:00"),
		event(t, "evt-h3", "2026-04-01T09:00:00+02:00"), event(t, "evt-h4", "2026-05-01T09:00:00+02:00"),
		event(t, "evt-a", "2026-10-11T08:00:00+02:00"), event(t, "evt-b", "2026-11-15T08:00:00+01:00"),
	}}))
	require.NoError(t, f.push(t, window(event(t, "evt-a", "2026-10-11T08:00:00+02:00"))), "1 of 2 in the window is not under half")

	require.NoError(t, f.push(t, window(
		event(t, "evt-a", "2026-10-11T08:00:00+02:00"), event(t, "evt-c", "2026-12-01T08:00:00+01:00"),
		event(t, "evt-d", "2027-01-10T08:00:00+01:00"), event(t, "evt-e", "2027-02-10T08:00:00+01:00"),
	)))
	require.ErrorIs(t, f.push(t, window(event(t, "evt-a", "2026-10-11T08:00:00+02:00"))), imports.ErrTooFew)
	assert.Len(t, f.events(t), 8, "a refused push changes nothing")
}

// Review focus: a bootstrap push after the purge does not bring back what
// is older than 12 months.
func TestImportSkipsEventsPastRetention(t *testing.T) {
	f := newFixture(t)
	bootstrap := doc{From: "2024-09-02", To: "2027-09-02", Events: []Event{
		event(t, "evt-old", "2025-06-14T09:00:00+02:00", registered(101, "Martin", "Léa")),
		event(t, "evt-new", "2026-08-29T08:00:00+02:00"),
	}}
	require.NoError(t, f.push(t, bootstrap))
	assert.Equal(t, []string{"evt-new"}, f.events(t))
	assert.Equal(t, 1, f.count(t, `SELECT skipped_count FROM imports`))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM calendar_participants`))

	require.NoError(t, f.store.Purge(context.Background()))
	bootstrap.Events[1].Title = "Sortie renommée"
	require.NoError(t, f.push(t, bootstrap))
	assert.Equal(t, []string{"evt-new"}, f.events(t))
}

func TestPurgeDeletesEventsPastRetention(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.push(t, doc{From: "2025-09-03", To: "2027-09-02", Events: []Event{
		event(t, "evt-a", "2025-10-01T09:00:00+02:00", registered(101, "Martin", "Léa")),
		event(t, "evt-b", "2026-10-01T09:00:00+02:00", registered(101, "Martin", "Léa")),
	}}))
	require.NoError(t, f.store.Purge(context.Background()))
	assert.Len(t, f.events(t), 2, "both within 12 months")

	f.clock.t = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	require.NoError(t, f.store.Purge(context.Background()))
	assert.Equal(t, []string{"evt-b"}, f.events(t))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM calendar_participants`), "participants go with their event")
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM imports`), "the journal stays")
}

// A push replaces an event's unregistrations; they go with their event,
// deleted by a push or purged.
func TestImportStoresUnregistrations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa"))
	a.Unregistrations = []Unregistration{unreg("BERNARD", "Hugo", "2026-10-01T18:42:00+02:00", "Alice DUPONT")}
	b := event(t, "evt-b", "2026-11-15T08:00:00+01:00")
	require.NoError(t, f.push(t, window(a, b)))
	assert.Equal(t, []string{"BERNARD Hugo by Alice DUPONT"}, f.unregs(t, "evt-a"))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM calendar_unregistrations WHERE name_hash = ?`,
		f.keys.Hash(secure.NameKey("Bernard", "Hugo"))), "keyed like a member's name")
	assert.Empty(t, f.stored(t, "evt-a").Unregistrations)

	a.Unregistrations = []Unregistration{unreg("Petit", "Chloé", "2026-10-02T09:00:00+02:00", "")}
	require.NoError(t, f.push(t, window(a, b)))
	assert.Equal(t, []string{"Petit Chloé by "}, f.unregs(t, "evt-a"), "a push replaces them")

	require.NoError(t, f.push(t, window(b)), "evt-a deleted in VPDive")
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM calendar_unregistrations`), "gone with their event")

	b.Unregistrations = []Unregistration{unreg("Petit", "Chloé", "2026-11-02T09:00:00+01:00", "")}
	require.NoError(t, f.push(t, window(b)))
	f.clock.t = time.Date(2027, 11, 16, 10, 0, 0, 0, time.UTC)
	require.NoError(t, f.store.Purge(ctx))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM calendar_unregistrations`), "purged with their event")
}
