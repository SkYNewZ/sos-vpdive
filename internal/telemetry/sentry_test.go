package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const testDSN = "https://public@sentry.example.org/42"

// newTestSentry returns a logger feeding a Sentry client whose events stay
// in memory.
func newTestSentry(t *testing.T, logs *bytes.Buffer) (*slog.Logger, *sentry.Client, *sentry.MockTransport) {
	t.Helper()
	transport := &sentry.MockTransport{}
	client, err := NewSentry(SentryOptions{DSN: testDSN, Environment: "test", Release: "v1.2.3", Transport: transport})
	require.NoError(t, err)
	t.Cleanup(client.Close)
	return WithSentry(context.Background(), NewLogger(logs, slog.LevelInfo), client), client, transport
}

// errorEvents are the error events the transport received, logs left out.
func errorEvents(transport *sentry.MockTransport) []*sentry.Event {
	var out []*sentry.Event
	for _, e := range transport.Events() {
		if e.Type == "" {
			out = append(out, e)
		}
	}
	return out
}

// sentLogs are the Sentry log entries the transport received.
func sentLogs(transport *sentry.MockTransport) []sentry.Log {
	var out []sentry.Log
	for _, e := range transport.Events() {
		out = append(out, e.Logs...)
	}
	return out
}

func TestErrorRecordBecomesAnEventOnTheTrace(t *testing.T) {
	var logs bytes.Buffer
	logger, client, transport := newTestSentry(t, &logs)
	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "POST /demandes")

	logger.ErrorContext(ctx, "internal error", "error", errors.New("database is locked"), "page", "form")
	span.End()
	require.True(t, client.Flush(time.Second))

	events := errorEvents(transport)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, sentry.LevelError, e.Level)
	assert.Equal(t, "internal error", e.Message)
	require.NotEmpty(t, e.Exception)
	assert.Equal(t, "database is locked", e.Exception[len(e.Exception)-1].Value)
	assert.NotNil(t, e.Exception[len(e.Exception)-1].Stacktrace)
	assert.Equal(t, span.SpanContext().TraceID().String(), e.Contexts["trace"]["trace_id"])
	assert.Equal(t, span.SpanContext().SpanID().String(), e.Contexts["trace"]["span_id"])
	assert.Equal(t, "POST /demandes", e.Transaction, "the route template, never the path")
	assert.Equal(t, "form", e.Contexts["log"]["page"])
	assert.Equal(t, "v1.2.3", e.Release)
	assert.Equal(t, "test", e.Environment)
	assert.Contains(t, logs.String(), `"msg":"internal error"`, "the JSON log line is still written")
}

func TestErrorRecordWithoutCauseKeepsTheCallerStack(t *testing.T) {
	logger, client, transport := newTestSentry(t, &bytes.Buffer{})
	logger.With("job", "purge").Error("handler panic", "type", "runtime.boundsError")
	require.True(t, client.Flush(time.Second))

	events := errorEvents(transport)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, "handler panic", e.Message)
	assert.Empty(t, e.Exception)
	require.Len(t, e.Threads, 1)
	assert.NotNil(t, e.Threads[0].Stacktrace)
	assert.Equal(t, "runtime.boundsError", e.Contexts["log"]["type"])
	assert.Equal(t, "purge", e.Contexts["log"]["job"], "attributes from With are kept")
}

func TestWarningsAreLogsNotEvents(t *testing.T) {
	logger, client, transport := newTestSentry(t, &bytes.Buffer{})
	logger.Warn("mail delivery postponed", "stage", "smtp_unavailable")
	logger.Info("request", "route", "GET /")
	logger.Debug("write response") // below LOG_LEVEL: neither stdout nor Sentry
	require.True(t, client.Flush(time.Second))

	assert.Empty(t, errorEvents(transport))
	sent := sentLogs(transport)
	bodies := make([]string, 0, len(sent))
	for _, l := range sent {
		bodies = append(bodies, l.Body)
	}
	assert.ElementsMatch(t, []string{"mail delivery postponed", "request"}, bodies)
}

func TestScrubDropsRequestAndUser(t *testing.T) {
	_, client, transport := newTestSentry(t, &bytes.Buffer{})
	event := sentry.NewEvent()
	event.Message = "internal error"
	event.Request = &sentry.Request{
		URL:     "https://sos.example.org/suivi/secret-token",
		Data:    "nom=Dupont",
		Cookies: "__Host-session=abc",
		Headers: map[string]string{"Referer": "https://sos.example.org/suivi/secret-token"},
		Env:     map[string]string{"REMOTE_ADDR": "192.0.2.10"},
	}
	event.User = sentry.User{ID: "alice", Email: "alice@example.org", IPAddress: "192.0.2.10"}
	client.CaptureEvent(event, nil, nil)
	require.True(t, client.Flush(time.Second))

	events := errorEvents(transport)
	require.Len(t, events, 1)
	assert.Nil(t, events[0].Request)
	assert.True(t, events[0].User.IsEmpty())
}

func TestUnreachableSentryNeverSlowsTheCaller(t *testing.T) {
	client, err := NewSentry(SentryOptions{DSN: "http://public@127.0.0.1:1/42"})
	require.NoError(t, err)
	t.Cleanup(client.Close)
	logger := WithSentry(context.Background(), NewLogger(&bytes.Buffer{}, slog.LevelInfo), client)

	start := time.Now()
	for range 20 {
		logger.Error("internal error", "error", errors.New("boom"))
	}
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

// fakeSentry stands for sentry.io: it counts what reaches each path.
type fakeSentry struct {
	mu   sync.Mutex
	hits map[string]int
}

func (f *fakeSentry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[r.URL.Path]++
	w.WriteHeader(http.StatusOK)
}

func (f *fakeSentry) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func TestSetupSendsSpansErrorsAndLogsToSentry(t *testing.T) {
	restoreGlobalProvider(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	fake := &fakeSentry{hits: map[string]int{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dsn := "http://public@" + strings.TrimPrefix(srv.URL, "http://") + "/42"

	var logs bytes.Buffer
	logger, shutdown, err := Setup(context.Background(), NewLogger(&logs, slog.LevelInfo),
		SentryOptions{DSN: dsn, Environment: "test", Release: "dev"})
	require.NoError(t, err)
	ctx, span := otel.Tracer("test").Start(context.Background(), "GET /")
	logger.ErrorContext(ctx, "internal error", "error", errors.New("boom"))
	span.End()
	require.NoError(t, shutdown(context.Background()))

	assert.Positive(t, fake.count("/api/42/integration/otlp/v1/traces/"), "spans go to Sentry over OTLP")
	assert.Positive(t, fake.count("/api/42/envelope/"), "the error event and the logs")
	assert.NotContains(t, logs.String(), "trace export failed")
}

func TestSetupWithInvalidDSNRunsWithoutSentry(t *testing.T) {
	restoreGlobalProvider(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	var logs bytes.Buffer
	base := NewLogger(&logs, slog.LevelInfo)
	logger, shutdown, err := Setup(context.Background(), base, SentryOptions{DSN: "not a dsn"})
	require.NoError(t, err, "never a startup failure")
	assert.Same(t, base, logger)
	assert.Contains(t, logs.String(), `"level":"WARN","msg":"sentry is off: invalid SENTRY_DSN"`)
	require.NoError(t, shutdown(context.Background()))
}
