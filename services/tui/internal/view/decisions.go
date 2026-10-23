package view

import (
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/format"
)

var decisionHeaders = []string{"WHEN", "TENANT", "TRIGGER", "ACTION", "RATIONALE", "POLICY SNAPSHOT"}
var decisionWidths = []int{19, 16, 16, 14, 46, 28}

// Decisions renders the "Agent Decisions" tab: every recorded
// AgentDecision across all tenants (newest first — see
// model.sortDecisionsNewestFirst), the dashboard surface
// docs/THREAT-MODEL.md names as missing entirely before this existed:
// "The TUI does not currently display rationale/agent decisions at
// all — only tenants, deposits, cards, and the live activity stream."
//
// Trigger/Rationale/PolicySnapshotJson are LLM-authored (or, for
// Trigger, at minimum influenceable by an adversarial deposit/webhook
// payload) free text — every one of them goes through
// format.SanitizedCell, which strips terminal-control-character
// injection risk BEFORE truncating, closing the same
// terminal-escape-sequence gap the CLI's `status` command's own
// sanitizeControlChars already closes for this exact data (see
// pkg/textsanitize, which both now share).
func Decisions(decisions []*ledgerpb.AgentDecision, tenantNames map[string]string, cursor int) string {
	rows := make([][]string, 0, len(decisions))
	for _, d := range decisions {
		action := format.AgentActionLabel(d.GetAction())
		rows = append(rows, []string{
			format.Unix(d.GetCreatedAtUnix()),
			tenantNameOrID(tenantNames, d.GetTenantId()),
			format.SanitizedCell(d.GetTrigger(), decisionWidths[2]),
			action,
			format.SanitizedCell(d.GetRationale(), decisionWidths[4]),
			format.SanitizedCell(d.GetPolicySnapshotJson(), decisionWidths[5]),
		})
	}
	return Table(decisionHeaders, rows, decisionWidths, cursor, 3, agentActionStyle)
}

// tenantNameOrID resolves a tenant_id to its display name, falling
// back to the raw id when it isn't known yet — the same fallback
// model.resolveTenantName applies for the activity feed, duplicated
// here (rather than imported) since internal/view must stay
// independent of internal/model (no import cycle: model already
// imports view).
func tenantNameOrID(names map[string]string, tenantID string) string {
	if name, ok := names[tenantID]; ok && name != "" {
		return name
	}
	return tenantID
}
