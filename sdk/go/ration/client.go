// Package ration is the typed Go SDK for Ration's gateway.proto Gateway
// service (docs/SCALE-EXPANSION.md section 7) — the client a platform
// customer, or Ration's own TUI/CLI, imports to talk to a running Ration
// deployment without hand-building protobuf messages. gateway.proto's
// external surface is unchanged by the section-1 service split, so this
// SDK is stable against both the pre-split monolith and the current
// thin-BFF gateway.
package ration

import (
	"context"
	"fmt"
	"io"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Card and ActivityEvent are gateway.proto's generated wire types,
// re-exported here so callers never need to import pkg/pb/gateway
// directly — the generated stubs are this SDK's implementation detail,
// not its public contract. CardStatus is common.proto's shared enum.
type (
	Card          = gatewaypb.Card
	ActivityEvent = gatewaypb.ActivityEvent
	CardStatus    = commonpb.CardStatus
)

// BridgeFundingResult is RequestBridgeFunding's typed result.
type BridgeFundingResult struct {
	GrantedMinorUnits int64
	FullyGranted      bool
	Explanation       string
}

// Client is a typed wrapper over gateway.proto's generated gRPC client.
type Client struct {
	conn *grpc.ClientConn
	gw   gatewaypb.GatewayClient
}

// DialOption configures Dial.
type DialOption func(*dialConfig)

type dialConfig struct {
	grpcOpts []grpc.DialOption
}

// WithDialOptions appends raw grpc.DialOptions — transport credentials for
// mTLS (see pkg/mtls for how every other Ration service dials its peers),
// interceptors, keepalive params, and so on. The last WithTransportCredentials
// option in the chain wins, so passing one here overrides Dial's insecure
// default.
func WithDialOptions(opts ...grpc.DialOption) DialOption {
	return func(c *dialConfig) { c.grpcOpts = append(c.grpcOpts, opts...) }
}

// Dial connects to a Ration gateway at addr (e.g. "localhost:7002", gateway's
// GATEWAY_GRPC_ADDR default). Dials in plaintext by default; pass
// WithDialOptions(grpc.WithTransportCredentials(...)) for mTLS.
func Dial(addr string, opts ...DialOption) (*Client, error) {
	cfg := &dialConfig{grpcOpts: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}}
	for _, opt := range opts {
		opt(cfg)
	}
	conn, err := grpc.NewClient(addr, cfg.grpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("ration: dial %s: %w", addr, err)
	}
	return NewFromConn(conn), nil
}

// NewFromConn wraps an already-established *grpc.ClientConn — for callers
// who need dial options this package doesn't expose, or tests dialing a
// bufconn listener.
func NewFromConn(conn *grpc.ClientConn) *Client {
	return &Client{conn: conn, gw: gatewaypb.NewGatewayClient(conn)}
}

// Close releases the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// IssueCard issues a new card for tenantID with the given spending limit.
// limitInterval is "daily" or "monthly".
func (c *Client) IssueCard(ctx context.Context, tenantID, cardholderName string, limitMinorUnits int64, limitInterval, currency string) (*Card, error) {
	return c.gw.IssueCard(ctx, &gatewaypb.IssueCardRequest{
		TenantId:        tenantID,
		CardholderName:  cardholderName,
		LimitMinorUnits: limitMinorUnits,
		LimitInterval:   limitInterval,
		Currency:        currency,
	})
}

// AdjustCardLimit changes an existing card's spending limit.
func (c *Client) AdjustCardLimit(ctx context.Context, cardID string, newLimitMinorUnits int64, reason string) (*Card, error) {
	return c.gw.AdjustCardLimit(ctx, &gatewaypb.AdjustCardLimitRequest{
		CardId:             cardID,
		NewLimitMinorUnits: newLimitMinorUnits,
		Reason:             reason,
	})
}

// GetCard fetches a single card by id.
func (c *Client) GetCard(ctx context.Context, cardID string) (*Card, error) {
	return c.gw.GetCard(ctx, &gatewaypb.GetCardRequest{CardId: cardID})
}

// ListCards lists every card for tenantID.
func (c *Client) ListCards(ctx context.Context, tenantID string) ([]*Card, error) {
	resp, err := c.gw.ListCards(ctx, &gatewaypb.ListCardsRequest{TenantId: tenantID})
	if err != nil {
		return nil, err
	}
	return resp.GetCards(), nil
}

// RequestBridgeFunding asks the agent's hard-rule policy envelope (see
// docs/AGENT-DESIGN.md) to grant bridge funding for tenantID. The grant may
// be partial or zero — check FullyGranted and Explanation, not just the
// error, to see what happened.
func (c *Client) RequestBridgeFunding(ctx context.Context, tenantID string, requestedMinorUnits int64, reason string) (*BridgeFundingResult, error) {
	resp, err := c.gw.RationBridgeFunding(ctx, &gatewaypb.RationBridgeFundingRequest{
		TenantId:            tenantID,
		RequestedMinorUnits: requestedMinorUnits,
		Reason:              reason,
	})
	if err != nil {
		return nil, err
	}
	return &BridgeFundingResult{
		GrantedMinorUnits: resp.GetGrantedMinorUnits(),
		FullyGranted:      resp.GetFullyGranted(),
		Explanation:       resp.GetExplanation(),
	}, nil
}

// SimulateDeposit is a demo/judging hook: it records an expected deposit
// with ledger-core, optionally backdated (markLate) so it is immediately
// reportable as late. expectedAtUnix of 0 defaults to 24h from now (see
// grpcserver.Server.SimulateDeposit). Returns the new deposit's id.
func (c *Client) SimulateDeposit(ctx context.Context, tenantID string, amountMinorUnits, expectedAtUnix int64, markLate bool) (string, error) {
	resp, err := c.gw.SimulateDeposit(ctx, &gatewaypb.SimulateDepositRequest{
		TenantId:         tenantID,
		AmountMinorUnits: amountMinorUnits,
		ExpectedAtUnix:   expectedAtUnix,
		MarkLate:         markLate,
	})
	if err != nil {
		return "", err
	}
	return resp.GetDepositId(), nil
}

// StreamActivity subscribes to tenantID's activity feed (empty tenantID
// subscribes to every tenant) and returns a channel of events. The events
// channel closes when the stream ends — via ctx cancellation, server
// shutdown, or a non-EOF error, which is reported once on the returned
// error channel immediately before events closes.
func (c *Client) StreamActivity(ctx context.Context, tenantID string) (<-chan *ActivityEvent, <-chan error, error) {
	stream, err := c.gw.StreamActivity(ctx, &gatewaypb.StreamActivityRequest{TenantId: tenantID})
	if err != nil {
		return nil, nil, err
	}

	events := make(chan *ActivityEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		for {
			ev, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					errs <- err
				}
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, errs, nil
}
