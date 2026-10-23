// Package authclient is a typed gRPC client wrapper around the
// generated pb.AuthClient, used by the agent's main.go to mint a single
// operator-scoped JWT at startup and attach it to every gateway call.
//
// docs/THREAT-MODEL.md's own residual TODO for item 1 names only
// services/cli and services/tui, but the agent is a third direct
// gateway caller (RationBridgeFunding, IssueCard, AdjustCardLimit, all
// tenant- or card-scoped) that would otherwise regress the moment
// RATION_GATEWAY_REQUIRE_TENANT_AUTH defaults to true: the agent reacts
// to whichever tenant has a late deposit on a given tick, so — exactly
// like the TUI's all-tenants dashboard — a single tenant-scoped
// credential could never cover it. An operator-scoped credential is the
// correct fit here, not a design shortcut: AGENT-DESIGN.md already
// treats the agent as a trusted platform-level decision loop, never as
// a stand-in for any one tenant.
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
// comment on the gateway side). See this file's package doc comment for
// why the agent specifically needs an operator credential rather than a
// tenant-scoped one.
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
