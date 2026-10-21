package suggest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecisionsChoosesArticleAndCategory(t *testing.T) {
	c, calls := decisionClient(t, `{"answers":[{"type":"choice","name":"categorie","choice":"inscription","confidence":0.9},{"type":"choice","name":"fiche","choice":"carnet","confidence":0.8}],"usage":{"input_tokens":123}}`, 200)
	got, err := c.Choose(context.Background(), Request{CategoryID: "paiement"}, []Fiche{{ID: "carnet"}}, []Category{{ID: "inscription"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"carnet"}, got.IDs)
	assert.Equal(t, "inscription", got.CategoryID)
	assert.True(t, got.ValidDecisions)
	assert.Equal(t, 123, got.InputTokens)
	assert.Len(t, calls, 1)
}

func TestCategoryDecisionValidation(t *testing.T) {
	for _, tc := range []struct {
		name, choice, confidence, want string
		invalid                        bool
	}{
		{"boundary .90", `"inscription"`, `0.90`, "inscription", false},
		{"below .90", `"inscription"`, `0.899`, "", false},
		{"same category", `"paiement"`, `0.99`, "", false},
		{"autre is a category", `"autre"`, `0.99`, "autre", false},
		{"__none__ is abstention", `"__none__"`, `0.99`, "", false},
		{"unknown choice", `"invented"`, `0.99`, "", true},
		{"numeric choice", `42`, `0.99`, "", true},
		{"null choice", `null`, `0.99`, "", true},
		{"null confidence", `"inscription"`, `null`, "", true},
		{"negative confidence", `"inscription"`, `-0.1`, "", true},
		{"excess confidence", `"inscription"`, `1.1`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := fmt.Sprintf(`{"type":"choice","name":"categorie","choice":%s,"confidence":%s}`, tc.choice, tc.confidence)
			c, _ := decisionClient(t, `{"answers":[{"type":"choice","name":"fiche","choice":"carnet","confidence":0.95},`+answer+`],"usage":{"input_tokens":123}}`, 200)
			got, err := c.Choose(context.Background(), Request{CategoryID: "paiement"}, []Fiche{{ID: "carnet"}}, []Category{{ID: "inscription"}, {ID: "paiement"}, {ID: "autre"}})
			if tc.invalid {
				require.ErrorIs(t, err, ErrInvalid)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []string{"carnet"}, got.IDs)
			assert.Equal(t, tc.want, got.CategoryID)
			assert.True(t, got.ValidDecisions)
			assert.Equal(t, 123, got.InputTokens)
		})
	}
}

func TestDecisionsKeepIndependentAnswers(t *testing.T) {
	fiche := `{"type":"choice","name":"fiche","choice":"carnet","confidence":0.95}`
	category := `{"type":"choice","name":"categorie","choice":"inscription","confidence":0.95}`
	for _, tc := range []struct {
		name, answers string
		wantIDs       []string
		wantCategory  string
	}{
		{"missing category", fiche, []string{"carnet"}, ""},
		{"missing confidence", fiche + `,{"type":"choice","name":"categorie","choice":"inscription"}`, []string{"carnet"}, ""},
		{"refusal", fiche + `,{"type":"refusal","name":"categorie"}`, []string{"carnet"}, ""},
		{"duplicate category", category + `,` + fiche + `,` + category, []string{"carnet"}, ""},
		{"duplicate fiche", fiche + `,` + category + `,` + fiche, nil, "inscription"},
		{"missing fiche", category, nil, "inscription"},
		{"unexpected answer name", fiche + `,{"type":"choice","name":"unknown","choice":"inscription","confidence":0.95}`, []string{"carnet"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := decisionClient(t, `{"answers":[`+tc.answers+`],"usage":{"input_tokens":123}}`, 200)
			got, err := c.Choose(context.Background(), Request{CategoryID: "paiement"}, []Fiche{{ID: "carnet"}}, []Category{{ID: "inscription"}})
			require.ErrorIs(t, err, ErrInvalid)
			assert.Equal(t, tc.wantIDs, got.IDs)
			assert.Equal(t, tc.wantCategory, got.CategoryID)
			assert.True(t, got.ValidDecisions)
			assert.NotContains(t, err.Error(), "inscription")
		})
	}
}

func TestDecisionsClassifyWithoutArticles(t *testing.T) {
	c, calls := decisionClient(t, `{"answers":[{"type":"choice","name":"categorie","choice":"inscription","confidence":0.95}]}`, 200)
	got, err := c.Choose(context.Background(), Request{CategoryID: "paiement"}, nil, []Category{{ID: "inscription"}})
	require.NoError(t, err)
	assert.Equal(t, "inscription", got.CategoryID)
	assert.Len(t, calls, 1)
}
