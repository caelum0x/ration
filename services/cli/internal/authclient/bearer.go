package authclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// bearerHeader must match services/gateway/internal/tenantauth's
// authorizationHeader constant exactly — these two sides of the same
// credential exchange are defined independently (different Go modules)
// but must agree on the wire.
const bearerHeader = "authorization"

// UnaryClientInterceptor attaches "authorization: Bearer <token>" to
// every outgoing unary RPC's context, so a caller only has to mint a
// credential once (see Client.MintOperatorJWT) and dial with this
// interceptor, rather than remembering to attach it at every call site.
func UnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, bearerHeader, "Bearer "+token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor is UnaryClientInterceptor's counterpart for
// streaming RPCs (e.g. StreamActivity).
func StreamClientInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, bearerHeader, "Bearer "+token)
		return streamer(ctx, desc, cc, method, opts...)
	}
}
