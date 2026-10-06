package payments

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
)

func products(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Product
	}
	return out
}

func TestBlockStates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bernard := f.nameHash("Bernard", "Hugo")

	b, err := f.store.Block(ctx, bernard)
	require.NoError(t, err)
	assert.Equal(t, BlockNoLines, b.State, "nothing imported")

	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importPayments(t, f.valid(t))
	for name, c := range map[string]struct {
		hash []byte
		want BlockState
	}{
		"not a member": {nil, BlockNoMember},
		"homonyms":     {f.nameHash("Martin", "Léa"), BlockAmbiguous},
		"no line":      {f.nameHash("Nobody", "Here"), BlockEmpty},
		"lines":        {bernard, BlockLines},
	} {
		b, err := f.store.Block(ctx, c.hash)
		require.NoError(t, err)
		assert.Equal(t, c.want, b.State, name)
		assert.Equal(t, "alice", b.Import.ImportedBy, name)
		assert.True(t, fixtureCreated.Equal(b.Import.ExportedAt), name)
	}

	require.NoError(t, f.store.Purge(ctx))
	f.clock.t = f.clock.t.AddDate(0, 4, 0)
	require.NoError(t, f.store.Purge(ctx))
	b, err = f.store.Block(ctx, bernard)
	require.NoError(t, err)
	assert.Equal(t, BlockPurged, b.State, "purged after 90 days: an import happened, its lines are gone")
	assert.Equal(t, "alice", b.Import.ImportedBy)
}

// Spec §7.3: balances as read, lines to settle, cancelled outings still to
// delete and the ten latest lines, newest first.
func TestBlockLists(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	memberstest.Import(t, f.members, "members_valid.xlsx")
	f.importPayments(t, f.valid(t))

	b, err := f.store.Block(ctx, f.nameHash("Bernard", "Hugo"))
	require.NoError(t, err)
	assert.Equal(t, []string{carnetTitle, "Formation N2", carnetTitle}, products(b.Balances),
		"two balances of one title stay two lines")
	assert.Equal(t, []Amount{-3000, -6000, -18000}, []Amount{b.Balances[0].UnitPrice, b.Balances[1].UnitPrice, b.Balances[2].UnitPrice})
	assert.Equal(t, []string{"Formation RIFAP", "Plongée Porquerolles"}, products(b.ToSettle))
	assert.Equal(t, []string{cancelledLower, cancelledCaps}, products(b.Cancelled))
	require.Len(t, b.Latest, 10)
	assert.Equal(t, "Sortie Sec de la Croix", b.Latest[0].Product)
	assert.Equal(t, "Formation N2", b.Latest[9].Product, "the purchase and the oldest balance are older than the ten latest")
}

func TestLineReading(t *testing.T) {
	paidCarnet := Line{State: StatePaid, Method: MethodPrepaid, UnitPrice: 3000, Product: "SORTIE ANNULÉE - Levant"}
	assert.True(t, paidCarnet.CancelledOuting(), "capitals")
	assert.True(t, paidCarnet.Prepaid())
	assert.True(t, Line{State: StatePaid, Method: MethodVPayDive, Product: "Plongée (annulée)"}.CancelledOuting())
	assert.False(t, Line{State: StateCancelled, Product: "Plongée annulée"}.CancelledOuting(), "a cancelled line has no effect")
	assert.False(t, Line{State: StatePaid, Product: "Plongée"}.CancelledOuting())

	assert.True(t, Line{State: StateDue, ProductType: TypeCard, UnitPrice: -18000}.Balance())
	assert.True(t, Line{State: StateDue, ProductType: TypeTraining, UnitPrice: -6000}.Balance())
	assert.False(t, Line{State: StateDue, ProductType: "", UnitPrice: -500}.Balance(), "not a carnet or a training")
	assert.False(t, Line{State: StatePaid, ProductType: TypeCard, UnitPrice: 30000}.Balance(), "a purchase")

	assert.True(t, Line{State: StateDue, UnitPrice: 3500}.ToSettle())
	assert.True(t, Line{State: StatePartial, UnitPrice: 12000}.ToSettle())
	assert.False(t, Line{State: StateDue, ProductType: TypeCard, UnitPrice: -18000}.ToSettle(), "a balance is not due")

	assert.Equal(t, Amount(3000), Line{Method: MethodPrepaid, UnitPrice: 3000}.Settled(), "a carnet pays the unit price")
	assert.Equal(t, Amount(3000), Line{Method: MethodVPayDive, UnitPrice: 3500, Paid: 3000}.Settled(), "real money: what was paid")

	assert.True(t, Line{State: StatePaid, Method: MethodVPayDive, Paid: -3500}.ProbableRefund())
	assert.False(t, Line{State: StatePaid, Method: MethodVPayDive, Paid: -3500, ProductType: TypeCard}.ProbableRefund())
	assert.False(t, Line{State: StatePaid, Method: MethodVPayDive, Paid: 3500}.ProbableRefund())
}
