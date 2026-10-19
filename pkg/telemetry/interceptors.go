package telemetry

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor returns a grpc.UnaryServerInterceptor that:
//  1. extracts an incoming x-ration-correlation-id (from a caller that
//     already has one, e.g. gateway forwarding a cli-issued ID to
//     ledger-core) or mints a fresh one if this RPC is the root of a new
//     unit of work;
//  2. stores it on the request context (retrievable via
//     CorrelationIDFromContext for use in RecordAgentDecision audit
//     trails and application logs);
//  3. logs one structured JSON line per RPC (method, duration, error,
//     correlation_id, trace_id/span_id) via logger.
//
// Combine with ServerOption (grpc.StatsHandler) for tracing — this
// interceptor handles correlation IDs and logging, the stats handler
// handles the W3C trace context; both run on every RPC.
func UnaryServerInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = ensureCorrelationID(ctx)
		start := time.Now()

		resp, err := handler(ctx, req)

		logRPC(logger, ctx, info.FullMethod, time.Since(start), err)
		return resp, err
	}
}

// StreamServerInterceptor is StreamServerInterceptor's streaming
// counterpart, used for gateway.proto's StreamActivity RPC. The
// correlation ID and per-RPC log line cover the stream's full lifetime
// (open to close), not each message.
func StreamServerInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ensureCorrelationID(ss.Context())
		wrapped := &correlationServerStream{ServerStream: ss, ctx: ctx}
		start := time.Now()

		err := handler(srv, wrapped)

		logRPC(logger, ctx, info.FullMethod, time.Since(start), err)
		return err
	}
}

// correlationServerStream overrides grpc.ServerStream.Context() so
// handler code (and anything it calls) observes the correlation-ID-
// bearing context rather than the raw stream context.
type correlationServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *correlationServerStream) Context() context.Context { return s.ctx }

// UnaryClientInterceptor returns a grpc.UnaryClientInterceptor that
// ensures the outgoing context carries a correlation ID (minting one if
// the caller — typically a CLI command's root context — never set one)
// and propagates it to the callee via gRPC metadata.
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = injectCorrelationID(ctx)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor is UnaryClientInterceptor's streaming
// counterpart (used when dialing gateway's StreamActivity from the TUI).
func StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = injectCorrelationID(ctx)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// ensureCorrelationID reads x-ration-correlation-id out of incoming gRPC
// metadata if present, otherwise mints a new one, and returns a context
// carrying it (for CorrelationIDFromContext).
func ensureCorrelationID(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(CorrelationIDHeader); len(vals) > 0 && vals[0] != "" {
			return ContextWithCorrelationID(ctx, vals[0])
		}
	}
	return ContextWithCorrelationID(ctx, NewCorrelationID())
}

// injectCorrelationID ensures ctx carries a correlation ID (minting one
// if absent) and attaches it to outgoing gRPC metadata so the next hop's
// server interceptor picks it up.
func injectCorrelationID(ctx context.Context) context.Context {
	id := CorrelationIDFromContext(ctx)
	if id == "" {
		id = NewCorrelationID()
		ctx = ContextWithCorrelationID(ctx, id)
	}
	return metadata.AppendToOutgoingContext(ctx, CorrelationIDHeader, id)
}

func logRPC(logger *slog.Logger, ctx context.Context, method string, dur time.Duration, err error) {
	if logger == nil {
		return
	}
	attrs := append(LogAttrs(ctx),
		slog.String("grpc_method", method),
		slog.Duration("duration", dur),
	)
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()), slog.String("grpc_code", status.Code(err).String()))
		logger.LogAttrs(ctx, slog.LevelWarn, "rpc failed", attrs...)
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "rpc completed", attrs...)
}
