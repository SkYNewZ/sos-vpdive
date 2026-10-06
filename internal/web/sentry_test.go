package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestPanicReachesSentryOnTheRequestTrace(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)
	h := e.srv.instrument("GET /panique", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = r.URL.Path[len(r.URL.Path)] // a runtime panic: index out of range
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/panique", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	i := slices.IndexFunc(spans.Ended(), func(s sdktrace.ReadOnlySpan) bool { return s.Name() == "GET /panique" })
	require.NotEqual(t, -1, i)
	span := spans.Ended()[i]
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "http_500", span.Status().Description)
	assert.Contains(t, e.logs.String(), `"msg":"request","method":"GET","route":"GET /panique","status":500`)

	events := e.sentryEvents(t)
	require.Len(t, events, 1)
	ev := events[0]
	assert.Equal(t, "handler panic", ev.Message)
	assert.Equal(t, span.SpanContext().TraceID().String(), ev.Contexts["trace"]["trace_id"])
	assert.Equal(t, span.SpanContext().SpanID().String(), ev.Contexts["trace"]["span_id"])
	assert.Equal(t, "GET /panique", ev.Transaction)
	require.Len(t, ev.Threads, 1)
	assert.True(t, slices.ContainsFunc(ev.Threads[0].Stacktrace.Frames, func(f sentry.Frame) bool {
		return strings.HasPrefix(f.Function, "TestPanicReachesSentryOnTheRequestTrace")
	}), "the stack shows the panic site")
}

// The §13 privacy criterion: a request filed with witness values fails, and
// nothing Sentry receives carries them.
func TestSubmissionErrorReachesSentryWithoutPersonalData(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	key := e.formKey(t)
	_, err := e.db.ExecContext(context.Background(), "DROP TABLE attachments")
	require.NoError(t, err)
	v := validRequest(key)
	v.Set("prenom", "Witnessfirst")
	v.Set("nom", "Witnesslast")
	v.Set("description", "Witness description text for Sentry")
	const token = "WitnessTrackingToken0123456789abcdefghijklm"

	body, contentType := multipartBody(t, v, pngBytes(t))
	rec := e.do(t, http.MethodPost, publicHost, "/demandes", body, contentType, func(r *http.Request) {
		r.Header.Set("Referer", "https://"+publicHost+"/suivi/"+token)
		r.AddCookie(&http.Cookie{Name: "witness", Value: "WitnessCookieValue"})
	})
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	events := e.sentryEvents(t)
	require.Len(t, events, 1)
	assert.Equal(t, "internal error", events[0].Message)
	payload := strings.ToLower(e.sentryPayload(t))
	for _, witness := range []string{
		"witnessfirst", "witnesslast", "lea.martin", "witness description", token, key,
		"witnesscookievalue", "referer", "/suivi/",
	} {
		assert.NotContains(t, payload, strings.ToLower(witness))
	}
}

// Expected refusals are answered, not reported (spec §9.10).
func TestExpectedRefusalsCreateNoSentryEvent(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")

	invalid := validRequest(e.formKey(t))
	invalid.Set("description", "")
	assert.Equal(t, http.StatusUnprocessableEntity, e.sendRequest(t, invalid).Code)

	stranger := validRequest(e.formKey(t))
	stranger.Set("email", "nobody@example.org")
	assert.Equal(t, http.StatusUnprocessableEntity, e.sendRequest(t, stranger).Code)

	assert.Empty(t, e.sentryEvents(t))
}
