package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// validMollie parses vpaydive_valid.xlsx.
func (f *fixture) validMollie(t *testing.T) *MollieExport {
	t.Helper()
	rows, created := readFixture(t, "vpaydive_valid.xlsx")
	exp, err := ParseMollie(rows, created, paris(t))
	require.NoError(t, err)
	return exp
}

func (f *fixture) importMollie(t *testing.T, exp *MollieExport) {
	t.Helper()
	p, err := f.mollie.NewPreview(context.Background(), "alice", exp)
	require.NoError(t, err)
	require.NoError(t, f.mollie.Confirm(context.Background(), p.ID, "alice", true))
}

func TestMolliePreviewAndConfirm(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, ok, err := f.mollie.LastImport(ctx)
	require.NoError(t, err)
	assert.False(t, ok)

	exp := f.validMollie(t)
	exp.FileHash = []byte("file-1")
	p, err := f.mollie.NewPreview(ctx, "alice", exp)
	require.NoError(t, err)
	assert.Equal(t, 16, p.Lines)
	assert.Equal(t, 0, p.Current)
	assert.Equal(t, 1, p.Skipped)
	assert.Equal(t, 3, p.ToCheck)
	assert.Equal(t, map[string]int{"En attente": 1}, p.UnknownSettled)
	assert.True(t, mollieCreated.Equal(p.Created))
	assert.True(t, exp.PeriodFrom.Equal(p.PeriodFrom))
	assert.False(t, p.NeedsSecondConfirm)
	again, err := f.mollie.Preview(p.ID, "alice")
	require.NoError(t, err)
	assert.Same(t, p, again)

	require.NoError(t, f.mollie.Confirm(ctx, p.ID, "alice", false))
	last, ok, err := f.mollie.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, imports.Mollie, last.Kind)
	assert.Equal(t, "alice", last.ImportedBy)
	assert.Equal(t, 16, last.Rows)
	assert.Equal(t, 1, last.Skipped)
	assert.Equal(t, []byte("file-1"), last.FileHash)
	assert.True(t, mollieCreated.Equal(last.ExportedAt))
	assert.True(t, exp.PeriodTo.Equal(last.PeriodTo))
	assert.Equal(t, 16, f.count(t, `SELECT COUNT(*) FROM online_payment_lines WHERE import_id = ?`, last.ID))
	r, err := f.mollie.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, 16, r.Attached.Lines+r.Ambiguous.Lines+r.Unmatched.Lines)

	small := &MollieExport{Lines: exp.Lines[:3]}
	p, err = f.mollie.NewPreview(ctx, "alice", small)
	require.NoError(t, err)
	assert.Equal(t, 16, p.Current)
	assert.True(t, p.NeedsSecondConfirm, "3 lines replace 16")
	require.ErrorIs(t, f.mollie.Confirm(ctx, p.ID, "alice", false), imports.ErrSecondConfirmRequired)
	assert.Equal(t, 16, f.count(t, `SELECT COUNT(*) FROM online_payment_lines`))
	_, ok, err = f.store.LastImport(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "a Mollie import is no payments import")
}

