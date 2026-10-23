// Package authclient is a typed gRPC client wrapper around the
// generated pb.AuthClient, used by internal/tenantauth to verify a
// caller-presented API key or JWT before a tenant-scoped RPC is allowed
// to proceed (docs/THREAT-MODEL.md's "Known gaps" item 1 — "no
// tenant-scoped authorization anywhere, still"). Mostly this wraps the
// two read-only verification RPCs gateway needs at request time;
// IssueAPIKey/RevokeAPIKey/IssueServiceCert remain operator/service-
// provisioning calls issued directly against auth by whoever is
// provisioning a tenant's credentials. The one exception is
// MintOperatorJWT below: gateway is itself a direct caller of
// ledger-core (see cmd/gateway/main.go's SimulateDeposit path and
// internal/grpcserver), so — exactly like services/cli/tui/agent — it
// needs its own operator-scoped credential to present to ledger-core's
// own tenantauth check (services/ledger-core/src/tenantauth.rs,
// docs/THREAT-MODEL.md item 1's "the biggest one" sub-gap).
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
// RPC, so callers in internal/tenantauth don't touch raw pb types or
// context plumbing directly.
type Client struct {
	conn    *grpc.ClientConn
	stub    authpb.AuthClient
	Timeout time.Duration
}

// Dial connects to auth at addr. The connection is lazy (gRPC only
// actually dials on first RPC) but addr is still validated as
// non-empty here so misconfiguration fails at startup, not on the
// first request.
//
// extraOpts are applied after a plaintext-insecure default transport
// credential, so a caller passing its own grpc.WithTransportCredentials
// (e.g. pkg/mtls's Config.DialOption(), for mTLS to auth) simply
// overrides the default rather than needing this function to know
// anything about mTLS itself.
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

// NewFromConn wraps an already-established connection — used by tests
// (bufconn) and anywhere the caller wants to manage the ClientConn
// lifecycle itself.
func NewFromConn(conn *grpc.ClientConn) *Client {
	return &Client{conn: conn, stub: authpb.NewAuthClient(conn), Timeout: 5 * time.Second}
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.Timeout)
}

// VerifyAPIKey checks secret against auth's stored key hashes. The
// five-value return (rather than a struct) matches
// internal/tenantauth.Verifier exactly, so *Client satisfies that
// interface with no adapter needed.
func (c *Client) VerifyAPIKey(ctx context.Context, secret string) (tenantID string, valid bool, isOperator bool, reason string, err error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.VerifyAPIKey(ctx, &authpb.VerifyAPIKeyRequest{Secret: secret})
	if err != nil {
		return "", false, false, "", err
	}
	return resp.GetTenantId(), resp.GetValid(), resp.GetIsOperator(), resp.GetReason(), nil
}

// VerifyJWT checks token's signature and expiry against auth.
func (c *Client) VerifyJWT(ctx context.Context, token string) (tenantID string, valid bool, isOperator bool, reason string, err error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.VerifyJWT(ctx, &authpb.VerifyJWTRequest{Token: token})
	if err != nil {
		return "", false, false, "", err
	}
	return resp.GetTenantId(), resp.GetValid(), resp.GetIsOperator(), resp.GetReason(), nil
}

// MintOperatorJWT mints a cross-tenant ("operator") JWT for subject, so
// gateway can present it as an "authorization: Bearer <token>"
// credential on every tenant-scoped ledger-core RPC it issues directly
// (SimulateDeposit's RecordDeposit call, internal/grpcserver's
// ApplyEntry/GetBalance/etc — all of which, by the time gateway makes
// them, have already been authorized for the right tenant_id by this
// process's own internal/tenantauth; gateway is a trusted forwarder,
// not re-asserting a tenant-scoped identity of its own). See
// services/cli/internal/authclient.MintOperatorJWT's doc comment for
// why operator scope, not tenant scope, is correct here too.
func (c *Client) MintOperatorJWT(ctx context.Context, subject string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.MintJWT(ctx, &authpb.MintJWTRequest{
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
