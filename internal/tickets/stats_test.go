package tickets

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIDByRef(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, ref := e.submit(t)
	got, err := e.store.IDByRef(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, id, got)
	_, err = e.store.IDByRef(ctx, "CPP-9999")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = e.store.IDByRef(ctx, "")
	require.ErrorIs(t, err, ErrNotFound, "a draft has no ref yet and never matches")
}

// Spec §8.3: a closed request that is deleted lives on in stats_monthly
// only. It stays counted as closed, with the hours it took.
func TestStatsCountsLiveAndArchived(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()

	deleted, _ := e.submit(t)
	require.NoError(t, e.apply(t, deleted, Command{Action: ActionTake}))
	e.clock.advance(5 * time.Hour)
	require.NoError(t, e.apply(t, deleted, Command{Action: ActionClose}))
	require.NoError(t, e.apply(t, deleted, Command{Action: ActionDelete}))

	closed, _ := e.submit(t)
	require.NoError(t, e.apply(t, closed, Command{Action: ActionTake}))
	e.clock.advance(3 * time.Hour)
	require.NoError(t, e.apply(t, closed, Command{Action: ActionClose}))

	e.clock.advance(31 * 24 * time.Hour) // October
	e.submit(t)
	taken, _ := e.submit(t)
	require.NoError(t, e.apply(t, taken, Command{Action: ActionTake}))
	_, err := e.db.ExecContext(ctx, `INSERT INTO deflections (category, kb_ids, created_at) VALUES ('paiement', '["fiche"]', ?)`, e.clock.now().Unix())
	require.NoError(t, err)

	got, err := e.store.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[Status]int{StatusDone: 1, StatusTodo: 1, StatusInProgress: 1}, got.ByStatus)
	assert.Equal(t, []MonthStat{
		{Month: "2026-09", Category: "carnet", Submitted: 1, Closed: 2, HoursToClose: 8},
		{Month: "2026-10", Category: "carnet", Submitted: 2},
		{Month: "2026-10", Category: "paiement", Avoided: 1},
	}, got.Months)

	empty, err := newTestStore(t).store.Stats(ctx)
	require.NoError(t, err)
	assert.Empty(t, empty.ByStatus)
	assert.Empty(t, empty.Months)
}
