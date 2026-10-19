package model

import (
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/view"
)

// Tab identifies one of the four dashboard panes.
type Tab int

const (
	TabTenants Tab = iota
	TabDeposits
	TabCards
	TabActivity
)

// tabCount must match the number of Tab constants above.
const tabCount = 4

var tabNames = [tabCount]string{"Tenants", "Deposits", "Cards", "Activity"}

// String renders the tab's display name, used by the tab bar.
func (t Tab) String() string {
	if t < 0 || int(t) >= tabCount {
		return "?"
	}
	return tabNames[t]
}

// TabLabels returns the ordered display names for every tab, for the tab
// bar to render.
func TabLabels() []string {
	out := make([]string, tabCount)
	copy(out, tabNames[:])
	return out
}

// NextTab returns the tab after t, wrapping from the last tab back to the
// first. Pure state transition — no model/terminal dependency — so it's
// unit tested directly.
func NextTab(t Tab) Tab {
	return Tab((int(t) + 1) % tabCount)
}

// PrevTab returns the tab before t, wrapping from the first tab to the
// last.
func PrevTab(t Tab) Tab {
	return Tab((int(t) - 1 + tabCount) % tabCount)
}

// clampCursor keeps a row cursor inside [0, length-1], or 0 when length is
// 0. Pure and independent of Bubble Tea, so it's unit tested directly
// rather than only exercised indirectly through key handling.
func clampCursor(cursor, length int) int {
	if length <= 0 {
		return 0
	}
	if cursor < 0 {
		return 0
	}
	if cursor >= length {
		return length - 1
	}
	return cursor
}

// maxActivityRows bounds the in-memory activity feed so a long-running demo
// session can't grow it unboundedly.
const maxActivityRows = 500

// prependActivity adds ev to the front of rows (newest-first) and trims the
// tail so the result never exceeds max entries. Pure — no mutation of the
// input slice's backing array beyond what append naturally does on a fresh
// slice, no I/O — so it's unit tested directly.
func prependActivity(rows []view.ActivityRow, ev view.ActivityRow, max int) []view.ActivityRow {
	out := make([]view.ActivityRow, 0, min(len(rows)+1, max+1))
	out = append(out, ev)
	out = append(out, rows...)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// buildTenantNames indexes tenants by id -> display name, used to resolve
// tenant_id fields on deposits/cards/activity events into readable names
// without a lookup RPC per row. Pure function over already-fetched data.
func buildTenantNames(tenants []*ledgerpb.Tenant) map[string]string {
	names := make(map[string]string, len(tenants))
	for _, t := range tenants {
		names[t.GetId()] = t.GetName()
	}
	return names
}

// resolveTenantName looks up a tenant's display name, falling back to the
// raw id when it isn't known yet (e.g. an activity event arrives before the
// next tenant refresh completes).
func resolveTenantName(names map[string]string, tenantID string) string {
	if name, ok := names[tenantID]; ok && name != "" {
		return name
	}
	return tenantID
}
