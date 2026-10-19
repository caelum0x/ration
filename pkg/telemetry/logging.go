package telemetry

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger returns a structured JSON slog.Logger (log/slog per
// docs/SCALE-EXPANSION.md section 2) tagged with serviceName, writing to
// stdout. Every service's main.go should construct its base logger with
// this (replacing a bare slog.NewJSONHandler call) so every log line
// everywhere carries the same "service" field to filter by in whatever
// aggregates stdout (docker-compose logs, CloudWatch, etc).
func NewLogger(serviceName string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
	})
	return slog.New(handler).With(slog.String("service", serviceName))
}

// LogAttrs returns the correlation_id, trace_id and span_id attributes
// carried by ctx, suitable for splatting into any slog call:
//
//	logger.LogAttrs(ctx, slog.LevelInfo, "handled request", telemetry.LogAttrs(ctx)...)
//
// trace_id/span_id are read from the active OpenTelemetry span in ctx
// (set by the otelgrpc stats handlers from grpc.go, or by WithSpan), so
// a log line can always be pivoted into the matching trace in
// Grafana/Tempo/Jaeger even without an explicit correlation ID.
func LogAttrs(ctx context.Context) []slog.Attr {
	attrs := make([]slog.Attr, 0, 3)
	if id := CorrelationIDFromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("correlation_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return attrs
}
