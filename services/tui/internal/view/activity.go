package view

import (
	"fmt"
	"strings"

	"github.com/caelum0x/ration/pkg/textsanitize"
	"github.com/caelum0x/ration/services/tui/internal/format"
)

// ActivityRow is one line in the live activity feed, already resolved to a
// display-ready tenant name (the model does the tenant_id -> name lookup
// once per event, not per render).
type ActivityRow struct {
	ID         string
	TenantName string
	Kind       string
	Summary    string
	AtUnix     int64
}

// Activity renders the activity feed tab: a scrolling, newest-first list
// fed live by the gateway's StreamActivity RPC. rows is expected
// newest-first (the model prepends on arrival); height is the number of
// visible lines available for the list.
func Activity(rows []ActivityRow, cursor, height int) string {
	if len(rows) == 0 {
		return EmptyStyle.Render("(waiting for activity…)")
	}

	start, end := visibleWindow(len(rows), cursor, height)

	var b strings.Builder
	for i := start; i < end; i++ {
		r := rows[i]
		// Summary is built server-side (services/gateway/internal/
		// grpcserver/server.go's publish calls) by interpolating
		// free-text, operator/LLM-influenceable fields verbatim — e.g.
		// IssueCard's CardholderName and AdjustCardLimit's Reason —
		// into the activity feed's Summary string with no
		// sanitization anywhere upstream, unlike every other surface
		// that renders CardholderName (round 13/16's own
		// textsanitize/sanitizeControlChars sweep never reached this
		// RPC's own Summary field, a separate code path). Strip here,
		// at the render boundary, the same place every other
		// terminal-facing field in this package already does it
		// (format.SanitizedCell's own doc comment), so no ANSI/VT100
		// escape sequence embedded in a cardholder name or an
		// adjust-limit reason can survive into this tab.
		summary := textsanitize.StripControlChars(r.Summary)
		// r.TenantName is resolveTenantName's lookup of the Tenant's own
		// operator-supplied Name (round-trips raw if the tenant_id isn't
		// yet known locally) — the same unsanitized-free-text-into-
		// terminal class as Summary just above, on a sibling field this
		// line renders raw via format.Truncate alone (which only cuts
		// length, never strips control bytes, per that function's own
		// doc comment). Strip it the same way.
		tenantName := textsanitize.StripControlChars(r.TenantName)
		line := fmt.Sprintf("%s  %-8s  %-16s  %s",
			format.Unix(r.AtUnix),
			strings.ToUpper(r.Kind),
			format.Truncate(tenantName, 16),
			summary,
		)
		kindLabel := strings.ToUpper(r.Kind)
		styled := activityKindStyle(r.Kind).Render(kindLabel)
		line = strings.Replace(line, kindLabel, styled, 1)

		if i == cursor {
			line = SelectedRowStyle.Render(format.Truncate(
				fmt.Sprintf("%s  %-8s  %-16s  %s", format.Unix(r.AtUnix), kindLabel, format.Truncate(r.TenantName, 16), summary),
				200,
			))
		}

		b.WriteString(line)
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// visibleWindow computes the [start, end) slice of a total-length list to
// display given the available height, keeping cursor inside the window.
// Pure and independent of any terminal/render state, so it's unit tested
// directly.
func visibleWindow(total, cursor, height int) (int, int) {
	if height <= 0 || total == 0 {
		return 0, 0
	}
	if total <= height {
		return 0, total
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= total {
		cursor = total - 1
	}

	start := cursor - height/2
	if start < 0 {
		start = 0
	}
	end := start + height
	if end > total {
		end = total
		start = end - height
	}
	return start, end
}
