package payments

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

type fixture struct {
	store   *Store
	mollie  *MollieStore
	members *members.Store
	db      *sql.DB
	path    string
	keys    *secure.Keys
	clock   *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), store.FileName)
	db, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{5}, 32))
	require.NoError(t, err)
	c := &clock{t: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)}
	return &fixture{
		store: NewStore(db, keys, c.now), mollie: NewMollieStore(db, keys, c.now), members: members.NewStore(db, keys, c.now),
		db: db, path: path, keys: keys, clock: c,
	}
}

// valid parses payments_valid.xlsx.
func (f *fixture) valid(t *testing.T) *Export {
	t.Helper()
	rows, created := readFixture(t, "payments_valid.xlsx")
	exp, err := Parse(rows, created, paris(t))
	require.NoError(t, err)
	return exp
}

func (f *fixture) importPayments(t *testing.T, exp *Export) {
	t.Helper()
	p, err := f.store.NewPreview(context.Background(), "alice", exp)
	require.NoError(t, err)
	require.NoError(t, f.store.Confirm(context.Background(), p.ID, "alice", true))
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func (f *fixture) nameHash(last, first string) []byte {
	return f.keys.Hash(secure.NameKey(last, first))
}

func TestPreviewAndConfirm(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.False(t, ok)

	exp := f.valid(t)
	p, err := f.store.NewPreview(ctx, "alice", exp)
	require.NoError(t, err)
	assert.Equal(t, 20, p.Lines)
	assert.Equal(t, 0, p.Current)
	assert.Equal(t, 1, p.Skipped)
	assert.Equal(t, map[string]int{"En attente": 1}, p.UnknownStates)
	assert.True(t, fixtureCreated.Equal(p.Created))
	assert.True(t, ShortPeriod(p.PeriodFrom, p.PeriodTo), "January to June is under 12 months")
	assert.False(t, p.NeedsSecondConfirm)
	again, err := f.store.Preview(p.ID, "alice")
	require.NoError(t, err)
	assert.Same(t, p, again)

	require.NoError(t, f.store.Confirm(ctx, p.ID, "alice", false))
	last, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "alice", last.ImportedBy)
	assert.Equal(t, 20, last.Rows)
	assert.Equal(t, 1, last.Skipped)
	assert.True(t, fixtureCreated.Equal(last.ExportedAt))
	assert.True(t, exp.PeriodFrom.Equal(last.PeriodFrom))
	assert.True(t, exp.PeriodTo.Equal(last.PeriodTo))
	assert.Equal(t, 20, f.count(t, `SELECT COUNT(*) FROM payment_lines WHERE import_id = ?`, last.ID))
	assert.Nil(t, last.FileHash, "no file hash given")
	assert.Equal(t, 1, p.ToCheck, "the partial payment")
}

