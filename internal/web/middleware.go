package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const tracerName = "github.com/SkYNewZ/sos-vpdive/internal/web"

// contentSecurityPolicy is spec §11.5, identical on both domains.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' https://challenges.cloudflare.com; " +
	"frame-src https://challenges.cloudflare.com; " +
	"connect-src 'self'; img-src 'self' data:; style-src 'self'; " +
	"worker-src 'self'; manifest-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'self'; form-action 'self'"

type ctxKey int

const ctxClientIP ctxKey = iota

// recoverPanics answers 500 instead of dropping the connection. It logs the
// panic type only: the value could hold request data.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		defer func() {
			if v := recover(); v != nil {
				s.logger.ErrorContext(ctx, "handler panic", "type", fmt.Sprintf("%T", v))
				http.Error(w, "Erreur interne.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxClientIP, s.clientIP(r))))
	})
}

// clientIP returns the visitor's address. Forwarded headers are read only
// when the peer is a trusted proxy, walking X-Forwarded-For from the right.
func (s *Server) clientIP(r *http.Request) netip.Addr {
	peer := remoteAddr(r)
	if !s.trusted(peer) {
		return peer
	}
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for _, part := range slices.Backward(parts) {
		raw := strings.TrimSpace(part)
		if raw == "" {
			continue
		}
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			return peer
		}
		if ip = ip.Unmap(); !s.trusted(ip) {
			return ip
		}
	}
	return peer
}

func remoteAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

func (s *Server) trusted(ip netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// securityHeaders sets the headers of spec §11.5 and §11.8 on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Strict-Transport-Security", "max-age=63072000")
		h.Set("X-Robots-Tag", "noindex, nofollow, noai, noimageai")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// refuseAIRobots answers 403 to AI robots that announce themselves, except
// on robots.txt.
func (s *Server) refuseAIRobots(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" && s.robots.refuses(r.UserAgent()) {
			s.writeText(w, r, http.StatusForbidden, "Accès refusé aux robots d'IA.\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireOrigin refuses any mutation whose Origin is not exactly the domain's
// origin; a missing Origin is refused too (spec §11.2).
func (s *Server) requireOrigin(site *url.URL, next http.Handler) http.Handler {
	origin := site.Scheme + "://" + site.Host
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("Origin") != origin {
				s.writeText(w, r, http.StatusForbidden, "Requête refusée : origine non valide.\n")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// instrument wraps a route with a server span and an access log line, both
// named after the route pattern, never the path (spec §9.9). Incoming trace
// context is honoured only from trusted proxies.
func (s *Server) instrument(route string, next http.Handler) http.Handler {
	tracer := otel.Tracer(tracerName)
	_, routePath, found := strings.Cut(route, " ")
	if !found {
		routePath = route
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := r.Context()
		if s.trusted(remoteAddr(r)) {
			ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(r.Header))
		}
		ctx, span := tracer.Start(ctx, route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", routePath),
			))
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= http.StatusInternalServerError {
			telemetry.Fail(span, "http_"+strconv.Itoa(rec.status))
		}
		s.logger.InfoContext(ctx, "request", "method", r.Method, "route", route,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter

	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// matchHost compares a request Host with a configured URL, ignoring case and
// the scheme's default port.
func matchHost(host string, site *url.URL) bool {
	return hostKey(host, site.Scheme) == hostKey(site.Host, site.Scheme)
}

func hostKey(host, scheme string) string {
	host = strings.ToLower(host)
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return h
	}
	return host
}
