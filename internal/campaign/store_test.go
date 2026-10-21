package campaign

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type storeFixture struct {
	store *Store
	db    *sql.DB
	path  string
	keys  *secure.Keys
	clock *clock
}

// newStoreFixture opens a database holding the members of members_valid.xlsx.
func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), store.FileName)
	db, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	c := &clock{t: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)}
	memberstest.Import(t, members.NewStore(db, keys, c.now), "members_valid.xlsx")
	return &storeFixture{store: NewStore(db, keys, c.now), db: db, path: path, keys: keys, clock: c}
}

// export parses a workbook as the import routes do, with the hash of its bytes.
func (f *storeFixture) export(t *testing.T, data []byte) *Export {
	t.Helper()
	rows, err := xlsx.ReadFirstSheet(data, imports.Limits())
	require.NoError(t, err)
	exp, err := Parse(rows, paris(t))
	require.NoError(t, err)
	exp.FileHash = f.keys.Hash(string(data))
	return exp
}

func (f *storeFixture) push(t *testing.T, data []byte) error {
	t.Helper()
	return f.store.Import(context.Background(), f.export(t, data))
}

func (f *storeFixture) validFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(memberstest.FixturePath("campaign_valid.xlsx"))
	require.NoError(t, err)
	return data
}

func (f *storeFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func (f *storeFixture) nameHash(last, first string) []byte {
	return f.keys.Hash(secure.NameKey(last, first))
}

// Design « Stockage »: a push replaces the people in place as « script »; an
// unchanged file and a file under half of the people in place are refused.
func TestImportReplacesThePeople(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	exp := f.export(t, f.validFixture(t))
	require.NoError(t, f.store.Import(ctx, exp))

	last, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, imports.Campaign, last.Kind)
	assert.Equal(t, imports.ScriptAuthor, last.ImportedBy)
	assert.Equal(t, 10, last.Rows)
	assert.Equal(t, 1, last.Skipped)
	assert.True(t, exp.Exported.Equal(last.ExportedAt))
	assert.Equal(t, 10, f.count(t, `SELECT COUNT(*) FROM campaign_lines WHERE import_id = ?`, last.ID))
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM campaign_lines WHERE ambiguous = 1`),
		"Martin Léa and MARTIN Lea, members' homonyms; Inconnu Paul, twice in the export")

	state, _, people, err := f.store.NameLines(ctx, f.nameHash("Petit", "Chloé"))
	require.NoError(t, err)
	assert.Equal(t, payments.BlockLines, state)
	require.Len(t, people, 1)
	assert.Equal(t, "Hugo Bernard", people[0].ValidatedBy)
	assert.Equal(t, payments.Amount(5000), people[0].Membership[0].Amount)

	require.ErrorIs(t, f.store.Import(ctx, exp), imports.ErrUnchanged)
	small := xlsxtest.Build(t, campaignSheet(fixtureExported, enrolled("Bernard", "Hugo", paidMembership, paidLicence, "Tout réglé")))
	require.ErrorIs(t, f.push(t, small), imports.ErrTooFew, "1 person cannot replace 10")
	assert.Equal(t, 10, f.count(t, `SELECT COUNT(*) FROM campaign_lines`))
}

// Design « Stockage », Codex review: a name found twice in the export is
// ambiguous whether the members list holds zero or one person of that name.
func TestNameTwiceInTheExportIsAmbiguous(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	require.NoError(t, f.push(t, xlsxtest.Build(t, campaignSheet(fixtureExported,
		enrolled("Inconnu", "Paul", paidMembership, paidLicence, "Tout réglé"), invited("INCONNU", "Paul"),
		enrolled("Bernard", "Hugo", paidMembership, paidLicence, "Tout réglé"), invited("Bernard", "Hugo"),
		invited("Petit", "Chloé"),
	))))

	for _, name := range [][2]string{{"Inconnu", "Paul"}, {"Bernard", "Hugo"}} {
		assert.Equal(t, 2, f.count(t, `SELECT COUNT(*) FROM campaign_lines WHERE ambiguous = 1 AND name_hash = ?`,
			f.nameHash(name[0], name[1])), name[0])
	}
	state, _, people, err := f.store.NameLines(ctx, f.nameHash("Bernard", "Hugo"))
	require.NoError(t, err)
	assert.Equal(t, payments.BlockAmbiguous, state, "one member, two lines: never attributed")
	assert.Nil(t, people)
	state, _, _, err = f.store.NameLines(ctx, f.nameHash("Petit", "Chloé"))
	require.NoError(t, err)
	assert.Equal(t, payments.BlockLines, state)
}

// Design « Import »: an upload is previewed, then confirmed by its uploader
// with the same homonym marks as a push.
func TestPreviewThenConfirm(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	p, err := f.store.NewPreview(ctx, "alice", f.export(t, f.validFixture(t)))
	require.NoError(t, err)
	assert.Equal(t, 10, p.People)
	assert.Equal(t, 1, p.Skipped)
	assert.Zero(t, p.Current)
	assert.False(t, p.NeedsSecondConfirm)
	got, err := f.store.Preview(p.ID, "alice")
	require.NoError(t, err)
	assert.Same(t, p, got)
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM campaign_lines`), "a preview stores nothing")

	require.ErrorIs(t, f.store.Confirm(ctx, p.ID, "bob", false), imports.ErrPreviewNotFound)
	require.NoError(t, f.store.Confirm(ctx, p.ID, "alice", false))
	last, _, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.Equal(t, "alice", last.ImportedBy)
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM campaign_lines WHERE ambiguous = 1`))

	small, err := f.store.NewPreview(ctx, "alice", f.export(t, xlsxtest.Build(t,
		campaignSheet(fixtureExported, invited("Bernard", "Hugo")))))
	require.NoError(t, err)
	assert.Equal(t, 10, small.Current)
	assert.True(t, small.NeedsSecondConfirm, "the only way past a too_few refusal")
	require.ErrorIs(t, f.store.Confirm(ctx, small.ID, "alice", false), imports.ErrSecondConfirmRequired)
	require.NoError(t, f.store.Confirm(ctx, small.ID, "alice", true))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM campaign_lines`))
}

