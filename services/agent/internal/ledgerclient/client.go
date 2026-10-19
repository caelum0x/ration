// Package ledgerclient is a thin, typed wrapper around the generated
// ledger-core gRPC client. It exists so the rest of the agent (the loop
// runner and the LLM tool handlers) talk to ledger-core through a small,
// purpose-built interface instead of the raw generated client, and so
// RecordAgentDecision — the one call every tool invocation must make for
// audit purposes — has a single, consistent call site.
package ledgerclient

import (
	"context"
	"fmt"
	"time"

	common "github.com/caelum0x/ration/pkg/pb/common"
	ledger "github.com/caelum0x/ration/pkg/pb/ledger"
)

// Client wraps ledger.LedgerCoreClient with the subset of operations the
// agent needs, with Go-native types (time.Time instead of unix int64,
// errors instead of raw gRPC statuses with no context).
type Client struct {
	raw ledger.LedgerCoreClient
}

// New builds a Client around an existing generated gRPC client. Accepting
// the interface (rather than a concrete *grpc.ClientConn) keeps this
// package trivially fakeable in tests.
func New(raw ledger.LedgerCoreClient) *Client {
	return &Client{raw: raw}
}

// ListLateDeposits returns every deposit ledger-core considers late as of
// asOf.
func (c *Client) ListLateDeposits(ctx context.Context, asOf time.Time) ([]*ledger.Deposit, error) {
	resp, err := c.raw.ListLateDeposits(ctx, &ledger.ListLateDepositsRequest{
		AsOfUnix: asOf.Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: ListLateDeposits: %w", err)
	}
	return resp.GetDeposits(), nil
}

// ListDeposits returns every deposit recorded for a tenant.
func (c *Client) ListDeposits(ctx context.Context, tenantID string) ([]*ledger.Deposit, error) {
	resp, err := c.raw.ListDeposits(ctx, &ledger.ListDepositsRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: ListDeposits(%s): %w", tenantID, err)
	}
	return resp.GetDeposits(), nil
}

// GetTenant fetches a tenant by id.
func (c *Client) GetTenant(ctx context.Context, tenantID string) (*ledger.Tenant, error) {
	t, err := c.raw.GetTenant(ctx, &ledger.GetTenantRequest{Id: tenantID})
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: GetTenant(%s): %w", tenantID, err)
	}
	return t, nil
}

// CheckReserveFloor asks ledger-core how much headroom a tenant has above
// its reserve floor for a proposed debit. Passing proposedDebitMinorUnits
// of 0 returns the tenant's current unconditional headroom, which is what
// the bridge-funding envelope math in internal/policy uses.
func (c *Client) CheckReserveFloor(ctx context.Context, tenantID string, proposedDebitMinorUnits int64) (*ledger.CheckReserveFloorResponse, error) {
	resp, err := c.raw.CheckReserveFloor(ctx, &ledger.CheckReserveFloorRequest{
		TenantId:                tenantID,
		ProposedDebitMinorUnits: proposedDebitMinorUnits,
	})
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: CheckReserveFloor(%s): %w", tenantID, err)
	}
	return resp, nil
}

// ListAgentDecisions returns the most recent agent decisions recorded for
// a tenant, most-recent-first (as returned by ledger-core), bounded by
// limit.
func (c *Client) ListAgentDecisions(ctx context.Context, tenantID string, limit int32) ([]*ledger.AgentDecision, error) {
	resp, err := c.raw.ListAgentDecisions(ctx, &ledger.ListAgentDecisionsRequest{
		TenantId: tenantID,
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: ListAgentDecisions(%s): %w", tenantID, err)
	}
	return resp.GetDecisions(), nil
}

// RecordAgentDecision writes an audit record for a single agent decision —
// a hard-rule computation or an LLM tool call — before or immediately
// after any associated money movement. snapshotJSON should capture the
// exact rule inputs (headroom, deposit amount, velocity state) that
// produced the decision; pass "{}" if there is nothing to capture.
func (c *Client) RecordAgentDecision(
	ctx context.Context,
	tenantID string,
	trigger string,
	action common.AgentAction,
	rationale string,
	snapshotJSON string,
) (*ledger.AgentDecision, error) {
	decision, err := c.raw.RecordAgentDecision(ctx, &ledger.RecordAgentDecisionRequest{
		TenantId:           tenantID,
		Trigger:            trigger,
		Action:             action,
		Rationale:          rationale,
		PolicySnapshotJson: snapshotJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("ledgerclient: RecordAgentDecision(%s, %s): %w", tenantID, action, err)
	}
	return decision, nil
}
