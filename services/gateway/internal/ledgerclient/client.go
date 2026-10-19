// Package ledgerclient is a typed gRPC client wrapper around the
// generated pb.LedgerCoreClient. Every balance-affecting call the
// gateway makes goes through this package — the gateway never mutates
// money state directly, ledger-core (Rust, backed by the C WAL) is the
// one source of truth.
package ledgerclient

import (
	"context"
	"fmt"
	"time"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a LedgerCoreClient gRPC stub with a Go-idiomatic method
// per RPC, so callers in internal/grpcserver don't touch raw pb types or
// context plumbing directly. The gateway's only remaining direct
// ledger-core dependency, post section-1 split, is SimulateDeposit (see
// internal/grpcserver's doc comment) — everything card/bridge-related
// now goes through internal/cardissuanceclient to services/card-issuance
// instead.
type Client struct {
	conn    *grpc.ClientConn
	stub    ledgerpb.LedgerCoreClient
	Timeout time.Duration
}

// Dial connects to ledger-core at addr. The connection is lazy (gRPC only
// actually dials on first RPC) but we still validate addr is non-empty
// here so misconfiguration fails at startup, not on the first request.
//
// extraOpts are applied after a plaintext-insecure default transport
// credential, so a caller passing its own grpc.WithTransportCredentials
// (e.g. pkg/mtls's Config.DialOption(), for mTLS to ledger-core) simply
// overrides the default rather than needing this function to know
// anything about mTLS itself. Any other DialOption (interceptors,
// telemetry, …) is applied as given.
func Dial(addr string, extraOpts ...grpc.DialOption) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("ledgerclient: addr must not be empty")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, extraOpts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: dial %s: %w", addr, err)
	}
	return &Client{
		conn:    conn,
		stub:    ledgerpb.NewLedgerCoreClient(conn),
		Timeout: 10 * time.Second,
	}, nil
}

// NewFromConn wraps an already-established connection — used by tests
// (bufconn) and anywhere the caller wants to manage the ClientConn
// lifecycle itself.
func NewFromConn(conn *grpc.ClientConn) *Client {
	return &Client{conn: conn, stub: ledgerpb.NewLedgerCoreClient(conn), Timeout: 10 * time.Second}
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

func (c *Client) CreateTenant(ctx context.Context, req *ledgerpb.CreateTenantRequest) (*ledgerpb.Tenant, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.CreateTenant(ctx, req)
}

func (c *Client) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.GetTenant(ctx, &ledgerpb.GetTenantRequest{Id: tenantID})
}

func (c *Client) ListTenants(ctx context.Context, req *ledgerpb.ListTenantsRequest) (*ledgerpb.ListTenantsResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.ListTenants(ctx, req)
}

func (c *Client) RecordDeposit(ctx context.Context, req *ledgerpb.RecordDepositRequest) (*ledgerpb.Deposit, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.RecordDeposit(ctx, req)
}

func (c *Client) MarkDepositReceived(ctx context.Context, req *ledgerpb.MarkDepositReceivedRequest) (*ledgerpb.Deposit, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.MarkDepositReceived(ctx, req)
}

func (c *Client) ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.ListDeposits(ctx, &ledgerpb.ListDepositsRequest{TenantId: tenantID})
	if err != nil {
		return nil, err
	}
	return resp.GetDeposits(), nil
}

func (c *Client) ListLateDeposits(ctx context.Context, asOfUnix int64) ([]*ledgerpb.Deposit, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.ListLateDeposits(ctx, &ledgerpb.ListLateDepositsRequest{AsOfUnix: asOfUnix})
	if err != nil {
		return nil, err
	}
	return resp.GetDeposits(), nil
}

func (c *Client) ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.ApplyEntry(ctx, req)
}

func (c *Client) GetBalance(ctx context.Context, tenantID string) (*ledgerpb.Balance, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.GetBalance(ctx, &ledgerpb.GetBalanceRequest{TenantId: tenantID})
}

func (c *Client) ListEntries(ctx context.Context, tenantID string) ([]*ledgerpb.LedgerEntry, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	var all []*ledgerpb.LedgerEntry
	pageToken := ""
	for {
		resp, err := c.stub.ListEntries(ctx, &ledgerpb.ListEntriesRequest{
			TenantId:  tenantID,
			PageSize:  200,
			PageToken: pageToken,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, resp.GetEntries()...)
		if resp.GetNextPageToken() == "" {
			break
		}
		pageToken = resp.GetNextPageToken()
	}
	return all, nil
}

func (c *Client) CheckReserveFloor(ctx context.Context, tenantID string, proposedDebitMinorUnits int64) (*ledgerpb.CheckReserveFloorResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.CheckReserveFloor(ctx, &ledgerpb.CheckReserveFloorRequest{
		TenantId:                tenantID,
		ProposedDebitMinorUnits: proposedDebitMinorUnits,
	})
}

func (c *Client) RecordAgentDecision(ctx context.Context, req *ledgerpb.RecordAgentDecisionRequest) (*ledgerpb.AgentDecision, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.stub.RecordAgentDecision(ctx, req)
}

func (c *Client) ListAgentDecisions(ctx context.Context, tenantID string, limit int32) ([]*ledgerpb.AgentDecision, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.stub.ListAgentDecisions(ctx, &ledgerpb.ListAgentDecisionsRequest{TenantId: tenantID, Limit: limit})
	if err != nil {
		return nil, err
	}
	return resp.GetDecisions(), nil
}
