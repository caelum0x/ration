// Package authclient is a typed gRPC client wrapper around the
// generated pb.AuthClient, used by the CLI's main.go to mint a single
// operator-scoped JWT at startup and attach it to every gateway call —
// closing docs/THREAT-MODEL.md's "Known gaps" item 1's own named
// residual TODO: "wire services/cli and services/tui to call
// auth.IssueAPIKey (or MintJWT) once and send the result as metadata on
// every gateway call."
//
// This mirrors services/gateway/internal/authclient's existing pattern
// (same Dial/NewFromConn/Close shape) but is its own package, not a
// shared import: each service under services/ is its own Go module, and
// gateway's authclient only exposes the two read-only Verify* RPCs this
// package doesn't need — it needs MintJWT instead.
package authclient

import (
	"context"
	"fmt"
	"time"

	authpb "github.com/caelum0x/ration/pkg/pb/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps an AuthClient gRPC stub with a Go-idiomatic method per
// RPC this binary needs.
type Client struct {
	conn    *grpc.ClientConn
	stub    authpb.AuthClient
	Timeout time.Duration
}

// Dial connects to auth at addr. The connection is lazy (gRPC only
// actually dials on first RPC) but addr is still validated as
// non-empty here so misconfiguration fails at startup, not on the
// first request.
func Dial(addr string, extraOpts ...grpc.DialOption) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("authclient: addr must not be empty")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, extraOpts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("authclient: dial %s: %w", addr, err)
	}
	return &Client{
		conn:    conn,
		stub:    authpb.NewAuthClient(conn),
		Timeout: 5 * time.Second,
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// MintOperatorJWT mints a cross-tenant ("operator") JWT for subject, so
// this process can present it as an "authorization: Bearer <token>"
// credential on every tenant-scoped gateway RPC it issues, regardless of
// which tenant_id that particular RPC names (see proto/auth.proto's
// is_operator field and internal/tenantauth's operator-bypass doc
// comment on the gateway side). The CLI legitimately needs this: a
// single invocation often inspects or drives more than one tenant (e.g.
// `ration seed` creates several), and a tenant-scoped credential could
// never cover that.
func (c *Client) MintOperatorJWT(ctx context.Context, subject string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := c.stub.MintJWT(rctx, &authpb.MintJWTRequest{
		Subject:    subject,
		TenantId:   "",
		TtlSeconds: int64(ttl / time.Second),
		IsOperator: true,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authclient: mint operator JWT: %w", err)
	}
	return resp.GetToken(), time.Unix(resp.GetExpiresAtUnix(), 0), nil
}
