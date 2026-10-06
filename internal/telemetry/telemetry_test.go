package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func restoreGlobalProvider(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
}

func TestLoggerAddsTraceIDs(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).InfoContext(ctx, "hello")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, span.SpanContext().TraceID().String(), rec["trace_id"])
	assert.Equal(t, span.SpanContext().SpanID().String(), rec["span_id"])
}

func TestLoggerWithoutSpanHasNoTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).With("k", "v").InfoContext(context.Background(), "hello")
	assert.NotContains(t, buf.String(), "trace_id")
	assert.Contains(t, buf.String(), `"k":"v"`)
}

func TestSetupWithoutEndpointExportsNothing(t *testing.T) {
	restoreGlobalProvider(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	var logs bytes.Buffer
	_, shutdown, err := Setup(context.Background(), NewLogger(&logs, slog.LevelDebug), SentryOptions{})
	require.NoError(t, err)

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	span.End()

	// With an exporter, Shutdown would try localhost:4318 and fail.
	require.NoError(t, shutdown(context.Background()))
	assert.Empty(t, logs.String())
}

func TestSetupExportsToConfiguredEndpoint(t *testing.T) {
	restoreGlobalProvider(t)
	var hits atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_SERVICE_NAME", "")

	_, shutdown, err := Setup(context.Background(), NewLogger(&bytes.Buffer{}, slog.LevelInfo), SentryOptions{})
	require.NoError(t, err)
	_, span := otel.Tracer("test").Start(context.Background(), "op")
	span.End()
	require.NoError(t, shutdown(context.Background()))

	assert.Positive(t, hits.Load())
}

func TestFail(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	_, span := tp.Tracer("test").Start(context.Background(), "op")
	Fail(span, "db_error")
	span.End()

	ended := rec.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Equal(t, "db_error", ended[0].Status().Description)
	assert.Contains(t, ended[0].Attributes(), attribute.String("error.code", "db_error"))
}
