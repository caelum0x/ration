// Package telemetry wires OpenTelemetry tracing into every Go service
// (gateway, agent, tui, cli), so one CLI command can be followed as a
// single trace across every hop it causes: cli/tui -> gateway ->
// ledger-core, and agent -> {gateway, ledger-core}.
//
// Design:
//   - Setup installs a batching OTLP/gRPC trace exporter talking to the
//     local otel-collector (infra/otel-collector) and registers the W3C
//     tracecontext + baggage propagators globally, so every span created
//     anywhere in the process (via otel.Tracer) and every gRPC call made
//     with the stats handlers below automatically joins the same trace.
//   - ServerOption/DialOption wrap the battle-tested
//     go.opentelemetry.io/contrib otelgrpc stats handlers rather than
//     hand-rolling span propagation: they already handle request/response
//     size, status-code mapping and W3C traceparent propagation over gRPC
//     metadata correctly, which is exactly "gRPC client/server
//     interceptors propagating trace context across every service hop"
//     from docs/SCALE-EXPANSION.md section 2.
//   - WithSpan is the one helper application code should reach for when a
//     unit of work (a policy decision, an LLM call, a ledger write)
//     deserves its own child span without every call site hand-rolling
//     tracer.Start/End/RecordError bookkeeping.
//
// If OTEL_EXPORTER_OTLP_ENDPOINT (or RATION_OTEL_EXPORTER_ENDPOINT) is
// unset, Setup still installs the SDK against the default
// "localhost:4317" collector address. The OTLP/gRPC exporter is
// non-blocking: if no collector is listening, span export calls fail
// silently in the background and the service's actual request path is
// completely unaffected (this is deliberate — observability must never
// become a new way for the ledger to go down).
package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// defaultOTLPEndpoint matches infra/otel-collector's gRPC OTLP receiver
// port (4317) in the local docker-compose observability stack.
const defaultOTLPEndpoint = "localhost:4317"

// ShutdownFunc flushes and tears down the tracer provider. Callers must
// invoke it (typically via defer) before process exit so the final batch
// of spans is exported rather than dropped.
type ShutdownFunc func(context.Context) error

// Setup installs a global TracerProvider for serviceName, exporting spans
// over OTLP/gRPC, and installs the W3C tracecontext+baggage propagators
// globally so otelgrpc's stats handlers (see ServerOption/DialOption) and
// manual otel.GetTextMapPropagator() callers all agree on the wire
// format. serviceVersion is recorded as a resource attribute for
// filtering in Grafana/Jaeger by deploy.
func Setup(ctx context.Context, serviceName, serviceVersion string) (ShutdownFunc, error) {
	endpoint := endpointFromEnv()

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
		// Keep the connection attempt itself bounded: a collector that's
		// down at boot must never delay service startup.
		otlptracegrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create OTLP trace exporter for %s: %w", endpoint, err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
			attribute.String("ration.deployment.component", serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(shutdownCtx context.Context) error {
		if err := tp.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("telemetry: shutdown tracer provider: %w", err)
		}
		return nil
	}
	return shutdown, nil
}

// endpointFromEnv resolves the OTLP collector endpoint, preferring the
// OpenTelemetry-standard env var and falling back to a project-specific
// one, then the local docker-compose default.
func endpointFromEnv() string {
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		return v
	}
	if v := os.Getenv("RATION_OTEL_EXPORTER_ENDPOINT"); v != "" {
		return v
	}
	return defaultOTLPEndpoint
}

// Tracer returns a named tracer scoped to name (typically the calling
// package's import path), for callers that want to start spans directly
// rather than going through WithSpan.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// WithSpan starts a child span named spanName on tracerName's tracer,
// runs fn with the span-bearing context, and records fn's error (if any)
// on the span before ending it. It returns fn's error unchanged so
// callers can keep their existing error handling.
//
// Every agent decision and ledger-affecting RPC handler in this project
// should wrap its core logic in WithSpan so a trace exported to Grafana
// Tempo/Jaeger shows exactly which policy/ledger step a request spent
// time in, per docs/SCALE-EXPANSION.md section 2's "every agent decision
// and ledger entry must be traceable end to end" requirement.
func WithSpan(ctx context.Context, tracerName, spanName string, fn func(context.Context) error) error {
	ctx, span := Tracer(tracerName).Start(ctx, spanName)
	defer span.End()

	if err := fn(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}
