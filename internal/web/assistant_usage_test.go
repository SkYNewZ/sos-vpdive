package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

func TestCostMicro(t *testing.T) {
	a := &config.Assistant{Priced: true, PriceInput: 300_000, PriceOutput: 1_200_000, PriceCached: 30_000}
	assert.Equal(t, int64(1590), costMicro(assistant.Usage{Input: 1000, Output: 1000, CacheRead: 3000}, a),
		"300 + 1200 + 90 micro-dollars")
}

func TestUsageJournal(t *testing.T) {
	stub := &streamStub{}
	e := newTestEnv(t, withAssistant(t, stub, 50))
	ctx := context.Background()
	e.srv.recordUsage(ctx, usageEntry{Account: "alice", Origin: "page", Model: "test-model", Outcome: "ok", Duration: 3 * time.Second,
		Result: assistant.Result{Calls: 2, Tools: map[string]int{"find_member": 1}, Usage: assistant.Usage{Input: 1000, Output: 500, CacheRead: 2000}}})
	e.srv.recordUsage(ctx, usageEntry{Account: "alice", Origin: "demande", Model: "test-model", Outcome: "limit"})
	assert.Equal(t, 2, e.count(t, "assistant_usage"))

	cookie := e.login(t)
	status, page := e.page(t, cookie, "/assistant/journal")
	require.Equal(t, http.StatusOK, status)
	// 1 000 input × 0.3 + 2 000 cached × 0.03 + 500 output × 1.2 = 960 µ$.
	for _, want := range []string{"Alice", "2 questions", "find_member", "quota atteint", "0,0010 $"} {
		assert.Contains(t, page, want)
	}

	bob := e.loginBob(t)
	status, _ = e.page(t, bob, "/assistant/journal")
	assert.Equal(t, http.StatusNotFound, status, "the owner only")

	e.clock.advance(13 * 30 * 24 * time.Hour)
	require.NoError(t, e.srv.Purge(ctx))
	assert.Equal(t, 0, e.count(t, "assistant_usage"), "12 months")
}
