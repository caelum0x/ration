package authclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// bearerHeader must match services/gateway/internal/tenantauth's
// authorizationHeader constant (and services/ledger-core/src/
// tenantauth.rs's AUTHORIZATION_HEADER) exactly — all three sides of
// the same credential exchange are defined independently but must
// agree on the wire.
const bearerHeader = "authorization"

// UnaryClientInterceptor attaches "authorization: Bearer <token>" to
// every outgoing unary RPC's context — see MintOperatorJWT's doc
// comment for why gateway needs this on its own outgoing ledger-core
// calls, not just incoming ones.
func UnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, bearerHeader, "Bearer "+token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor is UnaryClientInterceptor's counterpart for
// streaming RPCs.
func StreamClientInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, bearerHeader, "Bearer "+token)
		return streamer(ctx, desc, cc, method, opts...)
	}
}
