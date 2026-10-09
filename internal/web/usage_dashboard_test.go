package web

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func micro(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

func TestBuildDashboard(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	// Tuesday 27 October 2026, 9:00 in Paris: summer time ended on Sunday 25.
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	calls := []usageCall{
		{Account: "alice", At: time.Date(2026, 10, 26, 22, 59, 0, 0, time.UTC), Cost: micro(70_000)},   // 23:59 on the 26th
		{Account: "alice", At: time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC), Cost: micro(80_000)},    // midnight on the 27th
		{Account: "bob", At: time.Date(2026, 10, 25, 22, 30, 0, 0, time.UTC), Cost: micro(50_000)},     // 23:30 on the 25th, winter time
		{Account: "", At: time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC), Cost: micro(1_000)},           // a suggestion today
		{Account: "", At: time.Date(2026, 9, 30, 22, 30, 0, 0, time.UTC), Cost: micro(2_000)},          // 0:30 on 1 October, summer time
		{Account: "alice", At: time.Date(2026, 9, 30, 21, 30, 0, 0, time.UTC), Cost: micro(40_000)},    // 23:30 on 30 September
		{Account: "bob", At: time.Date(2025, 11, 1, 10, 0, 0, 0, time.UTC), Cost: micro(10_000)},       // the oldest month shown
		{Account: "bob", At: time.Date(2025, 10, 31, 10, 0, 0, 0, time.UTC), Cost: micro(999_000_000)}, // before it
	}
	d := buildDashboard(calls, now, paris)

	require.Len(t, d.Days.Columns, dashboardDays)
	assert.Equal(t, "lun. 28 sept.", d.Days.Columns[0].Label)
	assert.Equal(t, "mar. 27 oct.", d.Days.Columns[dashboardDays-1].Label, "today closes the days")
	for i, c := range d.Days.Columns[1:] {
		assert.NotEqual(t, d.Days.Columns[i].Label, c.Label, "one column per day across the time change")
	}
	day := func(label string) split {
		for _, c := range d.Days.Columns {
			if c.Label == label {
				return c.split
			}
		}
		t.Fatalf("no column %s", label)
		return split{}
	}
	assert.Equal(t, split{Assistant: spend{Calls: 1, Cost: micro(70_000)}}, day("lun. 26 oct."), "23:59 stays on its day")
	assert.Equal(t, split{Assistant: spend{Calls: 1, Cost: micro(80_000)}, Suggest: spend{Calls: 1, Cost: micro(1_000)}}, d.Today)
	assert.Equal(t, split{Assistant: spend{Calls: 1, Cost: micro(50_000)}}, day("dim. 25 oct."))
	assert.Equal(t, split{Assistant: spend{Calls: 1, Cost: micro(40_000)}}, day("mer. 30 sept."))

	require.Len(t, d.Months.Columns, dashboardMonths)
	assert.Equal(t, "novembre 2025", d.Months.Columns[0].Label)
	assert.Equal(t, split{Assistant: spend{Calls: 1, Cost: micro(10_000)}}, d.Months.Columns[0].split, "October 2025 is out")
	assert.Equal(t, split{Assistant: spend{Calls: 1, Cost: micro(40_000)}}, d.Months.Columns[10].split)
	assert.Equal(t, split{Assistant: spend{Calls: 3, Cost: micro(200_000)}, Suggest: spend{Calls: 2, Cost: micro(3_000)}}, d.Month)
	assert.Equal(t, micro(203_000), d.Month.Cost())
	assert.True(t, d.Days.Cost)
	assert.True(t, d.Months.Cost)
	assert.Equal(t, "0,25\u00a0$", d.Months.Top, "the axis tops at a round amount")
	assert.Equal(t, "0,10\u00a0$", d.Days.Top)

	assert.Equal(t, []accountSpend{
		{Account: "alice", Calls: 2, Cost: micro(150_000), Share: 100},
		{Account: "bob", Calls: 1, Cost: micro(50_000), Share: 33},
		{Account: "", Calls: 2, Cost: micro(3_000), Share: 2},
	}, d.Accounts, "this month, the costliest first; the form's suggestions under no account")

	c := d.Days.Columns[dashboardDays-1]
	assert.NotEmpty(t, c.A)
	assert.NotEmpty(t, c.S)
	assert.Empty(t, d.Days.Columns[0].A, "nothing drawn for an empty day")
}

func TestBuildDashboardWithoutPrices(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	d := buildDashboard([]usageCall{{Account: "alice", At: now}, {At: now}, {At: now}}, now, paris)
	assert.False(t, d.Days.Cost, "no price: the charts count calls")
	assert.Equal(t, "5", d.Days.Top, "3 calls today")
	assert.False(t, d.Month.Cost().Valid)
	assert.Equal(t, []accountSpend{
		{Calls: 2, Share: 100},
		{Account: "alice", Calls: 1, Share: 50},
	}, d.Accounts)
}

func TestBuildDashboardWithPartialPrices(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	calls := []usageCall{
		{Account: "alice", At: now, Cost: micro(80_000)},
		{At: now}, // LLM_PRICE_* unset
		{At: now.AddDate(0, 0, -35), Cost: micro(1_000)}, // 4 September
	}
	d := buildDashboard(calls, now, paris)
	assert.False(t, d.Days.Cost, "a call without a price: the days count calls")
	assert.NotEmpty(t, d.Days.Columns[dashboardDays-1].S, "the suggestion still shows")
	assert.False(t, d.Months.Cost)
	assert.Equal(t, micro(80_000), d.Today.Assistant.Cost)
	assert.False(t, d.Today.Suggest.Cost.Valid)
	assert.False(t, d.Today.Cost().Valid, "no total that leaves calls out")
	assert.Equal(t, []accountSpend{
		{Calls: 1, Share: 100},
		{Account: "alice", Calls: 1, Cost: micro(80_000), Share: 100},
	}, d.Accounts, "shares count calls")

	calls[1].At = now.AddDate(0, 0, -36) // 3 September: out of the days, in the months
	d = buildDashboard(calls, now, paris)
	assert.True(t, d.Days.Cost, "every call of the 30 days had a price")
	assert.False(t, d.Months.Cost)
	assert.False(t, d.Months.Columns[dashboardMonths-2].Suggest.Cost.Valid, "a partial sum would understate September")
}

func TestDollars(t *testing.T) {
	for micro, want := range map[int64]string{0: "0,00\u00a0$", 2_413_456: "2,41\u00a0$", 10_000: "0,01\u00a0$", 1_234: "0,0012\u00a0$"} {
		assert.Equal(t, want, dollars(sql.NullInt64{Int64: micro, Valid: true}))
	}
	assert.Equal(t, "—", dollars(sql.NullInt64{}))
}
