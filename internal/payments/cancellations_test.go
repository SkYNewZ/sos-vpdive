package payments

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spec §7.4: one row per cancelled outing (title and « Du »), newest first
// (amended by the owner);
// a dive paid on two carnets is two lines but one person.
func TestCancellations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.store.Cancellations(ctx)
	require.NoError(t, err)
	assert.False(t, c.Imported)
	assert.False(t, c.Purged)

	f.importPayments(t, f.valid(t))
	c, err = f.store.Cancellations(ctx)
	require.NoError(t, err)
	require.True(t, c.Imported)
	assert.Equal(t, "alice", c.Import.ImportedBy)
	loc := paris(t)
	want := []Outing{
		{Title: cancelledCaps, Starts: time.Date(2026, 5, 10, 9, 0, 0, 0, loc), Persons: 3, Lines: 4, ByCarnet: 9000},
		{Title: cancelledLower, Starts: time.Date(2026, 5, 8, 20, 0, 0, 0, loc), Persons: 1, Lines: 1, ByMoney: 3500},
	}
	require.Len(t, c.Outings, len(want))
	for i, o := range c.Outings {
		assert.True(t, want[i].Starts.Equal(o.Starts), "%v", o.Starts)
		want[i].Starts = o.Starts // same instant, the location read back may differ
	}
	assert.Equal(t, want, c.Outings)
	assert.Equal(t, 5, c.Lines)
	assert.Equal(t, 4, c.Persons, "inscriptions: distinct persons per outing, summed")
}

func TestCancellationsOrderUnknownDatesLast(t *testing.T) {
	f := newFixture(t)
	noDate := prepaid("Bernard", "Hugo", 30, "Sortie annulée sans date", nil, "05/01/2026 10:12:00")
	late := prepaid("Bernard", "Hugo", 30, "Sortie annulée en juin", "20/06/2026 09:00", "05/01/2026 10:12:00")
	early := prepaid("Durand", "Noé", 30, "Sortie annulée en mai", "02/05/2026 09:00", "05/01/2026 10:12:00")
	sameTitleOtherDay := prepaid("Durand", "Noé", 30, "Sortie annulée en juin", "27/06/2026 09:00", "05/01/2026 10:12:00")
	exp, err := parseSheet(t, noDate, late, early, sameTitleOtherDay)
	require.NoError(t, err)
	f.importPayments(t, exp)

	c, err := f.store.Cancellations(context.Background())
	require.NoError(t, err)
	titles := make([]string, len(c.Outings))
	for i, o := range c.Outings {
		titles[i] = o.Title
	}
	assert.Equal(t, []string{"Sortie annulée en juin", "Sortie annulée en juin", "Sortie annulée en mai", "Sortie annulée sans date"}, titles,
		"a title on two dates is two outings")
	assert.Equal(t, 27, c.Outings[0].Starts.Day(), "newest first")
	assert.True(t, c.Outings[3].Starts.IsZero())
}

func TestCancellationsAfterThePurge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.importPayments(t, f.valid(t))
	f.clock.t = f.clock.t.AddDate(0, 4, 0)
	require.NoError(t, f.store.Purge(ctx))

	c, err := f.store.Cancellations(ctx)
	require.NoError(t, err)
	assert.False(t, c.Imported)
	assert.True(t, c.Purged, "the page can say the lines were purged")
}
