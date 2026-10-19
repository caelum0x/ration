package telemetry

import (
	"context"

	"github.com/google/uuid"
)

// CorrelationIDHeader is the gRPC metadata key a correlation ID travels
// under between Ration services. Unlike a trace ID (which identifies a
// span tree inside the tracing backend), the correlation ID is a plain
// string every service's structured logs also carry, so "find every log
// line for this one CLI invocation" works with a simple grep even
// without a tracing backend running.
const CorrelationIDHeader = "x-ration-correlation-id"

type correlationIDKey struct{}

// NewCorrelationID generates a fresh correlation ID. Called once per
// externally-triggered unit of work: a CLI command invocation, an
// incoming webhook, or one agent loop tick — everything that unit of
// work causes downstream reuses the same ID.
func NewCorrelationID() string {
	return uuid.NewString()
}

// ContextWithCorrelationID returns a copy of ctx carrying id, retrievable
// later via CorrelationIDFromContext.
func ContextWithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, id)
}

// CorrelationIDFromContext returns the correlation ID stored in ctx, or
// "" if none was ever set (e.g. a context never threaded through the
// client/server interceptors below).
func CorrelationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey{}).(string)
	return id
}
