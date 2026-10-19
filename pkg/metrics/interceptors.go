package metrics

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

// UnaryServerInterceptor returns a grpc.UnaryServerInterceptor recording
// request counts and latency histograms for every unary RPC against m.
func (m *Metrics) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		m.observe(info.FullMethod, time.Since(start), err)
		return resp, err
	}
}

// StreamServerInterceptor is the streaming counterpart, used for
// gateway.proto's StreamActivity RPC. The whole stream's lifetime (open
// to close) counts as one observation under its method name.
func (m *Metrics) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		m.observe(info.FullMethod, time.Since(start), err)
		return err
	}
}
