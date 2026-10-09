package carnets

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
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
	keys, err := secure.NewKeys(bytes.Repeat([]byte{6}, 32))
	require.NoError(t, err)
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	memberstest.Import(t, members.NewStore(db, keys, c.now), "members_valid.xlsx")
	return &storeFixture{store: NewStore(db, keys, c.now), db: db, path: path, keys: keys, clock: c}
}

// push imports data as the pushed route does: parsed, with the hash of its
// bytes, its holders resolved.
func (f *storeFixture) push(t *testing.T, data []byte) error {
	t.Helper()
	exp, err := Parse(data, paris(t))
	require.NoError(t, err)
	exp.FileHash = f.keys.Hash(string(data))
	require.NoError(t, f.store.Resolve(context.Background(), exp))
	return f.store.Import(context.Background(), exp)
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

// Design §2-3: the cards of known holders replace those in place, as
// « script »; an unchanged body and a list under half are refused.
func TestImportStoresTheCardsOfKnownHolders(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	data := fixture(t)
	exp, err := Parse(data, paris(t))
	require.NoError(t, err)
	exp.FileHash = f.keys.Hash(string(data))
	require.NoError(t, f.store.Resolve(ctx, exp))
	assert.Equal(t, 2, exp.ToCheck, "INCONNU Paul and LEROY")
	require.Len(t, exp.Cards, 4)
	require.NoError(t, f.store.Import(ctx, exp))

	last, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, imports.ScriptAuthor, last.ImportedBy)
	assert.Equal(t, 4, last.Rows)
	assert.Equal(t, 1, last.Skipped)
	assert.True(t, exp.From.Equal(last.PeriodFrom))
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM carnets WHERE import_id = ?`, last.ID))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM carnets WHERE ambiguous = 1`), "Léa Martin has a homonym")

	require.ErrorIs(t, f.store.Import(ctx, exp), imports.ErrUnchanged)
	require.ErrorIs(t, f.push(t, pushOf(t, cart("BERNARD Hugo", "-75", added()))), imports.ErrTooFew, "1 card cannot replace 4")
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM carnets`))
}

// Design « Vérification »: the database and its WAL hold no text in clear.
func TestDatabaseHoldsNoPlaintext(t *testing.T) {
	f := newStoreFixture(t)
	require.NoError(t, f.push(t, fixture(t)))
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
	for _, s := range []string{"BERNARD", "Hugo", "Épave", "Carte 10 plongées", "COMITE", "trésorier", "prépaye"} {
		assert.NotContains(t, string(raw), s)
	}
}

func TestPurgeAfterNinetyDays(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Purge(ctx), "nothing imported yet")
	require.NoError(t, f.push(t, fixture(t)))

	f.clock.t = f.clock.t.AddDate(0, 0, 89)
	require.NoError(t, f.store.Purge(ctx))
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM carnets`))

	f.clock.t = f.clock.t.AddDate(0, 0, 2)
	require.NoError(t, f.store.Purge(ctx))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM carnets`))
	_, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "the journal stays")
}

// Design §3, Review Focus 4: an erasure deletes the cards of the name hash,
// a homonym's included, and blanks the member as the author of other cards'
// lines, in any case, accent or order; never a namesake with one shared part.
func TestEraseBlanksTheMemberAsAuthorOnly(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	by := func(author string) map[string]any {
		l := line("prépaye", "2026-06-03T10:00:00+02:00", "prépaye Sortie Épave (14/06/2026) -25€")
		l["by"] = author
		return l
	}
	require.NoError(t, f.push(t, pushOf(t,
		cart("BERNARD Hugo", "-75", by("Léa MARTIN"), by("martin léa"), by("MARTIN Lea"), by("Léa DURAND"), by("Hugo BERNARD"), added()),
		cart("MARTIN Léa", "-50", added()),
	)))
	lea := f.nameHash("Martin", "Léa")

	n, err := f.store.Count(ctx, lea)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	none, err := f.store.Count(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, none, "a person outside the members list reaches nothing")

	var erased int
	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		erased, err = f.store.EraseTx(ctx, tx, lea, "Martin", "Léa")
		return err
	}))
	assert.Equal(t, 1, erased)
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM carnets WHERE name_hash = ?`, lea))

	_, _, cards, err := f.store.NameLines(ctx, f.nameHash("Bernard", "Hugo"))
	require.NoError(t, err)
	require.Len(t, cards, 1)
	authors := make([]string, len(cards[0].Entries))
	for i, e := range cards[0].Entries {
		authors[i] = e.By
	}
	assert.Equal(t, []string{"", "", "", "Léa DURAND", "Hugo BERNARD", "Hugo BERNARD"}, authors)

	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.store.EraseTx(ctx, tx, f.nameHash("Leroy", ""), "Leroy", "")
		return err
	}), "half a name blanks nothing")
}
