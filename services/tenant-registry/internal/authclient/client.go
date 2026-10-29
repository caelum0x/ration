// Package authclient is a typed gRPC client wrapper around the
// generated pb.AuthClient, used by tenant-registry's main.go to mint a
// single operator-scoped JWT at startup and attach it to every
// ledger-core call this process makes directly (CreateTenant/
// GetTenant/ListTenants) — closing docs/THREAT-MODEL.md's "Known gaps"
// item 1's "the biggest one" sub-gap for tenant-registry's half of it:
// it dials ledger-core directly, never through gateway, so without
// this it would be exempt from both gateway's internal/tenantauth AND
// ledger-core's own equivalent check
// (services/ledger-core/src/tenantauth.rs). Those three RPCs are also
// operator-only on ledger-core's own side (no single tenant to check a
// credential against — tenant provisioning and cross-tenant listing by
// nature), so operator scope is the only credential type that works
// here regardless.
//
// Mirrors services/cli/internal/authclient's existing pattern (same
// Dial/Close/MintOperatorJWT shape) but is its own package: each
// service under services/ is its own Go module.
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

// VerifyAPIKey checks secret against auth's stored key hashes. The
// five-value return (rather than a struct) matches
// internal/operatorauth.Verifier exactly, so *Client satisfies that
// interface with no adapter needed — same shape as
// services/gateway/internal/authclient's identical method.
func (c *Client) VerifyAPIKey(ctx context.Context, secret string) (tenantID string, valid bool, isOperator bool, reason string, err error) {
	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := c.stub.VerifyAPIKey(rctx, &authpb.VerifyAPIKeyRequest{Secret: secret})
	if err != nil {
		return "", false, false, "", err
	}
	return resp.GetTenantId(), resp.GetValid(), resp.GetIsOperator(), resp.GetReason(), nil
}

// VerifyJWT checks token's signature and expiry against auth.
func (c *Client) VerifyJWT(ctx context.Context, token string) (tenantID string, valid bool, isOperator bool, reason string, err error) {
	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := c.stub.VerifyJWT(rctx, &authpb.VerifyJWTRequest{Token: token})
	if err != nil {
		return "", false, false, "", err
	}
	return resp.GetTenantId(), resp.GetValid(), resp.GetIsOperator(), resp.GetReason(), nil
}

// MintOperatorJWT mints a cross-tenant ("operator") JWT for subject —
// see the package doc comment for why tenant-registry needs operator
// scope.
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
