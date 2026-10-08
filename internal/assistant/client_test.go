package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// q quotes s as a JSON string.
func q(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// sse writes events the way the Messages API streams them.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return b.String()
}

const messageStart = `{"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":80,"output_tokens":1}}}`

// textStream is a reply of one text block, cut into deltas.
func textStream(deltas ...string) string {
	events := []string{messageStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}
	for _, d := range deltas {
		events = append(events, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+q(d)+`}}`)
	}
	return sse(append(events, `{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`)...)
}

// toolStream is a reply asking for tools, each input cut in two fragments.
func toolStream(thinking bool, calls ...[3]string) string { // id, name, input
	events := []string{messageStart}
	i := 0
	if thinking {
		events = append(events,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Chercher Léa."}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`)
		i++
	}
	for _, c := range calls {
		idx := strconv.Itoa(i)
		half := len(c[2]) / 2
		events = append(events,
			`{"type":"content_block_start","index":`+idx+`,"content_block":{"type":"tool_use","id":`+q(c[0])+`,"name":`+q(c[1])+`,"input":{}}}`,
			`{"type":"content_block_delta","index":`+idx+`,"delta":{"type":"input_json_delta","partial_json":`+q(c[2][:half])+`}}`,
			`{"type":"content_block_delta","index":`+idx+`,"delta":{"type":"input_json_delta","partial_json":`+q(c[2][half:])+`}}`,
			`{"type":"content_block_stop","index":`+idx+`}`)
		i++
	}
	return sse(append(events, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`,
		`{"type":"message_stop"}`)...)
}

// scripted stands for the provider: each call gets the next reply, or the
// last one again, and the request bodies are kept.
type scripted struct {
	mu      sync.Mutex
	replies []string
	status  int
	hold    bool // never answers: waits for the request to end
	bodies  []string
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, string(body))
	reply := ""
	if len(s.replies) > 0 {
		reply = s.replies[0]
		if len(s.replies) > 1 {
			s.replies = s.replies[1:]
		}
	}
	status, hold := s.status, s.hold
	s.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if hold {
		_, _ = io.WriteString(w, sse(messageStart))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	_, _ = io.WriteString(w, reply)
}

// raw is the body of call i, as sent.
func (s *scripted) raw(t *testing.T, i int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Greater(t, len(s.bodies), i)
	return s.bodies[i]
}

// body is the body of call i, decoded.
func (s *scripted) body(t *testing.T, i int) map[string]any {
	t.Helper()
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(s.raw(t, i)), &v))
	return v
}

// fail makes every later call answer status.
//
//nolint:unused // the answer loop tests (Task 6) use it, then this directive goes
func (s *scripted) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func newTestClient(t *testing.T, s http.Handler, thinking bool) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return NewClient(&config.LLM{BaseURL: u, APIKey: "sk-test"}, "test-model", thinking)
}

func userMessages(t *testing.T, text string) []Message {
	t.Helper()
	m, err := UserText(text)
	require.NoError(t, err)
	return []Message{m}
}

func TestStreamText(t *testing.T) {
	s := &scripted{replies: []string{textStream("Bonjour ", "Léa.")}}
	c := newTestClient(t, s, false)
	var seen []string
	rep, err := c.stream(context.Background(), call{system: "Consignes", messages: userMessages(t, "Salut"),
		onText: func(d string) { seen = append(seen, d) }})
	require.NoError(t, err)
	assert.Equal(t, []string{"Bonjour ", "Léa."}, seen)
	assert.Equal(t, "Bonjour Léa.", rep.Text)
	assert.Equal(t, "end_turn", rep.StopReason)
	assert.Equal(t, Usage{Input: 120, Output: 42, CacheRead: 80}, rep.Usage)
	require.Len(t, rep.Content, 1)
	assert.JSONEq(t, `{"type":"text","text":"Bonjour Léa."}`, string(rep.Content[0]))

	body := s.body(t, 0)
	assert.Equal(t, true, body["stream"])
	assert.Equal(t, "test-model", body["model"])
	assert.InDelta(t, 1500, body["max_tokens"], 0)
	assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "Consignes", "cache_control": map[string]any{"type": "ephemeral"}}}, body["system"])
	assert.NotContains(t, body, "tool_choice")
}

func TestStreamToolsAndThinking(t *testing.T) {
	s := &scripted{replies: []string{toolStream(true,
		[3]string{"call_1", "find_member", `{"query":"Léa Martin"}`},
		[3]string{"call_2", "read_fiche", `{"id":"carnet-solde-negatif"}`})}}
	c := newTestClient(t, s, true)
	thought := 0
	rep, err := c.stream(context.Background(), call{system: "S", messages: userMessages(t, "Q"), noTools: true,
		onThink: func() { thought++ }})
	require.NoError(t, err)
	assert.Equal(t, 1, thought)
	assert.Equal(t, "tool_use", rep.StopReason)
	require.Len(t, rep.Content, 3)
	assert.JSONEq(t, `{"type":"thinking","thinking":"Chercher Léa.","signature":"sig"}`, string(rep.Content[0]))
	assert.JSONEq(t, `{"type":"tool_use","id":"call_1","name":"find_member","input":{"query":"Léa Martin"}}`, string(rep.Content[1]))
	require.Len(t, rep.ToolUses, 2)
	assert.Equal(t, "read_fiche", rep.ToolUses[1].Name)
	assert.JSONEq(t, `{"id":"carnet-solde-negatif"}`, string(rep.ToolUses[1].Input))

	body := s.body(t, 0)
	assert.InDelta(t, 4000, body["max_tokens"], 0)
	assert.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(2048)}, body["thinking"])
	assert.Equal(t, map[string]any{"type": "none"}, body["tool_choice"])
}

func TestStreamFailures(t *testing.T) {
	cases := map[string]struct {
		s    *scripted
		want error
	}{
		"status": {&scripted{status: http.StatusServiceUnavailable,
			replies: []string{`{"type":"error","error":{"type":"overloaded_error","message":"Léa Martin"}}`}}, ErrHTTP},
		"error event": {&scripted{replies: []string{sse(messageStart, `{"type":"error","error":{"type":"overloaded_error"}}`)}}, ErrHTTP},
		"no stop":     {&scripted{replies: []string{sse(messageStart)}}, ErrInvalid},
		"bad tool input": {&scripted{replies: []string{sse(messageStart,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"outing","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"id\":"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`)}}, ErrInvalid},
		"cut": {&scripted{replies: []string{strings.Replace(textStream("Lé"), "end_turn", "max_tokens", 1)}}, ErrInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newTestClient(t, tc.s, false).stream(context.Background(), call{messages: userMessages(t, "Q")})
			require.ErrorIs(t, err, tc.want)
			assert.NotContains(t, err.Error(), "Léa", "a provider message is never quoted")
		})
	}
}

func TestStreamRefusesRedirects(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	t.Cleanup(target.Close)
	c := newTestClient(t, http.RedirectHandler(target.URL, http.StatusTemporaryRedirect), false)
	_, err := c.stream(context.Background(), call{messages: userMessages(t, "Q")})
	require.ErrorIs(t, err, ErrHTTP)
	assert.False(t, followed, "the key never goes to another host")
}

func TestStreamIdleAndCancel(t *testing.T) {
	old := idleTimeout
	idleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { idleTimeout = old })
	c := newTestClient(t, &scripted{hold: true}, false)
	rep, err := c.stream(context.Background(), call{messages: userMessages(t, "Q")})
	require.ErrorIs(t, err, ErrTimeout)
	assert.Equal(t, "timeout", Code(err))
	assert.Equal(t, 120, rep.Usage.Input, "the usage read before the cut is kept for the journal")

	idleTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	rep, err = c.stream(ctx, call{messages: userMessages(t, "Q")})
	require.ErrorIs(t, err, ErrCanceled)
	assert.Equal(t, "canceled", Code(err))
	assert.Equal(t, 80, rep.Usage.CacheRead)
	assert.Equal(t, "internal", Code(errors.New("disk full")))
}
