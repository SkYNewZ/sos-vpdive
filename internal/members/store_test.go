package members

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	store *Store
	db    *sql.DB
	path  string
	keys  *secure.Keys
	clock *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), store.FileName)
	db, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{3}, 32))
	require.NoError(t, err)
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	return &fixture{store: NewStore(db, keys, c.now), db: db, path: path, keys: keys, clock: c}
}

func (f *fixture) export(t *testing.T, name string) *Export {
	t.Helper()
	exp, err := Parse(readFixture(t, name), paris(t))
	require.NoError(t, err)
	return exp
}

func (f *fixture) importFixture(t *testing.T, name string) {
	t.Helper()
	p, err := f.store.NewPreview(context.Background(), "alice", f.export(t, name))
	require.NoError(t, err)
	require.NoError(t, f.store.Confirm(context.Background(), p.ID, "alice", true))
}

func TestPreviewOnEmptyList(t *testing.T) {
	f := newFixture(t)
	p, err := f.store.NewPreview(context.Background(), "alice", f.export(t, "members_valid.xlsx"))
	require.NoError(t, err)
	assert.Equal(t, 6, p.Total)
	assert.Equal(t, 6, p.Added)
	assert.Equal(t, 0, p.Removed)
	assert.Equal(t, 0, p.Current)
	assert.Equal(t, 1, p.Skipped)
	assert.Equal(t, 1, p.AmbiguousGroups)
	assert.Equal(t, 2, p.AmbiguousAccounts)
	assert.False(t, p.NeedsSecondConfirm)

	again, err := f.store.Preview(p.ID, "alice")
	require.NoError(t, err)
	assert.Equal(t, p.ID, again.ID)
}

func TestConfirmReplacesListAndJournals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	has, err := f.store.HasList(ctx)
	require.NoError(t, err)
	assert.False(t, has)

	f.importFixture(t, "members_valid.xlsx")

	last, ok, err := f.store.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "alice", last.ImportedBy)
	assert.Equal(t, 6, last.Rows)
	assert.Equal(t, 1, last.Skipped)
	assert.True(t, last.ExportedAt.Equal(time.Date(2026, 9, 1, 8, 15, 0, 0, paris(t))))
	assert.Equal(t, f.clock.t.Truncate(time.Second).UTC(), last.ImportedAt)

	for _, c := range []struct {
		email string
		want  bool
	}{
		{" LEA.martin@example.org ", true},
		{"ines.leroy@example.org", true},
		{"unknown@example.org", false},
	} {
		got, err := f.store.Lookup(ctx, c.email)
		require.NoError(t, err)
		assert.Equal(t, c.want, got, c.email)
	}
	_, err = f.store.Lookup(ctx, "lea martin@example.org")
	require.ErrorIs(t, err, secure.ErrEmailSpace)
	has, err = f.store.HasList(ctx)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestSeasonsKeepAbsentAndEmptyApart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importFixture(t, "members_valid.xlsx")

	var sealed []byte
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT seasons FROM members WHERE email_hash = ?`,
		f.keys.Hash("chloe.petit@example.org")).Scan(&sealed))
	require.NotNil(t, sealed, "empty seasons are stored, encrypted")
	got, err := f.keys.OpenString(sealed)
	require.NoError(t, err)
	assert.Empty(t, got)

	f.importFixture(t, "members_minimal.xlsx")
	var nulls int
	require.NoError(t, f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM members WHERE seasons IS NULL AND licence_expires IS NULL`).Scan(&nulls))
	assert.Equal(t, 2, nulls, "absent columns are stored as NULL")
}

func TestSmallerExportNeedsSecondConfirmation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importFixture(t, "members_valid.xlsx")

	p, err := f.store.NewPreview(ctx, "alice", f.export(t, "members_minimal.xlsx"))
	require.NoError(t, err)
	assert.Equal(t, 2, p.Total)
	assert.Equal(t, 0, p.Added)
	assert.Equal(t, 4, p.Removed)
	assert.Equal(t, 6, p.Current)
	assert.True(t, p.NeedsSecondConfirm)

	err = f.store.Confirm(ctx, p.ID, "alice", false)
	require.ErrorIs(t, err, ErrSecondConfirmRequired)
	err = f.store.Confirm(ctx, p.ID, "alice", true)
	require.NoError(t, err)

	got, err := f.store.Lookup(ctx, "chloe.petit@example.org")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestPreviewExpiresAndBelongsToUploader(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, err := f.store.NewPreview(ctx, "alice", f.export(t, "members_valid.xlsx"))
	require.NoError(t, err)

	err = f.store.Confirm(ctx, p.ID, "bob", true)
	require.ErrorIs(t, err, ErrPreviewNotFound)

	f.clock.t = f.clock.t.Add(16 * time.Minute)
	err = f.store.Confirm(ctx, p.ID, "alice", true)
	require.ErrorIs(t, err, ErrPreviewNotFound)
	_, err = f.store.Preview(p.ID, "alice")
	require.ErrorIs(t, err, ErrPreviewNotFound)
}

func TestConfirmRefusesStalePreview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.store.NewPreview(ctx, "alice", f.export(t, "members_valid.xlsx"))
	require.NoError(t, err)
	b, err := f.store.NewPreview(ctx, "alice", f.export(t, "members_minimal.xlsx"))
	require.NoError(t, err)

	err = f.store.Confirm(ctx, a.ID, "alice", true)
	require.NoError(t, err)
	err = f.store.Confirm(ctx, b.ID, "alice", true)
	require.ErrorIs(t, err, ErrStale)

	got, err := f.store.Lookup(ctx, "ines.leroy@example.org")
	require.NoError(t, err)
	assert.True(t, got, "list from the first import is intact")
}

// A double click confirms once and answers the second click
// with ErrPreviewNotFound.
func TestConcurrentConfirmCreatesOneImport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, err := f.store.NewPreview(ctx, "alice", f.export(t, "members_valid.xlsx"))
	require.NoError(t, err)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			errs[i] = f.store.Confirm(ctx, p.ID, "alice", false)
		})
	}
	wg.Wait()

	okCount := 0
	for _, err := range errs {
		if err == nil {
			okCount++
			continue
		}
		require.ErrorIs(t, err, ErrPreviewNotFound)
	}
	assert.Equal(t, 1, okCount)
	var n int
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM imports`).Scan(&n))
	assert.Equal(t, 1, n)
}

// Spec §13: the database file shows no name, address or other personal data
// in clear, and ignored columns never reach it.
func TestDatabaseHoldsNoPlaintext(t *testing.T) {
	f := newFixture(t)
	f.importFixture(t, "members_valid.xlsx")
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
		"lea.martin@example.org", "hugo.bernard@example.org", "Martin", "Bernard", "Chloé", "Durand",
		"2024, 2025, 2026", "2026-12-31",
		witnessBirthDate, witnessAddress, witnessPhone, witnessContact, witnessComment, witnessCACI,
	} {
		assert.NotContains(t, string(raw), s)
	}
}

func TestPurgeAfterTwelveMonths(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importFixture(t, "members_valid.xlsx")

	f.clock.t = f.clock.t.AddDate(0, 11, 0)
	require.NoError(t, f.store.Purge(ctx))
	has, err := f.store.HasList(ctx)
	require.NoError(t, err)
	assert.True(t, has)

	f.clock.t = f.clock.t.AddDate(0, 2, 0)
	require.NoError(t, f.store.Purge(ctx))
	has, err = f.store.HasList(ctx)
	require.NoError(t, err)
	assert.False(t, has)
}
