package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantTitleMaskedOnceAndDoesNotBlockAnswer(t *testing.T) {
	entered := make(chan string, 1)
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		entered <- string(body)
		<-release
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"Paiement de la sortie"}],"usage":{"input_tokens":33,"output_tokens":8}}`)
	}))
	defer provider.Close()
	stub := &streamStub{replies: []string{sseText(t, "Réponse privée")}}
	e := newTestEnv(t, withAssistant(t, stub, 50), func(d *Deps) {
		d.Config.Summary = &config.LLM{BaseURL: mustURL(t, provider.URL), APIKey: "sk-test", Model: "test-summary", Timeout: 2 * time.Second, Prices: config.Prices{Set: true, Input: 300_000, Output: 1_200_000}}
	})
	var releaseOnce sync.Once
	releaseTitle := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { releaseTitle(); e.srv.WaitAssistantTitles() }()
	cookie := e.login(t)
	started := time.Now()
	_, events, _ := e.ask(t, cookie, url.Values{"text": {"Paiement : private@example.org, 06 12 34 56 78"}})
	require.Len(t, events.of("done"), 1)
	assert.Less(t, time.Since(started), time.Second, "title provider must not block the answer")
	id := events.of("start")[0]["conversation"].(string)
	select {
	case body := <-entered:
		assert.NotContains(t, body, "private@example.org")
		assert.NotContains(t, body, "06 12 34 56 78")
		assert.NotContains(t, body, "Réponse privée")
	case <-time.After(time.Second):
		t.Fatal("no title request")
	}
	for range 2 {
		status, _ := e.page(t, cookie, "/assistant")
		require.Equal(t, http.StatusOK, status)
	}
	// Release the provider early, while keeping the deferred close harmless.
	releaseTitle()
	e.srv.WaitAssistantTitles()
	c, err := e.srv.convs.Find(t.Context(), "alice", id)
	require.NoError(t, err)
	assert.Equal(t, "Paiement de la sortie", c.Title)
	_, followUp, _ := e.ask(t, cookie, url.Values{"text": {"Suite"}, "conversation": {id}})
	require.Len(t, followUp.of("done"), 1)
	e.srv.WaitAssistantTitles()
	assert.Empty(t, entered, "reads and follow-ups never retry title generation")
	var calls, input, output int
	require.NoError(t, e.db.QueryRowContext(t.Context(), `SELECT COUNT(*),SUM(input_tokens),SUM(output_tokens) FROM suggest_usage`).Scan(&calls, &input, &output))
	assert.Equal(t, 1, calls)
	assert.Equal(t, 33, input)
	assert.Equal(t, 8, output)
	assert.NotContains(t, e.logs.String(), "private@example.org")
}
func TestAssistantTitleFailureKeepsFallback(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"content":[{"type":"text","text":""}],"usage":{"input_tokens":9}}`)
			}))
			defer provider.Close()
			stub := &streamStub{replies: []string{sseText(t, "R")}}
			e := newTestEnv(t, withAssistant(t, stub, 50), func(d *Deps) {
				d.Config.Summary = &config.LLM{BaseURL: mustURL(t, provider.URL), APIKey: "sk-test", Model: "test-summary", Timeout: time.Second}
			})
			defer e.srv.WaitAssistantTitles()
			cookie := e.login(t)
			_, events, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
			require.Len(t, events.of("done"), 1)
			e.srv.WaitAssistantTitles()
			id := events.of("start")[0]["conversation"].(string)
			c, err := e.srv.convs.Find(t.Context(), "alice", id)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(c.Title, "Conversation du "))
			assert.Equal(t, 1, e.count(t, "suggest_usage"))
		})
	}
}
