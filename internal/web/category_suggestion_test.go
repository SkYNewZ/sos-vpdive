package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"

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

func categoryEnv(t *testing.T) (*testEnv, *modelStub, int64) {
	t.Helper()
	m := &modelStub{answer: `{"answers":[{"type":"choice","name":"fiche","choice":"autre","confidence":0.95},{"type":"choice","name":"categorie","choice":"inscription","confidence":0.95}]}`}
	e := newTestEnv(t, withModel(t, m, 200))
	e.importMembers(t, "members_valid.xlsx")
	v := validRequest(e.formKey(t))
	v.Set("categorie", "paiement")
	sent := e.sendRequest(t, v)
	require.Equal(t, http.StatusSeeOther, sent.Code, sent.Body.String())
	assert.NotContains(t, sent.Body.String(), "data-category-suggestion")
	return e, m, e.firstID(t)
}

func TestCategorySuggestionCommitteeFlow(t *testing.T) {
	e, m, id := categoryEnv(t)
	cookie := e.login(t)
	before := e.detail(t, id)
	assert.Equal(t, "paiement", before.Category)
	require.Equal(t, "inscription", before.SuggestedCategory)
	page := e.openTicket(t, cookie, id)
	assert.Contains(t, page.body, "data-category-suggestion")
	assert.Contains(t, page.body, "Appliquer cette catégorie")
	assert.Contains(t, page.body, "Garder « Paiement »")
	assert.Contains(t, page.body, "Reclasser dans")
	rec := e.act(t, cookie, id, page, url.Values{"action": {"category"}, "categorie": {"inscription"}})
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	after := e.detail(t, id)
	assert.Equal(t, "inscription", after.Category)
	assert.Empty(t, after.SuggestedCategory)
	assert.Equal(t, before.Fields, after.Fields)
	assert.Equal(t, "category_changed", after.Events[len(after.Events)-1].Type)
	assert.NotContains(t, e.openTicket(t, cookie, id).body, "data-category-suggestion")
	assert.Len(t, m.calls(), 1)
}

func TestCategorySuggestionDismissalSurvivesReload(t *testing.T) {
	e, m, id := categoryEnv(t)
	cookie := e.login(t)
	page := e.openTicket(t, cookie, id)
	mailCount := len(e.mails(t))
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, id, page, url.Values{"action": {"dismiss_category_suggestion"}}).Code)
	assert.Equal(t, "paiement", e.detail(t, id).Category)
	assert.NotContains(t, e.openTicket(t, cookie, id).body, "data-category-suggestion")
	assert.Len(t, e.mails(t), mailCount)
	assert.Len(t, m.calls(), 1)
	require.Equal(t, http.StatusConflict, e.act(t, cookie, id, page, url.Values{"action": {"dismiss_category_suggestion"}}).Code)
	current := e.openTicket(t, cookie, id)
	require.Equal(t, http.StatusUnprocessableEntity, e.act(t, cookie, id, current, url.Values{"action": {"dismiss_category_suggestion"}}).Code)
}

func TestCategorySuggestionIsCommitteeOnly(t *testing.T) {
	e, _, id := categoryEnv(t)
	var token string
	for _, mail := range e.mails(t) {
		assert.NotContains(t, mail.Text, "Catégorie suggérée")
		assert.NotContains(t, mail.Text, "Appliquer cette catégorie")
		if match := trackingLinkPattern.FindStringSubmatch(mail.Text); match != nil {
			token = match[1]
		}
	}
	require.NotEmpty(t, token)
	code, page := e.tracking(t, token)
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, page, "data-category-suggestion")
	assert.NotContains(t, page, "dismiss_category_suggestion")
	tb := &toolbox{s: e.srv, c: &assistant.Conversation{}}
	ref := tb.c.AddPerson(assistant.Person{Name: "Léa Martin", Email: "lea.martin@example.org"})
	got, _ := tb.call(t, "member_requests", `{"ref":"`+ref+`"}`)
	payload := mustJSON(t, got)
	assert.NotContains(t, payload, "suggested_category")
	assert.NotContains(t, payload, "SuggestedCategory")
	assert.NotContains(t, payload, "Inscription à une activité")
	assert.Equal(t, "paiement", e.detail(t, id).Category)
}

func TestCategorySuggestionVisibilityAndPermissions(t *testing.T) {
	for _, state := range []string{"unknown", "committee only", "same category", "closed", "absent"} {
		t.Run(state, func(t *testing.T) {
			e, _, id := categoryEnv(t)
			switch state {
			case "closed":
				e.apply(t, id, tickets.Command{Action: tickets.ActionClose})
			case "absent":
				_, err := e.db.ExecContext(context.Background(), `UPDATE tickets SET suggested_category=NULL WHERE id=?`, id)
				require.NoError(t, err)
			default:
				category := map[string]string{"unknown": "removed", "committee only": "bug", "same category": "paiement"}[state]
				_, err := e.db.ExecContext(context.Background(), `UPDATE tickets SET suggested_category=? WHERE id=?`, e.deps.Keys.SealString(category), id)
				require.NoError(t, err)
			}
			cookie := e.login(t)
			page := e.openTicket(t, cookie, id)
			assert.NotContains(t, page.body, "data-category-suggestion")
			if state != "closed" {
				assert.Contains(t, page.body, "Reclasser dans")
			}
		})
	}
	e, _, id := categoryEnv(t)
	cookie := e.login(t)
	path := "/demandes/" + strconv.FormatInt(id, 10) + "/actions"
	page := e.openTicket(t, cookie, id)
	v := url.Values{"action": {"dismiss_category_suggestion"}, "csrf": {page.csrf}, "version": {page.version}}
	forbidden := e.do(t, http.MethodPost, adminHost, path, formBody(v), formType, withCookie(cookie), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.org") })
	assert.Equal(t, http.StatusForbidden, forbidden.Code)
	assert.NotEqual(t, http.StatusSeeOther, e.do(t, http.MethodPost, publicHost, path, formBody(v), formType).Code)
	assert.Contains(t, e.detail(t, id).SuggestedCategory, "inscription")
	unauthenticated := e.do(t, http.MethodGet, adminHost, strings.TrimSuffix(path, "/actions"), nil)
	assert.Equal(t, http.StatusSeeOther, unauthenticated.Code)
}

func TestCategorySuggestionStaysOffScreen2(t *testing.T) {
	m := &modelStub{answer: `{"answers":[{"type":"choice","name":"fiche","choice":"carnet-solde-negatif","confidence":0.95},{"type":"choice","name":"categorie","choice":"inscription","confidence":0.95}]}`}
	e := newTestEnv(t, withModel(t, m, 200))
	e.importMembers(t, "members_valid.xlsx")
	page, token := e.screen2(t, validRequest(e.formKey(t)))
	assert.NotContains(t, page, "data-category-suggestion")
	assert.NotContains(t, page, "dismiss_category_suggestion")
	require.Equal(t, http.StatusSeeOther, e.draftAction(t, "/demandes/confirmer", token).Code)
	assert.Equal(t, "inscription", e.detail(t, e.firstID(t)).SuggestedCategory)
}
