package web

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantHistoryReturnAndNewConversation(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Réponse à retrouver")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Une question"}})
	require.Len(t, ev.of("done"), 1)
	id := ev.of("start")[0]["conversation"].(string)
	status, page := e.page(t, cookie, "/assistant")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, page, `data-conversation="`+id+`"`)
	assert.Contains(t, page, "Réponse à retrouver")
	assert.Contains(t, page, `data-assistant-history`)
	_, page = e.page(t, cookie, "/assistant?nouvelle=1")
	assert.Contains(t, page, `data-conversation=""`)
	assert.NotContains(t, page, "Réponse à retrouver")
	bob := e.loginBob(t)
	_, page = e.page(t, bob, "/assistant")
	assert.NotContains(t, page, "Une question")
	assert.NotContains(t, page, id)
	for _, suffix := range []string{"?page=-1", "?page=99999999999999999999999999", "?page=oops"} {
		status, _ = e.page(t, cookie, "/assistant"+suffix)
		assert.Equal(t, http.StatusOK, status)
	}
}
func TestAssistantHistoryExpiredIsReadOnly(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Ancienne réponse")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("done"), 1)
	id := ev.of("start")[0]["conversation"].(string)
	e.clock.advance(30 * time.Minute)
	status, page := e.page(t, cookie, "/assistant/"+id)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, page, "Ancienne réponse")
	assert.NotContains(t, page, `data-assistant-form`)
	assert.Contains(t, page, "Cette conversation est terminée")
	assert.Contains(t, page, `href="/assistant?nouvelle=1"`)
	before := len(stub.calls())
	status, _, _ = e.ask(t, cookie, url.Values{"text": {"Suite"}, "conversation": {id}})
	assert.Equal(t, http.StatusGone, status)
	assert.Len(t, stub.calls(), before)
	e.clock.advance(7 * 24 * time.Hour)
	status, _ = e.page(t, cookie, "/assistant/"+id)
	assert.Equal(t, http.StatusNotFound, status)
}
func TestAssistantHistoryClosedTicketCannotResumeFromStandalonePage(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Analyse privée")}}
	e, cookie := assistantEnv(t, stub, 50)
	ticket := e.submitTicket(t, "hugo.bernard@example.org")
	_, ev, _ := e.ask(t, cookie, url.Values{"demande": {itoa(ticket.ID)}})
	require.Len(t, ev.of("done"), 1)
	id := ev.of("start")[0]["conversation"].(string)
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, ticket.ID, e.openTicket(t, cookie, ticket.ID), url.Values{"action": {"take"}}).Code)
	require.Equal(t, http.StatusSeeOther, e.act(t, cookie, ticket.ID, e.openTicket(t, cookie, ticket.ID), url.Values{"action": {"close"}}).Code)
	status, page := e.page(t, cookie, "/assistant/"+id)
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, page, `data-assistant-form`)
	before := len(stub.calls())
	status, _, _ = e.ask(t, cookie, url.Values{"text": {"Suite"}, "conversation": {id}})
	assert.Equal(t, http.StatusConflict, status)
	assert.Len(t, stub.calls(), before)
}

func TestAssistantHistorySaveFailureIsExplicit(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Réponse temporaire")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, err := e.db.ExecContext(t.Context(), `CREATE TRIGGER fail_conversation_save BEFORE UPDATE OF transcript ON assistant_conversations BEGIN SELECT RAISE(ABORT,'synthetic disk failure'); END`)
	require.NoError(t, err)
	_, events, _ := e.ask(t, cookie, url.Values{"text": {"Question"}})
	assert.Empty(t, events.of("done"))
	require.Len(t, events.of("error"), 1)
	assert.Contains(t, events.of("error")[0]["message"], "enregistr")
	assert.Equal(t, 0, e.count(t, "assistant_conversations"), "failed initial saves leave no empty conversation")
}

func TestAssistantHistoryRetentionWhenAssistantIsDisabled(t *testing.T) {
	e := newTestEnv(t)
	archives := assistant.NewStore(e.db, e.deps.Keys, e.clock.now)
	c, err := archives.Begin(t.Context(), "synthetic-session", "alice", "", 0, nil)
	require.NoError(t, err)
	c.Exchanges = []assistant.Exchange{{Question: "Question", Answer: "Réponse"}}
	_, err = archives.Finish(t.Context(), c)
	require.NoError(t, err)
	e.clock.advance(7 * 24 * time.Hour)
	require.NoError(t, e.srv.Purge(t.Context()))
	assert.Equal(t, 0, e.count(t, "assistant_conversations"))
}
