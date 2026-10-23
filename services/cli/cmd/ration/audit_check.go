package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	auditpb "github.com/caelum0x/ration/pkg/pb/audit"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/spf13/cobra"
)

// ledgerAgentDecisionScanLimit bounds ListAgentDecisions's one-shot
// limit parameter (ledger.proto has no pagination for this RPC, unlike
// ListEntries/ListRecords). This is large enough to cover every
// decision any demo/hackathon-scale tenant will ever record in one
// run (see docs/AGENT-DESIGN.md, docs/ARCHITECTURE.md's stated demo-
// scale assumptions elsewhere in this project) — audit-check is a
// live diagnostic for that scale, not a long-term archival reconciler;
// a tenant that has recorded more than this many decisions needs a
// paginated ListAgentDecisions RPC added to ledger.proto first, which
// is a real, separate change, not something this command can work
// around on its own.
const ledgerAgentDecisionScanLimit = 1_000_000

// auditCheckTolerance is how many records of divergence between
// ledger-core's own count and audit-log's mirrored count audit-check
// treats as healthy, not a mismatch to report. The two counts are
// read via two independent, non-atomic RPC round-trips — a deposit or
// card operation landing on ledger-core between the two reads (most
// visibly under a live demo or the agent's own poll loop) can
// legitimately leave audit-log's mirror a record or two behind for a
// moment, since mirroring is explicitly best-effort and asynchronous
// (see services/ledger-core/src/audit_mirror.rs). A real, sustained
// gap — the actual failure mode docs/THREAT-MODEL.md's "Known gaps"
// #3 names ("no operational cross-check... between ledger-core's
// entry count and audit-log's mirrored count") — will be far larger
// than this tolerance.
const auditCheckTolerance = 2

