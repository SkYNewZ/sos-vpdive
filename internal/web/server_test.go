package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
)

func TestHostRouting(t *testing.T) {
	e := newTestEnv(t)
	tests := []struct {
		host, path string
		want       int
		contains   string
	}{
		{publicHost, "/", http.StatusOK, "pas encore ouvert"},
		{"SOS.Example.org:443", "/", http.StatusOK, "pas encore ouvert"}, // review focus 2
		{"sos.example.org:8443", "/", http.StatusNotFound, ""},
		{"unknown.example.org", "/", http.StatusNotFound, ""},
		{publicHost, "/connexion", http.StatusNotFound, ""},
		{publicHost, "/imports", http.StatusNotFound, ""},
		{adminHost, "/suivi/abc", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		rec := e.do(t, http.MethodGet, tt.host, tt.path, nil)
		assert.Equal(t, tt.want, rec.Code, "%s%s", tt.host, tt.path)
		assert.Contains(t, rec.Body.String(), tt.contains)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/", "/nope", "/robots.txt", "/static/fonts/OFL.txt"} {
		rec := e.do(t, http.MethodGet, publicHost, path, nil)
		h := rec.Header()
		assert.Equal(t, contentSecurityPolicy, h.Get("Content-Security-Policy"), path)
		assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), path)
		assert.NotEmpty(t, h.Get("Strict-Transport-Security"), path)
		assert.Equal(t, "noindex, nofollow, noai, noimageai", h.Get("X-Robots-Tag"), path)
		assert.Equal(t, "no-referrer", h.Get("Referrer-Policy"), path)
	}
	assert.Equal(t, "no-store", e.do(t, http.MethodGet, publicHost, "/", nil).Header().Get("Cache-Control"))
	assert.Contains(t, e.do(t, http.MethodGet, publicHost, "/", nil).Body.String(), `<meta name="robots" content="noindex, nofollow, noai, noimageai">`)
}

func TestStaticFilesAreVersioned(t *testing.T) {
	e := newTestEnv(t)
	url := e.srv.assets.URL("fonts/OFL.txt")
	require.Contains(t, url, "?v=")

	rec := e.do(t, http.MethodGet, adminHost, url, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))

	stale := e.do(t, http.MethodGet, adminHost, "/static/fonts/OFL.txt?v=old", nil)
	assert.Equal(t, "no-cache", stale.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, "/static/fonts/", nil).Code, "no listing")
}

func TestRobots(t *testing.T) {
	e := newTestEnv(t)
	for _, host := range []string{publicHost, adminHost} {
		rec := e.do(t, http.MethodGet, host, "/robots.txt", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		for _, want := range []string{"User-agent: GPTBot", "User-agent: Google-Extended", "Disallow: /", "User-agent: *\nAllow: /"} {
			assert.Contains(t, body, want, host)
		}
	}
	bot := func(r *http.Request) {
		r.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)")
	}
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, publicHost, "/", nil, bot).Code)
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, adminHost, "/connexion", nil, bot).Code)
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, publicHost, "/robots.txt", nil, bot).Code)
	extended := func(r *http.Request) { r.Header.Set("User-Agent", "Google-Extended") }
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, publicHost, "/", nil, extended).Code)
}

func TestOriginRequiredOnMutations(t *testing.T) {
	e := newTestEnv(t)
	origin := func(v string) func(*http.Request) {
		return func(r *http.Request) {
			if v == "" {
				r.Header.Del("Origin")
				return
			}
			r.Header.Set("Origin", v)
		}
	}
	for host, other := range map[string]string{publicHost: adminHost, adminHost: publicHost} {
		for _, o := range []string{"", "https://evil.example", "https://" + other, "http://" + host} {
			rec := e.do(t, http.MethodPost, host, "/", nil, origin(o))
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s with origin %q", host, o)
		}
	}
	// The right origin passes the check; no POST route exists yet.
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, publicHost, "/", nil).Code)
}

func TestHealthz(t *testing.T) {
	e := newTestEnv(t)
	for _, host := range []string{publicHost, adminHost} {
		rec := e.do(t, http.MethodGet, host, "/healthz", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "ok\n", rec.Body.String())
	}
	require.NoError(t, e.db.Close())
	assert.Equal(t, http.StatusServiceUnavailable, e.do(t, http.MethodGet, publicHost, "/healthz", nil).Code)
}

func TestInstrumentationNamesRoutesNeverPaths(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)

	e.do(t, http.MethodGet, adminHost, "/suivi/secret-token-123", nil)
	e.do(t, http.MethodGet, publicHost, "/", nil)
	e.do(t, http.MethodGet, publicHost, "/healthz", nil)

	names := make([]string, 0, len(rec.Ended()))
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
		for _, a := range s.Attributes() {
			assert.NotContains(t, a.Value.String(), "secret-token-123")
		}
	}
	assert.Contains(t, names, "unmatched")
	assert.Contains(t, names, "GET /")
	assert.NotContains(t, strings.Join(names, " "), "healthz", "healthz is not traced")
	assert.NotContains(t, e.logs.String(), "secret-token-123")
	assert.Contains(t, e.logs.String(), `"route":"GET /"`)
	assert.Contains(t, e.logs.String(), `"trace_id"`)
}

func TestTraceparentOnlyFromTrustedProxies(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	parent := func(remote string) func(*http.Request) {
		return func(r *http.Request) {
			r.RemoteAddr = remote
			r.Header.Set("Traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
		}
	}

	e.do(t, http.MethodGet, publicHost, "/", nil, parent("10.1.2.3:5000"))
	e.do(t, http.MethodGet, publicHost, "/", nil, parent("192.0.2.10:5000"))

	var spans []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "GET /" { // store.Open and CheckKey also emit spans
			spans = append(spans, s)
		}
	}
	require.Len(t, spans, 2)
	assert.Equal(t, traceID, spans[0].SpanContext().TraceID().String(), "trusted proxy")
	assert.NotEqual(t, traceID, spans[1].SpanContext().TraceID().String(), "untrusted peer")
}

func TestClientIP(t *testing.T) {
	e := newTestEnv(t)
	tests := []struct{ remote, xff, want string }{
		{"192.0.2.10:1", "198.51.100.7", "192.0.2.10"},
		{"10.0.0.2:1", "198.51.100.7, 10.0.0.9", "198.51.100.7"},
		{"10.0.0.2:1", "", "10.0.0.2"},
		{"10.0.0.2:1", "garbage", "10.0.0.2"},
	}
	for _, tt := range tests {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		require.NoError(t, err)
		r.RemoteAddr = tt.remote
		if tt.xff != "" {
			r.Header.Set("X-Forwarded-For", tt.xff)
		}
		assert.Equal(t, tt.want, e.srv.clientIP(r).String(), "%s via %q", tt.remote, tt.xff)
	}
}

func TestEmbeddedContentIsValid(t *testing.T) {
	_, err := loadRobots(sosvpdive.Content)
	require.NoError(t, err)
	links, err := loadVPDiveLinks(sosvpdive.Content, mustURL(t, "https://vpdive.example.org"))
	require.NoError(t, err)
	assert.Equal(t, "https://vpdive.example.org/app/admin/vpdive/%2Ff%2Fuser%2Findex?route=/f/user/index", links["membres"].URL)
}