// Spec §7.6: a pushed export replaces the lines without preview, as
// « script », and an unchanged file changes nothing.
func TestImportPushesWithoutPreview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	exp := f.valid(t)
	exp.FileHash = []byte("file-1")
	require.NoError(t, f.store.Import(ctx, exp))
	last, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, imports.ScriptAuthor, last.ImportedBy)
	assert.Equal(t, []byte("file-1"), last.FileHash)
	assert.Equal(t, 20, f.count(t, `SELECT COUNT(*) FROM payment_lines WHERE import_id = ?`, last.ID))

	require.ErrorIs(t, f.store.Import(ctx, exp), imports.ErrUnchanged)
	small := &Export{Lines: exp.Lines[:3], FileHash: []byte("file-2")}
	require.ErrorIs(t, f.store.Import(ctx, small), imports.ErrTooFew)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM imports`))

	again := f.valid(t)
	again.FileHash = []byte("file-3")
	p, err := f.store.NewPreview(ctx, "alice", again)
	require.NoError(t, err)
	require.NoError(t, f.store.Confirm(ctx, p.ID, "alice", false))
	last, _, err = f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("file-3"), last.FileHash, "a confirmed preview journals its file hash")
	assert.Equal(t, 20, f.count(t, `SELECT COUNT(*) FROM payment_lines`))
}

func TestSmallerExportNeedsSecondConfirmationAndStalePreviewsAreRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importPayments(t, f.valid(t))

	small := &Export{Lines: f.valid(t).Lines[:3]}
	p, err := f.store.NewPreview(ctx, "alice", small)
	require.NoError(t, err)
	assert.Equal(t, 20, p.Current)
	assert.True(t, p.NeedsSecondConfirm, "3 lines replace 20")
	require.ErrorIs(t, f.store.Confirm(ctx, p.ID, "alice", false), imports.ErrSecondConfirmRequired)

	other, err := f.store.NewPreview(ctx, "alice", f.valid(t))
	require.NoError(t, err)
	require.NoError(t, f.store.Confirm(ctx, other.ID, "alice", true))
	require.ErrorIs(t, f.store.Confirm(ctx, p.ID, "alice", true), imports.ErrStale)
	assert.Equal(t, 20, f.count(t, `SELECT COUNT(*) FROM payment_lines`), "the lines in place are intact")
}

// Spec §13: payers attached, ambiguous and without member, in lines and
// payers, decided at display time.
func TestReport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	empty, err := f.store.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Report{}, empty)

	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importPayments(t, f.valid(t))
	r, err := f.store.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Report{
		Attached:  Count{Lines: 17, Payers: 4}, // Bernard, Durand, Leroy, Petit
		Ambiguous: Count{Lines: 2, Payers: 1},  // Martin Léa and MARTIN Lea
		Unmatched: Count{Lines: 1, Payers: 1},  // Inconnu Paul
	}, r)
}

// Spec §13, recovery: a members reimport keeping one homonym leaves their
// lines unattributed, and attribution of the others survives the reimport.
// Only a new payments import starts afresh.
func TestMembersReimportKeepsTheMarkUntilTheNextPaymentsImport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importPayments(t, f.valid(t))

	memberstest.Import(t, f.members, "members_minimal.xlsx") // Martin Léa and Bernard Hugo only
	r, err := f.store.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Report{
		Attached:  Count{Lines: 12, Payers: 1},
		Ambiguous: Count{Lines: 2, Payers: 1},
		Unmatched: Count{Lines: 6, Payers: 4},
	}, r)

	f.importPayments(t, f.valid(t))
	r, err = f.store.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Count{Lines: 14, Payers: 2}, r.Attached, "Martin Léa is no longer ambiguous")
	assert.Zero(t, r.Ambiguous)
}

// Spec §13: the database file shows no name in clear, and the ignored
// columns never reach it.
func TestDatabaseHoldsNoPlaintext(t *testing.T) {
	f := newFixture(t)
	f.importPayments(t, f.valid(t))
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
	for _, s := range []string{
		"Bernard", "Hugo", "Durand", "Inconnu", "Porquerolles", carnetTitle, MethodPrepaid, // "vpaydive" is in the schema since lot 7
		fixtureComment, witnessAddress, witnessPostCode, witnessCity, witnessEquipment,
	} {
		assert.NotContains(t, string(raw), s)
	}
}

func TestPurgeAfterNinetyDays(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Purge(ctx), "nothing imported yet")
	f.importPayments(t, f.valid(t))

	f.clock.t = f.clock.t.AddDate(0, 0, 89)
	require.NoError(t, f.store.Purge(ctx))
	assert.Equal(t, 20, f.count(t, `SELECT COUNT(*) FROM payment_lines`))

	f.clock.t = f.clock.t.AddDate(0, 0, 2)
	require.NoError(t, f.store.Purge(ctx))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM payment_lines`))
	_, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "the journal stays")
}

// Owner decision: an erasure deletes every line of the name hash, the
// homonym's included.
func TestCountAndErase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importPayments(t, f.valid(t))
	martin := f.nameHash("Martin", "Léa")

	n, err := f.store.Count(ctx, martin)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both homonyms")
	n, err = f.store.Count(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, n, "no member, no line")

	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		n, err = f.store.EraseTx(ctx, tx, martin)
		return err
	}))
	assert.Equal(t, 2, n)
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM payment_lines WHERE name_hash = ?`, martin))
	assert.Equal(t, 18, f.count(t, `SELECT COUNT(*) FROM payment_lines`), "other people keep their lines")
}
