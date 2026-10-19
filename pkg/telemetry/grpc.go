package telemetry

import (
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

// ServerOption returns the grpc.ServerOption that wires OpenTelemetry
// trace propagation into every unary and streaming RPC a server
// receives: it extracts an incoming W3C traceparent from gRPC metadata
// (if the caller is itself instrumented, e.g. another Ration service)
// and starts a server span as its child, or starts a fresh root span if
// this is the entry point of a trace (e.g. the first hop from the CLI).
//
// Pass this to grpc.NewServer alongside UnaryServerInterceptor /
// StreamServerInterceptor (below) for structured request logging —
// stats handlers (tracing) and interceptors (logging/correlation IDs)
// compose cleanly and both run on every RPC.
func ServerOption() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler())
}

// DialOption returns the grpc.DialOption that wires OpenTelemetry trace
// propagation into every RPC a client issues: it starts a client span
// and injects its W3C traceparent into outgoing gRPC metadata so the
// receiving service's ServerOption picks it up as the same trace.
//
// Every grpc.NewClient call in this project (gateway's ledger-core
// client, agent's ledger-core/gateway clients, cli's and tui's clients)
// should pass this so a single CLI command's trace spans every hop it
// causes.
func DialOption() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler())
}
