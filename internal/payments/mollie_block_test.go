package payments

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
)

func TestMollieBlockStates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bernard := f.nameHash("Bernard", "Hugo")

	b, err := f.mollie.Block(ctx, bernard)
	require.NoError(t, err)
	assert.Equal(t, BlockNoLines, b.State, "nothing imported")

	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importMollie(t, f.validMollie(t))
	for name, c := range map[string]struct {
		hash []byte
		want BlockState
	}{
		"not a member": {nil, BlockNoMember},
		"homonyms":     {f.nameHash("Martin", "Léa"), BlockAmbiguous},
		"no line":      {f.nameHash("Nobody", "Here"), BlockEmpty},
		"lines":        {bernard, BlockLines},
	} {
		b, err := f.mollie.Block(ctx, c.hash)
		require.NoError(t, err)
		assert.Equal(t, c.want, b.State, name)
		assert.Equal(t, "alice", b.Import.ImportedBy, name)
		assert.True(t, mollieCreated.Equal(b.Import.ExportedAt), name)
	}

	f.clock.t = f.clock.t.AddDate(0, 4, 0)
	require.NoError(t, f.mollie.Purge(ctx))
	b, err = f.mollie.Block(ctx, bernard)
	require.NoError(t, err)
	assert.Equal(t, BlockPurged, b.State)
}

// Spec §7.5: the lines of one person at one minute are one payment, with
// its total; payments come newest first, lines in the order of the file.
func TestMollieBlockGroupsPaymentsNewestFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importMollie(t, f.validMollie(t))
	loc := paris(t)

	b, err := f.mollie.Block(ctx, f.nameHash("Bernard", "Hugo"))
	require.NoError(t, err)
	require.Len(t, b.Payments, 3)
	type summary struct {
		paidAt   time.Time
		method   string
		total    Amount
		products []string
	}
	got := make([]summary, 0, len(b.Payments))
	for _, p := range b.Payments {
		s := summary{paidAt: p.PaidAt.UTC(), method: p.Method, total: p.Total}
		for _, l := range p.Lines {
			s.products = append(s.products, l.Product)
		}
		got = append(got, s)
	}
	assert.Equal(t, []summary{
		{time.Date(2026, 8, 12, 14, 5, 0, 0, loc).UTC(), "Carte de crédit", 5300, []string{"Calendrier", "Location d'un gilet stabilisateur", "Supplément distance"}},
		{time.Date(2026, 7, 10, 9, 15, 0, 0, loc).UTC(), "Carte de crédit", -4000, []string{"Calendrier"}},
		{time.Date(2026, 7, 3, 18, 30, 0, 0, loc).UTC(), "Pay by Bank", 37000, []string{mollieCarnet, "Adhésion"}},
	}, got)
	assert.True(t, b.Payments[1].Lines[0].Negative(), "a refund or a discount")
	assert.False(t, b.Payments[0].Lines[0].Negative())
	assert.True(t, b.Payments[2].Lines[1].Unsettled())

	chloe, err := f.mollie.Block(ctx, f.nameHash("Petit", "Chloé"))
	require.NoError(t, err)
	require.Len(t, chloe.Payments, 2)
	assert.Equal(t, Amount(4000), chloe.Payments[1].Total, "50 € minus a 10 € discount, plus a line at zero")
}

// Spec §7.7: on the request page, a line to check is highlighted until a
// resolver masks it; then it says who checked it, and when.
func TestBlocksShowWhoCheckedALine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importPayments(t, f.valid(t))
	f.importMollie(t, f.validMollie(t))
	bernard := f.nameHash("Bernard", "Hugo")

	mollie, err := f.mollie.Block(ctx, bernard)
	require.NoError(t, err)
	membership := mollie.Payments[2].Lines[1]
	require.Equal(t, "Adhésion", membership.Product)
	assert.Nil(t, membership.Dismissal)
	block, err := f.store.Block(ctx, bernard)
	require.NoError(t, err)
	require.Equal(t, "Formation RIFAP", block.ToSettle[0].Product)
	assert.True(t, block.ToSettle[0].Partial())
	assert.Nil(t, block.ToSettle[0].Dismissal)

	open, _ := f.checks(t)
	checks := NewCheckStore(f.db, f.keys, f.members, f.clock.now)
	for _, c := range open {
		if c.Product == "Adhésion" || c.Product == "Formation RIFAP" {
			require.NoError(t, checks.Dismiss(ctx, c.Fingerprint, "bob"))
		}
	}
	mollie, err = f.mollie.Block(ctx, bernard)
	require.NoError(t, err)
	assert.Equal(t, &Dismissal{By: "bob", At: f.clock.t}, mollie.Payments[2].Lines[1].Dismissal)
	assert.Nil(t, mollie.Payments[2].Lines[0].Dismissal, "the carnet line of the same payment is still to check")
	block, err = f.store.Block(ctx, bernard)
	require.NoError(t, err)
	assert.Equal(t, &Dismissal{By: "bob", At: f.clock.t}, block.ToSettle[0].Dismissal)
	assert.Nil(t, block.ToSettle[1].Dismissal, "a line due in full is no line to check")
}
