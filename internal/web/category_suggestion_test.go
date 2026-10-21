package web

import (
	"context"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"testing"

	"github.com/SkYNewZ/sos-vpdive/internal/suggest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCombinedDecisionKeepsPartialResultAndBillsOnce(t *testing.T) {
	for _, tc := range []struct {
		name, answers string
		ids           []string
		category      string
	}{
		{"invalid category", `{"type":"choice","name":"fiche","choice":"carnet-solde-negatif","confidence":0.99},{"type":"choice","name":"categorie","choice":"invented","confidence":0.99}`, []string{"carnet-solde-negatif"}, ""},
		{"invalid fiche", `{"type":"choice","name":"fiche","choice":"invented","confidence":0.99},{"type":"choice","name":"categorie","choice":"inscription","confidence":0.99}`, nil, "inscription"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &modelStub{answer: `{"answers":[` + tc.answers + `]}`, usage: modelUsage{Input: 123}}
			e := newTestEnv(t, withModel(t, m, 200))
			ctx := context.Background()
			res, ok := e.srv.formModelCall(ctx, formModelSuggest, e.srv.cfg.Suggest, func() (suggest.Result, error) {
				return e.srv.suggest.Choose(ctx, suggest.Request{CategoryID: "paiement"}, e.srv.suggestionFiches(), e.srv.suggestionCategories())
			})
			require.True(t, ok)
			assert.Equal(t, tc.ids, res.IDs)
			assert.Equal(t, tc.category, res.CategoryID)
			var count, tokens int
			require.NoError(t, e.db.QueryRowContext(ctx, `SELECT count(*),sum(input_tokens) FROM suggest_usage WHERE outcome='invalid_output'`).Scan(&count, &tokens))
			assert.Equal(t, 1, count)
			assert.Equal(t, 123, tokens)
			assert.Len(t, m.calls(), 1)
		})
	}
}

// Preserve the ordinary summary failure path when the selector supports partial answers.
func TestSummaryErrorsStillDiscardResults(t *testing.T) {
	e := newTestEnv(t)
	result, ok := e.srv.formModelCall(context.Background(), formModelSummary, &config.LLM{DailyLimit: 200}, func() (suggest.Result, error) {
		return suggest.Result{Summary: "untrusted", ValidDecisions: true}, suggest.ErrInvalid
	})
	assert.False(t, ok)
	assert.Empty(t, result.Summary)
}
