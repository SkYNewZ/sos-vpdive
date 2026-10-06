package telemetry

import (
	"context"
	"log/slog"
	"slices"

	"github.com/getsentry/sentry-go"
	sentryotel "github.com/getsentry/sentry-go/otel"
	sentryslog "github.com/getsentry/sentry-go/slog"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// SentryOptions turns Sentry on when DSN is set (spec §9.10).
type SentryOptions struct {
	DSN         string
	Environment string
	Release     string
	Transport   sentry.Transport // nil: Sentry's own; tests keep events in memory
}

// NewSentry returns a Sentry client. Its events carry the trace of the
// context they are captured with, and never a request or a user. Logs need
// no switch since sentry-go 0.47: WithSentry's slog handler sends them.
func NewSentry(o SentryOptions) (*sentry.Client, error) {
	return sentry.NewClient(sentry.ClientOptions{
		Dsn:              o.DSN,
		Environment:      o.Environment,
		Release:          o.Release,
		Transport:        o.Transport,
		AttachStacktrace: true,
		EnableTracing:    true,
		TracesSampleRate: 1.0,
		Integrations: func(integrations []sentry.Integration) []sentry.Integration {
			return append(integrations, sentryotel.NewOtelIntegration())
		},
		BeforeSend: scrub,
	})
}

// scrub enforces spec §9.10 on every event: no request (body, cookies,
// headers including Referer, IP address, real URL) and no user. The route
// template is the event's transaction.
func scrub(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	event.Request = nil
	event.User = sentry.User{}
	return event
}

// WithSentry returns a logger that also feeds client. Every record becomes a
// Sentry log, and each record at Error or above also an error event, on the
// trace of its context (spec §9.10). Log levels thus decide what reaches
// Sentry as an error: expected failures log at Warn or below. ctx serves
// records logged without a context.
func WithSentry(ctx context.Context, logger *slog.Logger, client *sentry.Client) *slog.Logger {
	hub := sentry.NewHub(client, sentry.NewScope())
	base := logger.Handler()
	return slog.New(slog.NewMultiHandler(base, &sentryHandler{
		hub:   hub,
		level: base,
		logs:  sentryslog.Option{}.NewSentryHandler(sentry.SetHubOnContext(ctx, hub)),
	}))
}

type sentryHandler struct {
	hub   *sentry.Hub
	level slog.Handler // the stdout handler: Sentry follows LOG_LEVEL
	logs  slog.Handler
	attrs []slog.Attr // from WithAttrs, keys prefixed by their groups
	group string      // "" or "a.b."
}

func (h *sentryHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.level.Enabled(ctx, level)
}

func (h *sentryHandler) Handle(ctx context.Context, r slog.Record) error {
	// With the hub on the context, sentryslog keeps the request's span
	// instead of falling back to a background context.
	ctx = sentry.SetHubOnContext(ctx, h.hub)
	if r.Level >= slog.LevelError {
		h.capture(ctx, r)
	}
	return h.logs.Handle(ctx, r)
}

func (h *sentryHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.logs = h.logs.WithAttrs(attrs)
	c.attrs = slices.Clone(h.attrs)
	for _, a := range attrs {
		c.attrs = append(c.attrs, slog.Attr{Key: h.group + a.Key, Value: a.Value})
	}
	return &c
}

func (h *sentryHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.logs = h.logs.WithGroup(name)
	c.group += name + "."
	return &c
}

// capture sends r as an error event. The message is the record's, stable
// and free of personal data; an error attribute becomes the exception and
// the other attributes a "log" context. The stack is the caller's: inside a
// recovered panic, the panic site.
func (h *sentryHandler) capture(ctx context.Context, r slog.Record) {
	client := h.hub.Client()
	var cause error
	fields := sentry.Context{}
	add := func(key string, v slog.Value) {
		v = v.Resolve()
		if err, ok := v.Any().(error); ok && cause == nil {
			cause = err
			return
		}
		fields[key] = v.String()
	}
	for _, a := range h.attrs {
		add(a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(h.group+a.Key, a.Value)
		return true
	})

	var event *sentry.Event
	if cause != nil {
		event = client.EventFromException(cause, sentry.LevelError)
		event.Message = r.Message
	} else {
		event = client.EventFromMessage(r.Message, sentry.LevelError)
	}
	if len(fields) > 0 {
		event.Contexts["log"] = fields
	}
	if span, ok := trace.SpanFromContext(ctx).(sdktrace.ReadOnlySpan); ok {
		event.Transaction = span.Name()
	}
	client.CaptureEvent(event, &sentry.EventHint{Context: ctx}, h.hub.Scope())
}
