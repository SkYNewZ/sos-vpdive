// Package telemetry sets up OpenTelemetry tracing and structured logs that
// carry the current trace ids (spec §9.9).
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
// No global propagator is installed: trace context is never sent to third
// parties, and incoming context is read explicitly from trusted proxies only.
// Sampler and service name follow the standard OTEL_* variables.
func Setup(ctx context.Context, logger *slog.Logger) (func(context.Context) error, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", defaultServiceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		// Batching keeps export off the request path; a slow collector drops spans.
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("trace export failed", "error", err)
	}))
	return tp.Shutdown, nil
}

// Fail marks span as failed with a stable error code. The code is never a
// free-form message, which could carry personal data.
func Fail(span trace.Span, code string) {
	span.SetStatus(codes.Error, code)
	span.SetAttributes(attribute.String("error.code", code))
}
