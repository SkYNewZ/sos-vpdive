package imports

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type preview struct {
	Meta

	value string
}

func newPreviews() (*Previews[*preview], *clock) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	return NewPreviews[*preview](c.now), c
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

func TestPreviewsBelongToTheirUploaderAndExpire(t *testing.T) {
	ps, c := newPreviews()
	p := &preview{Username: "alice", value: "export"}
	require.NoError(t, ps.Put(p))
	id := p.ID
	assert.Len(t, id, secure.TokenLength)

	got, err := ps.Get(id, "alice")
	require.NoError(t, err)
	assert.Same(t, p, got)
	_, err = ps.Get(id, "bob")
	require.ErrorIs(t, err, ErrPreviewNotFound)
	_, err = ps.Get("unknown", "alice")
	require.ErrorIs(t, err, ErrPreviewNotFound)

	c.t = c.t.Add(16 * time.Minute)
	_, err = ps.Get(id, "alice")
	require.ErrorIs(t, err, ErrPreviewNotFound)
}

func TestTakeIsSingleUseAndAsksTheSecondConfirmation(t *testing.T) {
	ps, _ := newPreviews()
	p := &preview{Username: "alice", NeedsSecondConfirm: true}
	require.NoError(t, ps.Put(p))
	id := p.ID

	_, err := ps.Take(id, "bob", true)
	require.ErrorIs(t, err, ErrPreviewNotFound)
	_, err = ps.Take(id, "alice", false)
	require.ErrorIs(t, err, ErrSecondConfirmRequired)
	got, err := ps.Take(id, "alice", true)
	require.NoError(t, err, "a refused preview stays for a retry")
	assert.Equal(t, id, got.ID)
	_, err = ps.Take(id, "alice", true)
	require.ErrorIs(t, err, ErrPreviewNotFound)
}

func TestPreviewsAreFreedWithoutFurtherAccess(t *testing.T) {
	ps, _ := newPreviews()
	ps.ttl = 20 * time.Millisecond
	require.NoError(t, ps.Put(&preview{Username: "alice"}))
	require.Eventually(t, func() bool {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return len(ps.items) == 0
	}, 2*time.Second, 10*time.Millisecond)
}

func TestReplaceJournalsAndRefusesStalePreviews(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	_, ok, err := Last(ctx, db, Members)
	require.NoError(t, err)
	assert.False(t, ok)

	none, _, err := Last(ctx, db, Members)
	require.NoError(t, err)
	base := none.ID
	assert.Zero(t, base)

	exported := time.Date(2026, 9, 1, 8, 15, 0, 0, time.UTC)
	imported := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	var seen int64
	entry := Entry{Kind: Members, ExportedAt: exported, ImportedAt: imported, ImportedBy: "alice", Rows: 6, Skipped: 1}
	require.NoError(t, Replace(ctx, db, "test.replace", &Meta{Base: base}, entry,
		func(_ context.Context, _ *sql.Tx, importID int64) error {
			seen = importID
			return nil
		}))
	last, ok, err := Last(ctx, db, Members)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, seen, last.ID, "fn gets the journal row id")
	assert.Equal(t, Entry{Kind: Members, ExportedAt: exported, ImportedAt: imported, ImportedBy: "alice", Rows: 6, Skipped: 1}, last.Entry)

	called := false
	err = Replace(ctx, db, "test.replace", &Meta{Base: base}, entry, func(context.Context, *sql.Tx, int64) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ErrStale)
	assert.False(t, called)

	period := Entry{Kind: Payments, PeriodFrom: exported.AddDate(-1, 0, 0), PeriodTo: exported, ImportedAt: imported, ImportedBy: "bob", Rows: 3}
	require.NoError(t, Replace(ctx, db, "test.replace", &Meta{}, period,
		func(context.Context, *sql.Tx, int64) error { return nil }), "kinds are independent")
	pay, ok, err := Last(ctx, db, Payments)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, period, pay.Entry)
	assert.True(t, pay.ExportedAt.IsZero(), "no export date stays zero")
	latest, _, err := Last(ctx, db, Members)
	require.NoError(t, err)
	assert.Equal(t, last.ID, latest.ID, "a payments import leaves members previews fresh")
}

func TestReplaceRollsBackOnError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := Replace(ctx, db, "test.replace", &Meta{}, Entry{Kind: Payments, ImportedAt: time.Now(), ImportedBy: "alice"},
		func(context.Context, *sql.Tx, int64) error { return boom })
	require.ErrorIs(t, err, boom)
	_, ok, err := Last(ctx, db, Payments)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestMarkAmbiguousNeverClears(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	member := func(email, name string) {
		exec(`INSERT INTO members (email_hash, name_hash, first_name, last_name, email) VALUES (?, ?, x'01', x'02', x'03')`,
			[]byte(email), []byte(name))
	}
	exec(`INSERT INTO imports (id, kind, imported_at, imported_by, row_count, skipped_count) VALUES (1, 'payments', 0, 'alice', 3, 0)`)
	for _, name := range []string{"homonym", "homonym", "single"} {
		exec(`INSERT INTO payment_lines (import_id, name_hash, data) VALUES (1, ?, x'00')`, []byte(name))
	}
	member("a", "homonym")
	member("b", "homonym")
	member("c", "single")

	ambiguous := func(name string) bool {
		t.Helper()
		var a bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT MIN(ambiguous) FROM payment_lines WHERE name_hash = ?`, []byte(name)).Scan(&a))
		return a
	}
	require.NoError(t, store.Tx(ctx, db, "test.mark", MarkAmbiguous))
	assert.True(t, ambiguous("homonym"), "every line of the name")
	assert.False(t, ambiguous("single"))

	exec(`DELETE FROM members WHERE email_hash = ?`, []byte("b"))
	require.NoError(t, store.Tx(ctx, db, "test.mark", MarkAmbiguous))
	assert.True(t, ambiguous("homonym"), "a reimport keeping one homonym does not clear the mark")
	assert.False(t, ambiguous("single"))
}
