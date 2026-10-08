package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// streamStub stands for the provider in streaming mode: each call gets the
// next scripted stream, or the last one again; status answers an error.
type streamStub struct {
	mu      sync.Mutex
	replies []string
	status  int
	delay   time.Duration
	bodies  []string
}

func (m *streamStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.bodies = append(m.bodies, string(body))
	reply, status, delay := "", m.status, m.delay
	if len(m.replies) > 0 {
		reply = m.replies[0]
		if len(m.replies) > 1 {
			m.replies = m.replies[1:]
		}
	}
	m.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		http.Error(w, `{"type":"error","error":{"type":"api_error"}}`, status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, reply)
}

func (m *streamStub) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

// withAssistant turns the assistant on against stub, limit questions a day.
func withAssistant(t *testing.T, stub *streamStub, limit int) func(*Deps) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return func(d *Deps) {
		d.Config.LLM = &config.LLM{BaseURL: mustURL(t, srv.URL), APIKey: "sk-test", Model: "test-model", Timeout: time.Second, DailyLimit: 200}
		d.Config.Assistant = &config.Assistant{Model: "test-model", MaxTokens: 8000, DailyQuestions: limit,
			Priced: true, PriceInput: 300_000, PriceOutput: 1_200_000, PriceCached: 30_000}
	}
}

// sseEvents writes stream events as the provider does.
func sseEvents(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return b.String()
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

const sseStart = `{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}`

// sseText is a final answer streamed in the given deltas.
func sseText(t *testing.T, deltas ...string) string {
	t.Helper()
	events := []string{sseStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}
	for _, d := range deltas {
		events = append(events, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+jsonString(t, d)+`}}`)
	}
	return sseEvents(append(events, `{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`, `{"type":"message_stop"}`)...)
}

// sseTool asks for one tool call.
func sseTool(t *testing.T, name, input string) string {
	t.Helper()
	return sseEvents(sseStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"`+name+`","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+jsonString(t, input)+`}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`, `{"type":"message_stop"}`)
}

// streamEvents is a decoded NDJSON answer.
type streamEvents []map[string]any

func (ev streamEvents) of(typ string) []map[string]any {
	var out []map[string]any
	for _, e := range ev {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

// ask posts a question as cookie's session and decodes the stream.
func (e *testEnv) ask(t *testing.T, cookie *http.Cookie, v url.Values) (int, streamEvents, string) {
	t.Helper()
	if v.Get("csrf") == "" {
		v.Set("csrf", e.csrf(t, cookie, "/assistant"))
	}
	rec := e.postAs(t, cookie, "/assistant/messages", v)
	if rec.Code != http.StatusOK {
		return rec.Code, nil, rec.Body.String()
	}
	var out streamEvents
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		var ev map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &ev), sc.Text())
		out = append(out, ev)
	}
	require.NoError(t, sc.Err())
	return rec.Code, out, ""
}

// askUntil posts a question under ctx, so that a test can leave mid-answer.
func (e *testEnv) askUntil(ctx context.Context, cookie *http.Cookie, v url.Values) {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/assistant/messages", formBody(v))
	req.Host, req.RemoteAddr = adminHost, "192.0.2.10:40000"
	req.Header.Set("Origin", "https://"+adminHost)
	formType(req)
	req.AddCookie(cookie)
	e.srv.ServeHTTP(httptest.NewRecorder(), req)
}

// loginBob adds bob, a committee member who is not the owner, and signs him in.
func (e *testEnv) loginBob(t *testing.T) *http.Cookie {
	t.Helper()
	e.addBob(t)
	rec := e.postLogin(t, "bob", testPassword)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	return sessionCookie(t, rec)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
