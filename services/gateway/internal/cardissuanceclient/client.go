// Package cardissuanceclient is a typed gRPC client wrapper around the
// generated pb.CardIssuanceClient. The gateway is a thin BFF
// (docs/SCALE-EXPANSION.md section 1, docs/adr/0004): every card and
// rationing-math operation it exposes on gateway.proto is a direct
// fan-out to services/card-issuance over this client — the gateway
// itself holds no card or bridge-funding logic anymore, card-issuance
// does.
package cardissuanceclient

import (
	"context"
	"fmt"
	"time"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a CardIssuanceClient gRPC stub with a Go-idiomatic method
// per RPC, so callers in internal/grpcserver don't touch raw pb types or
// context plumbing directly.
type Client struct {
	conn    *grpc.ClientConn
	stub    cardissuancepb.CardIssuanceClient
	Timeout time.Duration
}

// Dial connects to card-issuance at addr. The connection is lazy (gRPC
// only actually dials on first RPC) but we still validate addr is
// non-empty here so misconfiguration fails at startup, not on the first
// request.
//
// extraOpts are applied after a plaintext-insecure default transport
// credential, so a caller passing its own grpc.WithTransportCredentials
// (e.g. pkg/mtls's Config.DialOption(), for mTLS to card-issuance)
// simply overrides the default rather than needing this function to
// know anything about mTLS itself. Any other DialOption (interceptors,
// telemetry, …) is applied as given.
func Dial(addr string, extraOpts ...grpc.DialOption) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("cardissuanceclient: addr must not be empty")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, extraOpts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("cardissuanceclient: dial %s: %w", addr, err)
	}
	return &Client{
		conn:    conn,
		stub:    cardissuancepb.NewCardIssuanceClient(conn),
		Timeout: 10 * time.Second,
	}, nil
}

// NewFromConn wraps an already-established connection — used by tests
// (bufconn) and anywhere the caller wants to manage the ClientConn
// lifecycle itself.
func NewFromConn(conn *grpc.ClientConn) *Client {
	return &Client{conn: conn, stub: cardissuancepb.NewCardIssuanceClient(conn), Timeout: 10 * time.Second}
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

func (c *Client) IssueCard(ctx context.Context, req *cardissuancepb.IssueCardRequest) (*cardissuancepb.Card, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.IssueCard(ctx, req)
}

func (c *Client) AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.AdjustCardLimit(ctx, req)
}

func (c *Client) GetCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.GetCard(ctx, &cardissuancepb.GetCardRequest{CardId: cardID})
}

func (c *Client) CloseCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.CloseCard(ctx, &cardissuancepb.CloseCardRequest{CardId: cardID})
}

func (c *Client) ListCards(ctx context.Context, tenantID string) ([]*cardissuancepb.Card, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.ListCards(ctx, &cardissuancepb.ListCardsRequest{TenantId: tenantID})
	if err != nil {
		return nil, err
	}
	return resp.GetCards(), nil
}

func (c *Client) RationBridgeFunding(ctx context.Context, req *cardissuancepb.RationBridgeFundingRequest) (*cardissuancepb.RationBridgeFundingResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.RationBridgeFunding(ctx, req)
}