// Design « Vérification »: the database and its WAL hold no text in clear.
func TestDatabaseHoldsNoPlaintext(t *testing.T) {
	f := newStoreFixture(t)
	require.NoError(t, f.push(t, f.validFixture(t)))
	_, err := f.db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	var raw []byte
	for _, p := range []string{f.path, f.path + "-wal"} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		raw = append(raw, b...)
	}
	for _, s := range []string{"Bernard", "Chloé", "Inconnu", "Licence fédérale", "Virements", "Tout réglé", StatusPending} {
		assert.NotContains(t, string(raw), s)
	}
}

func TestPurgeAfterNinetyDays(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Purge(ctx), "nothing imported yet")
	require.NoError(t, f.push(t, f.validFixture(t)))

	f.clock.t = f.clock.t.AddDate(0, 0, 89)
	require.NoError(t, f.store.Purge(ctx))
	assert.Equal(t, 10, f.count(t, `SELECT COUNT(*) FROM campaign_lines`))

	f.clock.t = f.clock.t.AddDate(0, 0, 2)
	require.NoError(t, f.store.Purge(ctx))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM campaign_lines`))
	_, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "the journal stays")
}

// Design « Effacement »: an erasure deletes the lines of the name hash, a
// homonym's included, and blanks the member as the validator of other
// people, in any case, accent or order; never a namesake with one shared
// part, never with half a name.
func TestEraseBlanksTheMemberAsValidatorOnly(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	validatedBy := func(last, first, by string) person {
		p := enrolled(last, first, paidMembership, paidLicence, "Tout réglé")
		p["Statut"], p["Status de validation"] = StatusValidated, "Adhesion confirmée le 14/10/2026 18:32 par "+by
		return p
	}
	require.NoError(t, f.push(t, xlsxtest.Build(t, campaignSheet(fixtureExported,
		validatedBy("Bernard", "Hugo", "Léa Durand"),
		validatedBy("Petit", "Chloé", "Hugo Bernard"),
		validatedBy("Durand", "Noé", "BERNARD hugo"),
		validatedBy("Martin", "Léa", "Hugo Durand"),
		validatedBy("Roux", "Max", "Hugo Bernard"),
	))))
	hugo := f.nameHash("Bernard", "Hugo")

	n, err := f.store.Count(ctx, hugo)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var erased int
	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		erased, err = f.store.EraseTx(ctx, tx, hugo, "Bernard", "Hugo")
		return err
	}))
	assert.Equal(t, 1, erased)
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM campaign_lines WHERE name_hash = ?`, hugo))

	validators := func() []string {
		t.Helper()
		names := [][2]string{{"Petit", "Chloé"}, {"Durand", "Noé"}, {"Martin", "Léa"}, {"Roux", "Max"}}
		out := make([]string, 0, len(names))
		for _, name := range names {
			rows, err := f.db.QueryContext(ctx, `SELECT data FROM campaign_lines WHERE name_hash = ?`, f.nameHash(name[0], name[1]))
			sealed, err := store.Collect(rows, err, func(rows *sql.Rows) (b []byte, err error) { return b, rows.Scan(&b) })
			require.NoError(t, err)
			require.Len(t, sealed, 1)
			plain, err := f.keys.Open(sealed[0])
			require.NoError(t, err)
			var p Person
			require.NoError(t, json.Unmarshal(plain, &p))
			out = append(out, p.ValidatedBy)
		}
		return out
	}
	assert.Equal(t, []string{"", "", "Hugo Durand", ""}, validators())

	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.store.EraseTx(ctx, tx, f.nameHash("Durand", ""), "Durand", "")
		return err
	}))
	assert.Equal(t, []string{"", "", "Hugo Durand", ""}, validators(), "half a name blanks nothing")
}

// Design « Page Suivi des adhésions »: the page reads every person in file
// order with the « ambiguë » mark, and tells a purge from no import.
func TestOverview(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	o, err := f.store.Overview(ctx)
	require.NoError(t, err)
	assert.False(t, o.Imported)
	assert.False(t, o.Purged)

	require.NoError(t, f.push(t, f.validFixture(t)))
	o, err = f.store.Overview(ctx)
	require.NoError(t, err)
	assert.True(t, o.Imported)
	assert.Equal(t, imports.ScriptAuthor, o.Import.ImportedBy)
	require.Len(t, o.People, 10)
	assert.Equal(t, "Bernard", o.People[0].Line.LastName, "file order")
	assert.False(t, o.People[0].Ambiguous)
	assert.Equal(t, "MARTIN", o.People[4].Line.LastName)
	assert.True(t, o.People[4].Ambiguous, "a member's homonym")

	f.clock.t = f.clock.t.AddDate(0, 0, 91)
	require.NoError(t, f.store.Purge(ctx))
	o, err = f.store.Overview(ctx)
	require.NoError(t, err)
	assert.False(t, o.Imported)
	assert.True(t, o.Purged)
	assert.Empty(t, o.People)
}