// Spec §7.6: a pushed export replaces the lines without preview, as
// « script »; an unchanged file changes nothing and half a file is refused.
func TestMollieImportPushesWithoutPreview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	exp := f.validMollie(t)
	exp.FileHash = []byte("file-1")
	require.NoError(t, f.mollie.Import(ctx, exp))
	last, ok, err := f.mollie.LastImport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, imports.ScriptAuthor, last.ImportedBy)
	assert.Equal(t, 16, f.count(t, `SELECT COUNT(*) FROM online_payment_lines WHERE import_id = ?`, last.ID))

	require.ErrorIs(t, f.mollie.Import(ctx, exp), imports.ErrUnchanged)
	small := &MollieExport{Lines: exp.Lines[:7], FileHash: []byte("file-2")}
	require.ErrorIs(t, f.mollie.Import(ctx, small), imports.ErrTooFew, "7 lines would replace 16")
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM imports`))
	half := &MollieExport{Lines: exp.Lines[:8], FileHash: []byte("file-3")}
	require.NoError(t, f.mollie.Import(ctx, half))
	assert.Equal(t, 8, f.count(t, `SELECT COUNT(*) FROM online_payment_lines`))
}

// Spec §13: Mollie lines are attributed like payment lines, ambiguity
// included, and a members reimport keeping one homonym leaves theirs
// unattributed until the next Mollie import.
func TestMollieReportAndAmbiguity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	empty, err := f.mollie.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Report{}, empty)

	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importMollie(t, f.validMollie(t))
	r, err := f.mollie.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Report{
		Attached:  Count{Lines: 12, Payers: 4}, // Bernard, Petit, Durand, Leroy
		Ambiguous: Count{Lines: 2, Payers: 1},  // Martin Léa and MARTIN Lea
		Unmatched: Count{Lines: 2, Payers: 1},  // Inconnu Paul
	}, r)

	memberstest.Import(t, f.members, "members_minimal.xlsx") // Martin Léa and Bernard Hugo only
	r, err = f.mollie.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Report{
		Attached:  Count{Lines: 6, Payers: 1},
		Ambiguous: Count{Lines: 2, Payers: 1},
		Unmatched: Count{Lines: 8, Payers: 4},
	}, r)

	f.importMollie(t, f.validMollie(t))
	r, err = f.mollie.Report(ctx)
	require.NoError(t, err)
	assert.Equal(t, Count{Lines: 8, Payers: 2}, r.Attached, "Martin Léa is no longer ambiguous")
	assert.Zero(t, r.Ambiguous)
}

// Spec §13: after import, no commission, net, transfer, end date, billing or
// API status value exists in the database, and no name in clear.
func TestMollieDatabaseHoldsOnlyTheKeptColumns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importMollie(t, f.validMollie(t))

	rows, err := f.db.QueryContext(ctx, `SELECT data FROM online_payment_lines`)
	sealed, err := store.Collect(rows, err, func(rows *sql.Rows) (b []byte, err error) {
		err = rows.Scan(&b)
		return b, err
	})
	require.NoError(t, err)
	require.Len(t, sealed, 16)
	allowed := []string{"product", "service", "starts", "amount", "settled", "paid_at", "method"}
	for _, b := range sealed {
		plain, err := f.keys.Open(b)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(plain, &fields))
		for k := range fields {
			assert.True(t, slices.Contains(allowed, k), "unexpected key %q", k)
		}
		for _, w := range []string{witnessEnd, witnessBilling, "0.0123", "1.23", "98.77", witnessAPIStatus, witnessTransferAsk, witnessTransferDone, witnessTransfer} {
			assert.NotContains(t, string(plain), w)
		}
	}

	_, err = f.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	var raw strings.Builder
	for _, p := range []string{f.path, f.path + "-wal"} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		raw.Write(b)
	}
	for _, s := range []string{"Bernard", "Hugo", "Inconnu", mollieOuting, mollieCarnet, "Pay by Bank"} {
		assert.NotContains(t, raw.String(), s)
	}
}

func TestMolliePurgeAfterNinetyDays(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.mollie.Purge(ctx), "nothing imported yet")
	f.importMollie(t, f.validMollie(t))
	f.importPayments(t, f.valid(t))

	f.clock.t = f.clock.t.AddDate(0, 0, 89)
	require.NoError(t, f.mollie.Purge(ctx))
	assert.Equal(t, 16, f.count(t, `SELECT COUNT(*) FROM online_payment_lines`))

	f.clock.t = f.clock.t.AddDate(0, 0, 2)
	require.NoError(t, f.mollie.Purge(ctx))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM online_payment_lines`))
	assert.Equal(t, 20, f.count(t, `SELECT COUNT(*) FROM payment_lines`), "payment lines have their own purge")
	_, ok, err := f.mollie.LastImport(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "the journal stays")
}

// Owner decision: an erasure deletes every line of the name hash, the
// homonym's included.
func TestMollieCountAndErase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importMollie(t, f.validMollie(t))
	martin := f.nameHash("Martin", "Léa")

	n, err := f.mollie.Count(ctx, martin)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both homonyms")
	n, err = f.mollie.Count(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, n, "no member, no line")

	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		n, err = f.mollie.EraseTx(ctx, tx, martin)
		return err
	}))
	assert.Equal(t, 2, n)
	assert.Equal(t, 14, f.count(t, `SELECT COUNT(*) FROM online_payment_lines`), "other people keep their lines")
}
