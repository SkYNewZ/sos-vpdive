package payments

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
)

func (f *fixture) checks(t *testing.T) (open, masked []Check) {
	t.Helper()
	open, masked, err := NewCheckStore(f.db, f.keys, f.members, f.clock.now).List(context.Background())
	require.NoError(t, err)
	return open, masked
}

// Spec §7.7: the lines of the VPayDive export not settled in VPDive and the
// partial payments of the payments export, oldest first.
func TestChecksListBothSignalsOldestFirst(t *testing.T) {
	f := newFixture(t)
	open, masked := f.checks(t)
	assert.Empty(t, open, "nothing imported")
	assert.Empty(t, masked)

	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importPayments(t, f.valid(t))
	f.importMollie(t, f.validMollie(t))
	loc := paris(t)
	at := func(day, month, hour, minute int) time.Time {
		return time.Date(2026, time.Month(month), day, hour, minute, 0, 0, loc)
	}
	hugo := Person{Name: "Hugo Bernard"}

	open, masked = f.checks(t)
	assert.Empty(t, masked)
	require.Len(t, open, 4)
	for i, want := range []Check{
		{Signal: SignalPartial, Person: hugo, Date: at(4, 6, 8, 0), Product: "Formation RIFAP", Amount: 6000, UnitPrice: 12000},
		{Signal: SignalUnsettled, Person: hugo, Date: at(3, 7, 18, 30), Product: "Adhésion", Amount: 7000},
		{Signal: SignalUnsettled, Person: hugo, Date: at(3, 7, 18, 30), Product: mollieCarnet, Amount: 30000},
		{Signal: SignalUnsettled, Person: Person{Name: "Noé Durand"}, Date: at(20, 7, 8, 0), Product: "Baptême", Amount: 3500},
	} {
		got := open[i]
		assert.Len(t, got.Fingerprint, 64, "hex HMAC")
		got.Fingerprint = ""
		assert.True(t, want.Date.Equal(got.Date), "line %d: %v", i, got.Date)
		got.Date = want.Date
		assert.Equal(t, want, got, "line %d", i)
	}
}

// Spec §7.7: who a line belongs to comes from the members list; names are
// never stored with the lines.
func TestChecksNameThePersonThroughTheMembersList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	memberstest.Import(t, f.members, "members_valid.xlsx")
	paid := time.Date(2026, 7, 1, 9, 0, 0, 0, paris(t))
	require.NoError(t, f.mollie.Import(ctx, &MollieExport{FileHash: []byte("f"), Lines: []MollieLine{
		{NameKey: "martin|lea", Product: "Adhésion", Amount: 7000, Settled: SettledNo, PaidAt: paid},
		{NameKey: "inconnu|paul", Product: "Baptême", Amount: 3500, Settled: SettledNo, PaidAt: paid.Add(time.Minute)},
	}}))
	open, _ := f.checks(t)
	require.Len(t, open, 2)
	assert.Equal(t, Person{Name: "Léa Martin", Shared: true}, open[0].Person, "homonyms: the name, flagged")
	assert.Equal(t, Person{}, open[1].Person, "no member: no name")
}

// Spec §13: a masked line stays masked after a new import of the same file,
// and a line gone from the export is gone from the page.
func TestDismissSurvivesReimportsAndFollowsTheExport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := NewCheckStore(f.db, f.keys, f.members, f.clock.now)
	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importMollie(t, f.validMollie(t))
	open, _ := f.checks(t)
	require.Len(t, open, 3)
	durand := open[2]
	require.Equal(t, "Baptême", durand.Product)

	require.ErrorIs(t, s.Dismiss(ctx, "00", "alice"), ErrCheckNotFound, "no such line")
	require.ErrorIs(t, s.Dismiss(ctx, "not hex", "alice"), ErrCheckNotFound)
	require.NoError(t, s.Dismiss(ctx, durand.Fingerprint, "alice"))
	require.NoError(t, s.Dismiss(ctx, durand.Fingerprint, "bob"), "twice is harmless")
	open, masked := f.checks(t)
	assert.Len(t, open, 2)
	require.Len(t, masked, 1)
	assert.Equal(t, durand.Fingerprint, masked[0].Fingerprint)
	assert.Equal(t, &Dismissal{By: "alice", At: f.clock.t}, masked[0].Dismissal, "the first masking is kept")

	f.importMollie(t, f.validMollie(t))
	open, masked = f.checks(t)
	assert.Len(t, open, 2)
	require.Len(t, masked, 1, "still masked after a new import of the same file")

	exp := f.validMollie(t)
	exp.Lines = exp.Lines[:10] // up to Petit Chloé: Durand's line leaves the export
	f.importMollie(t, exp)
	open, masked = f.checks(t)
	assert.Len(t, open, 2)
	assert.Empty(t, masked, "gone from the export, gone from the page")
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM dismissed_checks`), "the dismissal stays and holds no name")
}