func newAuditCheckCmd(flags *globalFlags) *cobra.Command {
	var tenantID string
	var tolerance int64

	cmd := &cobra.Command{
		Use:   "audit-check",
		Short: "Cross-check ledger-core's own entry/decision counts against audit-log's mirror",
		Long: `audit-check closes docs/THREAT-MODEL.md's "Known gaps" #3: there was no
operational cross-check between ledger-core's entry count and audit-log's
mirrored count, even though mirroring into audit-log is explicitly
best-effort (services/ledger-core/src/audit_mirror.rs) — a silently
failing mirror (network blip, audit-log down, a serialization bug) would
leave audit-log looking intact (VerifyChain only checks what IS there,
never what's MISSING) while quietly diverging from the live ledger.

For each tenant (every tenant, or just --tenant), it counts ledger-core's
own ApplyEntry-produced LedgerEntry records (ListEntries, fully paginated)
and RecordAgentDecision-produced AgentDecision records (ListAgentDecisions),
then counts audit-log's own mirrored records of each kind for that same
tenant (ListRecords, fully paginated, filtered by SourceKind), and reports
any tenant/kind pair whose counts diverge by more than --tolerance.

Exits 0 when every count matches (within tolerance); exits 1 and prints
every divergent tenant/kind pair otherwise — safe to wire into
docs/RUNBOOK.md's on-call checklist or a monitoring cron job.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditCheck(cmd, flags, tenantID, tolerance)
		},
	}

	cmd.Flags().StringVar(&tenantID, "tenant", "",
		"check only this tenant ID (default: every tenant)")
	cmd.Flags().Int64Var(&tolerance, "tolerance", auditCheckTolerance,
		"maximum count divergence treated as healthy, not a mismatch")

	return cmd
}

// countsByKind is one tenant's ledger-core-side and audit-log-side
// record counts for both SourceKinds.
type countsByKind struct {
	ledgerEntries   int64
	auditEntries    int64
	ledgerDecisions int64
	auditDecisions  int64
}

func runAuditCheck(cmd *cobra.Command, flags *globalFlags, tenantID string, tolerance int64) error {
	c, err := dialClients(cmd.Context(), flags.ledgerAddr, flags.gatewayAddr, flags.authAddr, flags.auditAddr)
	if err != nil {
		return err
	}
	defer c.close()

	ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout*4)
	defer cancel()

	out := cmd.OutOrStdout()

	var tenants []*ledgerpb.Tenant
	if tenantID != "" {
		t, err := c.Ledger.GetTenant(ctx, &ledgerpb.GetTenantRequest{Id: tenantID})
		if err != nil {
			return fmt.Errorf("get tenant %s: %w", tenantID, err)
		}
		tenants = []*ledgerpb.Tenant{t}
	} else {
		resp, err := c.Ledger.ListTenants(ctx, &ledgerpb.ListTenantsRequest{PageSize: 1000})
		if err != nil {
			return fmt.Errorf("list tenants: %w", err)
		}
		tenants = resp.GetTenants()
	}

	fmt.Fprintf(out, "audit-check: cross-checking %d tenant(s) against audit-log at %s\n\n", len(tenants), flags.auditAddr)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TENANT\tLEDGER ENTRIES\tAUDIT ENTRIES\tLEDGER DECISIONS\tAUDIT DECISIONS\tSTATUS")

	var mismatches []string
	for _, t := range tenants {
		counts, err := collectCounts(ctx, c, t.GetId())
		if err != nil {
			return fmt.Errorf("collect counts for tenant %s: %w", t.GetId(), err)
		}

		status := "OK"
		entryDiff := absInt64(counts.ledgerEntries - counts.auditEntries)
		decisionDiff := absInt64(counts.ledgerDecisions - counts.auditDecisions)
		if entryDiff > tolerance {
			status = "MISMATCH"
			mismatches = append(mismatches, fmt.Sprintf(
				"tenant %s (%s): ledger entries=%d vs audit entries=%d (diff %d > tolerance %d)",
				t.GetId(), t.GetName(), counts.ledgerEntries, counts.auditEntries, entryDiff, tolerance))
		}
		if decisionDiff > tolerance {
			status = "MISMATCH"
			mismatches = append(mismatches, fmt.Sprintf(
				"tenant %s (%s): ledger decisions=%d vs audit decisions=%d (diff %d > tolerance %d)",
				t.GetId(), t.GetName(), counts.ledgerDecisions, counts.auditDecisions, decisionDiff, tolerance))
		}

		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\n",
			t.GetName(), counts.ledgerEntries, counts.auditEntries, counts.ledgerDecisions, counts.auditDecisions, status)
	}
	_ = w.Flush()
	fmt.Fprintln(out)

	if len(mismatches) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "audit-check: DIVERGENCE FOUND:")
		for _, m := range mismatches {
			fmt.Fprintf(cmd.ErrOrStderr(), "  - %s\n", m)
		}
		return fmt.Errorf("audit-check: %d divergent tenant/kind pair(s) found", len(mismatches))
	}

	fmt.Fprintln(out, "audit-check: OK — every tenant's ledger-core and audit-log counts match within tolerance")
	return nil
}

// collectCounts gathers one tenant's four counts: ledger-core's own
// LedgerEntry/AgentDecision counts (the live source of truth) and
// audit-log's mirrored count of each (the independent, append-only
// copy — see docs/SCALE-EXPANSION.md section 8).
func collectCounts(ctx context.Context, c *clients, tenantID string) (countsByKind, error) {
	var out countsByKind

	ledgerEntries, err := countLedgerEntries(ctx, c, tenantID)
	if err != nil {
		return out, fmt.Errorf("count ledger-core entries: %w", err)
	}
	out.ledgerEntries = ledgerEntries

	decisionsResp, err := c.Ledger.ListAgentDecisions(ctx, &ledgerpb.ListAgentDecisionsRequest{
		TenantId: tenantID,
		Limit:    ledgerAgentDecisionScanLimit,
	})
	if err != nil {
		return out, fmt.Errorf("list agent decisions: %w", err)
	}
	out.ledgerDecisions = int64(len(decisionsResp.GetDecisions()))

	auditEntries, auditDecisions, err := countAuditRecords(ctx, c, tenantID)
	if err != nil {
		return out, fmt.Errorf("count audit-log records: %w", err)
	}
	out.auditEntries = auditEntries
	out.auditDecisions = auditDecisions

	return out, nil
}

// countLedgerEntries fully paginates ListEntries for tenantID and
// returns the total record count — ListEntries DOES support proper
// pagination (unlike ListAgentDecisions), so this is an exact count,
// not a scan-limit approximation.
func countLedgerEntries(ctx context.Context, c *clients, tenantID string) (int64, error) {
	var total int64
	pageToken := ""
	for {
		resp, err := c.Ledger.ListEntries(ctx, &ledgerpb.ListEntriesRequest{
			TenantId:  tenantID,
			PageSize:  500,
			PageToken: pageToken,
		})
		if err != nil {
			return 0, err
		}
		total += int64(len(resp.GetEntries()))
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return total, nil
		}
	}
}

// countAuditRecords fully paginates audit-log's ListRecords for
// tenantID and returns separate counts for LEDGER_ENTRY and
// AGENT_DECISION records.
func countAuditRecords(ctx context.Context, c *clients, tenantID string) (entries, decisions int64, err error) {
	pageToken := ""
	for {
		resp, err := c.Audit.ListRecords(ctx, &auditpb.ListRecordsRequest{
			TenantId:  tenantID,
			PageSize:  500,
			PageToken: pageToken,
		})
		if err != nil {
			return 0, 0, err
		}
		for _, r := range resp.GetRecords() {
			switch r.GetKind() {
			case auditpb.SourceKind_SOURCE_KIND_LEDGER_ENTRY:
				entries++
			case auditpb.SourceKind_SOURCE_KIND_AGENT_DECISION:
				decisions++
			}
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return entries, decisions, nil
		}
	}
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
