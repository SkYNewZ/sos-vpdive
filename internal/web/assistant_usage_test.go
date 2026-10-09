package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
)

func TestUsageJournal(t *testing.T) {
	stub := &streamStub{}
	e := newTestEnv(t, withAssistant(t, stub, 50))
	ctx := context.Background()
	e.srv.recordUsage(ctx, usageEntry{Account: "alice", Origin: "page", Model: "test-model", Outcome: "ok", Duration: 3 * time.Second,
		Result: assistant.Result{Calls: 2, Tools: map[string]int{"find_member": 1}, Usage: assistant.Usage{Input: 1000, Output: 500, CacheRead: 2000}}})
	e.srv.recordUsage(ctx, usageEntry{Account: "alice", Origin: "demande", Model: "test-model", Outcome: "limit"})
	assert.Equal(t, 2, e.count(t, "assistant_usage"))

	_, err := e.db.ExecContext(ctx, `INSERT INTO suggest_usage (at, model, input_tokens, output_tokens, cost_micro_usd, duration_ms, outcome)
		VALUES (?, 'test-model', 900, 60, 12000, 1200, 'ok')`, e.clock.now().Unix())
	require.NoError(t, err)

	cookie := e.login(t)
	status, page := e.page(t, cookie, "/assistant/journal")
	require.Equal(t, http.StatusOK, status)
	// 1 000 input × 0.3 + 2 000 cached × 0.03 + 500 output × 1.2 = 960 µ$.
	for _, want := range []string{"Alice", "find_member", "quota atteint", "0,0010\u00a0$",
		"Ce mois-ci, <strong>0,01\u00a0$</strong>\u00a0: 1 question à l'assistant (0,0010\u00a0$) et 1 suggestion du formulaire (0,01\u00a0$).",
		"Formulaire", `class="fill-assistant"`, `class="fill-suggest"`, "Voir les chiffres"} {
		assert.Contains(t, page, want)
	}
	assert.NotContains(t, page, "2 questions à l'assistant", "a question the quota refused called no model")

	_, assistantPage := e.page(t, cookie, "/assistant")
	assert.Contains(t, assistantPage, `href="/assistant/journal"`, "the owner finds the journal")

	bob := e.loginBob(t)
	status, _ = e.page(t, bob, "/assistant/journal")
	assert.Equal(t, http.StatusNotFound, status, "the owner only")
	_, assistantPage = e.page(t, bob, "/assistant")
	assert.NotContains(t, assistantPage, "/assistant/journal", "no link for the others")

	e.clock.advance(13 * 30 * 24 * time.Hour)
	require.NoError(t, e.srv.Purge(ctx))
	assert.Equal(t, 0, e.count(t, "assistant_usage"), "12 months")
}
