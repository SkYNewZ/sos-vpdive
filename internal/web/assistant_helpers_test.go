package web

import (
	"io"
	"net/http"
	"net/http/httptest"
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

// withAssistant turns the assistant on against stub, limit questions a day.
func withAssistant(t *testing.T, stub *streamStub, limit int) func(*Deps) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return func(d *Deps) {
		d.Config.LLM = &config.LLM{BaseURL: mustURL(t, srv.URL), APIKey: "sk-test", Model: "test-model", Timeout: time.Second, DailyLimit: 200}
		d.Config.Assistant = &config.Assistant{Model: "test-model", DailyQuestions: limit,
			Priced: true, PriceInput: 300_000, PriceOutput: 1_200_000, PriceCached: 30_000}
	}
}

// loginBob adds bob, a committee member who is not the owner, and signs him in.
func (e *testEnv) loginBob(t *testing.T) *http.Cookie {
	t.Helper()
	e.addBob(t)
	rec := e.postLogin(t, "bob", testPassword)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	return sessionCookie(t, rec)
}
