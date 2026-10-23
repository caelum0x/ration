// Package format holds pure, side-effect-free formatting and small
// state-transition helpers for the TUI. Kept separate from the Bubble Tea
// model/view code specifically so it can be unit tested without a terminal.
package format

import (
	"fmt"
	"strings"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	"github.com/caelum0x/ration/pkg/textsanitize"
)

// Money renders fixed-point minor units + an ISO 4217 currency code as a
// human-readable amount, e.g. Money(123456, "USD") -> "$1,234.56".
// Unknown currencies fall back to "<CODE> <amount>".
func Money(minorUnits int64, currency string) string {
	neg := minorUnits < 0
	abs := minorUnits
	if neg {
		abs = -abs
	}

	major := abs / 100
	minor := abs % 100
	grouped := groupThousands(major)

	symbol, ok := currencySymbols[strings.ToUpper(currency)]
	var out string
	if ok {
		out = fmt.Sprintf("%s%s.%02d", symbol, grouped, minor)
	} else {
		out = fmt.Sprintf("%s %s.%02d", strings.ToUpper(currency), grouped, minor)
	}

	if neg {
		return "-" + out
	}
	return out
}

var currencySymbols = map[string]string{
	"USD": "$",
	"EUR": "€",
	"GBP": "£",
	"JPY": "¥",
}

// groupThousands inserts "," every three digits from the right, e.g.
// groupThousands(1234567) -> "1,234,567". n must be >= 0.
func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// Unix renders a unix timestamp (seconds) as a short, fixed-width
// "YYYY-MM-DD HH:MM:SS" UTC string. A zero timestamp renders as "-".
func Unix(sec int64) string {
	if sec == 0 {
		return "-"
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04:05")
}

// CardStatusLabel renders a common.CardStatus enum as a short label used in
// table cells; the color is chosen separately by CardStatusColor so this
// stays presentation-only and terminal-free.
func CardStatusLabel(s commonpb.CardStatus) string {
	switch s {
	case commonpb.CardStatus_CARD_STATUS_ACTIVE:
		return "ACTIVE"
	case commonpb.CardStatus_CARD_STATUS_RATIONED:
		return "RATIONED"
	case commonpb.CardStatus_CARD_STATUS_SUSPENDED:
		return "SUSPENDED"
	case commonpb.CardStatus_CARD_STATUS_CLOSED:
		return "CLOSED"
	default:
		return "UNKNOWN"
	}
}

// DepositStatusLabel renders a common.DepositStatus enum as a short label.
func DepositStatusLabel(s commonpb.DepositStatus) string {
	switch s {
	case commonpb.DepositStatus_DEPOSIT_STATUS_EXPECTED:
		return "EXPECTED"
	case commonpb.DepositStatus_DEPOSIT_STATUS_RECEIVED:
		return "RECEIVED"
	case commonpb.DepositStatus_DEPOSIT_STATUS_LATE:
		return "LATE"
	default:
		return "UNKNOWN"
	}
}

// Truncate shortens s to at most width runes, appending an ellipsis when it
// had to cut, so table cells never wrap and blow up row height.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return string(r[:1])
	}
	return string(r[:width-1]) + "…"
}

// AgentActionLabel renders a common.AgentAction enum as a short label
// used in the Decisions tab's ACTION column; the color is chosen
// separately (see view.agentActionStyle) so this stays
// presentation-only.
func AgentActionLabel(a commonpb.AgentAction) string {
	switch a {
	case commonpb.AgentAction_AGENT_ACTION_ISSUE_CARD:
		return "ISSUE_CARD"
	case commonpb.AgentAction_AGENT_ACTION_ADJUST_LIMIT:
		return "ADJUST_LIMIT"
	case commonpb.AgentAction_AGENT_ACTION_RATION_BRIDGE:
		return "RATION_BRIDGE"
	case commonpb.AgentAction_AGENT_ACTION_RELEASE_HOLD:
		return "RELEASE_HOLD"
	case commonpb.AgentAction_AGENT_ACTION_DECLINE:
		return "DECLINE"
	case commonpb.AgentAction_AGENT_ACTION_NOTIFY:
		return "NOTIFY"
	default:
		return "UNSPECIFIED"
	}
}

// SanitizedCell strips terminal-control-character injection risk
// (pkg/textsanitize, docs/THREAT-MODEL.md's terminal-escape-sequence
// gap) from LLM-authored free text, then truncates it to width runes
// for table-cell display. Applying sanitize before truncate (not the
// other way around) matters: a truncated-but-unsanitized string could
// still carry an unterminated escape sequence that bleeds into every
// cell rendered after it.
func SanitizedCell(s string, width int) string {
	return Truncate(textsanitize.StripControlChars(s), width)
}
