package web

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllowCountsInAFixedWindow(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	allow := func(key string) bool {
		t.Helper()
		ok, err := e.srv.limiter.allow(ctx, key, 3, time.Hour)
		require.NoError(t, err)
		return ok
	}
	for i := range 3 {
		assert.True(t, allow("test:198.51.100.7"), "attempt %d", i+1)
	}
	assert.False(t, allow("test:198.51.100.7"))
	assert.True(t, allow("test:198.51.100.8"), "another value has its own counter")

	e.clock.advance(time.Hour + time.Second)
	assert.True(t, allow("test:198.51.100.7"), "a new window starts")
}

func TestAllowStoresHashedKeys(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	_, err := e.srv.limiter.allow(ctx, "form-email:lea.martin@example.org", 5, time.Hour)
	require.NoError(t, err)
	var key string
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT key FROM counters`).Scan(&key))
	assert.Regexp(t, `^form-email:[0-9a-f]{64}$`, key)
}

func TestPurgeDropsCountersOlderThanTwoDays(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	_, err := e.srv.limiter.allow(ctx, "recover-email:old@example.org", 3, 24*time.Hour)
	require.NoError(t, err)
	e.clock.advance(47 * time.Hour)
	_, err = e.srv.limiter.allow(ctx, "recover-email:new@example.org", 3, 24*time.Hour)
	require.NoError(t, err)
	e.clock.advance(2 * time.Hour)

	require.NoError(t, e.srv.Purge(ctx))
	assert.Equal(t, 1, e.count(t, "counters"))
}
