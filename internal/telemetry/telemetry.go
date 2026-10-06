// Package telemetry sets up OpenTelemetry tracing, structured logs that
// carry the current trace ids (spec §9.9), and the optional Sentry reporting
// of errors, logs and spans (spec §9.10).
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const defaultServiceName = "support-vpdive-cpp"

// Setup installs the global tracer provider. Spans are always recorded so that
// log lines carry trace ids, but they leave the process only when
// OTEL_EXPORTER_OTLP_ENDPOINT (or its traces variant) is set: without it the
// SDK would post to localhost:4318, so no exporter is created at all.
//
// With s.DSN set, spans also go to Sentry over OTLP, and the returned logger
// feeds Sentry too (WithSentry). config.Load has already turned an invalid
// DSN into a warning and an empty one.
//
// No global propagator is installed: trace context is never sent to third
// parties, and incoming context is read explicitly from trusted proxies only.
// Sampler and service name follow the standard OTEL_* variables.
func Setup(ctx context.Context, logger *slog.Logger, s SentryOptions) (*slog.Logger, func(context.Context) error, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", defaultServiceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("otlp exporter: %w", err)
		}
		// Batching keeps export off the request path; a slow collector drops spans.
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	local := logger // export failures are not sent to Sentry, which may be the one failing
	var client *sentry.Client
	if s.DSN != "" {
		if client, err = NewSentry(s); err != nil {
			return nil, nil, fmt.Errorf("sentry: %w", err)
		}
		exporter, err := sentryExporter(ctx, s.DSN)
		if err != nil {
			return nil, nil, fmt.Errorf("sentry exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
		logger = WithSentry(ctx, logger, client)
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		local.Warn("trace export failed", "error", err)
	}))
	shutdown := func(ctx context.Context) error {
		err := tp.Shutdown(ctx)
		if client != nil {
			client.Flush(sentryFlushTimeout)
		}
		return err
	}
	return logger, shutdown, nil
}

// sentryFlushTimeout bounds the wait for unsent Sentry events at shutdown.
const sentryFlushTimeout = 2 * time.Second

// sentryExporter sends spans to the OTLP endpoint of the DSN's project, as
// sentry-go's otel/otlp module does, but with WithEndpointURL: the DSN alone
// sets the scheme, so OTEL_EXPORTER_OTLP_* variables meant for another
// collector (an http endpoint, OTEL_EXPORTER_OTLP_INSECURE) never downgrade
// the export that carries Sentry's key.
func sentryExporter(ctx context.Context, rawDSN string) (*otlptrace.Exporter, error) {
	dsn, err := sentry.NewDsn(rawDSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	endpoint := dsn.GetAPIURL()
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/envelope/") + "/integration/otlp/v1/traces/"
	auth := "Sentry sentry_version=7, sentry_client=sentry.go/" + sentry.SDKVersion + ", sentry_key=" + dsn.GetPublicKey()
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint.String()),
		otlptracehttp.WithHeaders(map[string]string{"X-Sentry-Auth": auth}))
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	return exporter, nil
}

// Fail marks span as failed with a stable error code. The code is never a
// free-form message, which could carry personal data.
func Fail(span trace.Span, code string) {
	span.SetStatus(codes.Error, code)
	span.SetAttributes(attribute.String("error.code", code))
}

// Trace runs fn in a span named name; a failure records the stable code
// name+".failed".
func Trace(ctx context.Context, tracer trace.Tracer, name string, fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, name)
	defer span.End()
	err := fn(ctx)
	if err != nil {
		Fail(span, name+".failed")
	}
	return err
}
