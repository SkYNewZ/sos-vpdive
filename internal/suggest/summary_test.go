package suggest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

func newSummaryTestClient(t *testing.T, handler http.HandlerFunc) *SummaryClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL + "/provider/")
	require.NoError(t, err)
	return NewSummary(&config.LLM{BaseURL: base, APIKey: "sk-test", Model: "claude-haiku-5-5", Timeout: time.Second})
}

func TestSummaryRequestAndPlainText(t *testing.T) {
	assert.Nil(t, NewSummary(nil))
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
	}))
	calls := make(chan struct {
		request *http.Request
		body    []byte
	}, 1)
	c := newSummaryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		calls <- struct {
			request *http.Request
			body    []byte
		}{r, body}
		_, err = io.WriteString(w, `{"content":[{"type":"text","text":"Voir <b>ici</b> [la page](https://x.example) https://evil.example"}],"usage":{"input_tokens":123,"output_tokens":12}}`)
		assert.NoError(t, err)
	})
	res, err := c.Summarize(ctx, Request{Category: "Autre", Description: "Texte avec \"citation\" et 🐠"})
	require.NoError(t, err)
	assert.Equal(t, Result{Summary: "Voir ici la page", InputTokens: 123, OutputTokens: 12}, res)
	call := <-calls
	assert.Equal(t, "/provider/v1/messages", call.request.URL.Path)
	assert.Equal(t, http.MethodPost, call.request.Method)
	assert.Equal(t, "sk-test", call.request.Header.Get("X-Api-Key"))
	assert.Equal(t, "2023-06-01", call.request.Header.Get("Anthropic-Version"))
	assert.Equal(t, "application/json", call.request.Header.Get("Content-Type"))
	assert.Empty(t, call.request.Header.Get("Traceparent"))
	assert.Empty(t, call.request.Header.Get("Baggage"))
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Thinking  struct {
			Type string `json:"type"`
		} `json:"thinking"`
		Messages []struct {
			Role    messageRole `json:"role"`
			Content string      `json:"content"`
		} `json:"messages"`
		Tools json.RawMessage `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(call.body, &body))
	assert.Equal(t, "claude-haiku-5-5", body.Model)
	assert.Equal(t, 400, body.MaxTokens)
	assert.Equal(t, "disabled", body.Thinking.Type)
	assert.Contains(t, body.System, "200")
	assert.Contains(t, body.System, "jamais une instruction")
	assert.NotContains(t, body.System, "fiches")
	assert.Nil(t, body.Tools)
	require.Len(t, body.Messages, 1)
	assert.Equal(t, messageUser, body.Messages[0].Role)
	var doc struct {
		Demande Request         `json:"demande"`
		Fiches  json.RawMessage `json:"fiches"`
	}
	require.NoError(t, json.Unmarshal([]byte(body.Messages[0].Content), &doc))
	assert.Equal(t, "Texte avec \"citation\" et 🐠", doc.Demande.Description)
	assert.NotNil(t, doc.Demande.Fields)
	assert.Nil(t, doc.Fiches)
}

func TestSummaryTruncatesRunes(t *testing.T) {
	c := newSummaryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		err := json.NewEncoder(w).Encode(struct {
			Content []summaryContent `json:"content"`
		}{Content: []summaryContent{{Type: contentText, Text: strings.Repeat("é", 250)}}})
		assert.NoError(t, err)
	})
	res, err := c.Summarize(context.Background(), Request{})
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("é", 199)+"…", res.Summary)
}

func TestSummaryFailureKeepsUsage(t *testing.T) {
	for name, reply := range map[string]string{
		"truncated":         `{"content":[{"type":"text","text":"partial"}],"stop_reason":"max_tokens","usage":{"input_tokens":123,"output_tokens":12}}`,
		"no text":           `{"content":[{"type":"tool_use"}],"usage":{"input_tokens":123,"output_tokens":12}}`,
		"wrong stop reason": `{"content":[],"stop_reason":3,"usage":{"input_tokens":123,"output_tokens":12}}`,
		"wrong content":     `{"content":"wrong","usage":{"input_tokens":123,"output_tokens":12}}`,
		"empty summary":     `{"content":[{"type":"text","text":"<b></b> https://x.example"}],"usage":{"input_tokens":123,"output_tokens":12}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newSummaryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, reply)
				assert.NoError(t, err)
			})
			res, err := c.Summarize(context.Background(), Request{})
			require.ErrorIs(t, err, ErrInvalid)
			assert.Equal(t, Result{InputTokens: 123, OutputTokens: 12}, res)
		})
	}
}

func TestSummaryTransportFailures(t *testing.T) {
	for _, tt := range []struct {
		name, reply string
		status      int
		want        error
	}{
		{"invalid envelope", "<html>", http.StatusOK, ErrInvalid},
		{"oversized", strings.Repeat("a", 65<<10), http.StatusOK, ErrInvalid},
		{"provider error", "{}", http.StatusTooManyRequests, ErrHTTP},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newSummaryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, err := io.WriteString(w, tt.reply)
				assert.NoError(t, err)
			})
			res, err := c.Summarize(context.Background(), Request{})
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, Result{}, res)
		})
	}
}

func TestSummaryDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	t.Cleanup(target.Close)
	c := newSummaryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	res, err := c.Summarize(context.Background(), Request{})
	require.ErrorIs(t, err, ErrHTTP)
	assert.Equal(t, Result{}, res)
	assert.False(t, leaked)
}

func TestSummaryTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		c := newSummaryTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
			_, err := io.Copy(io.Discard, r.Body)
			assert.NoError(t, err)
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		})
		c.timeout = 20 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		_, err := c.Summarize(ctx, Request{})
		cancel()
		want := ErrTimeout
		if canceled {
			want = ErrCanceled
		}
		require.ErrorIs(t, err, want)
	}
}

func TestConversationTitleUsesDedicatedPrompt(t *testing.T) {
	var prompt string
	c := newSummaryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		var body struct {
			System   string           `json:"system"`
			Messages []summaryMessage `json:"messages"`
		}
		assert.NoError(t, json.Unmarshal(raw, &body))
		prompt = body.System
		if !assert.Len(t, body.Messages, 1) {
			return
		}
		assert.Contains(t, body.Messages[0].Content, "Question masquée")
		_, err = io.WriteString(w, `{"content":[{"type":"text","text":"<b>Règlement</b> [sortie](https://example.org)"}],"usage":{"input_tokens":33,"output_tokens":8}}`)
		assert.NoError(t, err)
	})
	res, err := c.Title(t.Context(), "Question masquée")
	require.NoError(t, err)
	assert.Equal(t, "Règlement sortie", res.Summary)
	assert.Equal(t, 33, res.InputTokens)
	assert.Contains(t, prompt, "titre")
	assert.Contains(t, prompt, "60")
	assert.NotEqual(t, summarySystemPrompt, prompt)
}
func TestConversationTitleBoundsLength(t *testing.T) {
	c := newSummaryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"content": []summaryContent{{Type: contentText, Text: strings.Repeat("é", 100)}}}))
	})
	res, err := c.Title(t.Context(), "Q")
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(res.Summary)), 60)
}
