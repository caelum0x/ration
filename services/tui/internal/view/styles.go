// Package view renders one Bubble Tea tab each: tenants, deposits, cards,
// and the live activity feed. Every function here is a pure
// data-in/string-out renderer — no gRPC calls, no mutable state — so the
// model package owns all state and these just draw it.
package view

import "github.com/charmbracelet/lipgloss"

var (
	colorFg       = lipgloss.Color("255")
	colorMuted    = lipgloss.Color("244")
	colorBorder   = lipgloss.Color("238")
	colorAccent   = lipgloss.Color("212")
	colorGood     = lipgloss.Color("42")
	colorWarn     = lipgloss.Color("214")
	colorBad      = lipgloss.Color("203")
	colorInfo     = lipgloss.Color("75")
	colorSelected = lipgloss.Color("236")
)

// HeaderStyle renders a tab's bold title line above its table.
var HeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 0, 1, 0)

// TableHeaderStyle renders column headers: bold, muted, underlined via a
// border on the bottom edge drawn by the caller.
var TableHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(colorFg)

// RowStyle renders a normal, unselected table row.
var RowStyle = lipgloss.NewStyle().Foreground(colorFg)

// SelectedRowStyle highlights the row under the cursor.
var SelectedRowStyle = lipgloss.NewStyle().Foreground(colorFg).Background(colorSelected).Bold(true)

// MutedStyle renders secondary/help text.
var MutedStyle = lipgloss.NewStyle().Foreground(colorMuted)

// ErrorStyle renders error/disconnected banners.
var ErrorStyle = lipgloss.NewStyle().Foreground(colorBad).Bold(true)

// EmptyStyle renders the "no rows yet" placeholder.
var EmptyStyle = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)

// cardStatusStyle maps a card status label to a color-coded style so
// ACTIVE/RATIONED/SUSPENDED/CLOSED are visually distinct at a glance.
func cardStatusStyle(label string) lipgloss.Style {
	switch label {
	case "ACTIVE":
		return lipgloss.NewStyle().Foreground(colorGood).Bold(true)
	case "RATIONED":
		return lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
	case "SUSPENDED":
		return lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	case "CLOSED":
		return lipgloss.NewStyle().Foreground(colorMuted)
	default:
		return lipgloss.NewStyle().Foreground(colorMuted)
	}
}

// depositStatusStyle maps a deposit status label to a color-coded style.
func depositStatusStyle(label string) lipgloss.Style {
	switch label {
	case "RECEIVED":
		return lipgloss.NewStyle().Foreground(colorGood).Bold(true)
	case "LATE":
		return lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	case "EXPECTED":
		return lipgloss.NewStyle().Foreground(colorInfo)
	default:
		return lipgloss.NewStyle().Foreground(colorMuted)
	}
}

// agentActionStyle maps an agent-decision action label (see
// format.AgentActionLabel) to a color-coded style, so the Decisions
// tab's granted/declined outcomes are visually distinct at a glance —
// the same purpose cardStatusStyle/depositStatusStyle serve for their
// own tabs.
func agentActionStyle(label string) lipgloss.Style {
	switch label {
	case "RATION_BRIDGE", "ISSUE_CARD", "ADJUST_LIMIT", "RELEASE_HOLD":
		return lipgloss.NewStyle().Foreground(colorGood).Bold(true)
	case "DECLINE":
		return lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	case "NOTIFY":
		return lipgloss.NewStyle().Foreground(colorInfo)
	default:
		return lipgloss.NewStyle().Foreground(colorMuted)
	}
}

// activityKindStyle color-codes the activity feed by event kind so the
// live stream is scannable without reading every line.
func activityKindStyle(kind string) lipgloss.Style {
	switch kind {
	case "deposit":
		return lipgloss.NewStyle().Foreground(colorInfo).Bold(true)
	case "card":
		return lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	case "agent_decision":
		return lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
	case "ledger_entry":
		return lipgloss.NewStyle().Foreground(colorGood).Bold(true)
	default:
		return lipgloss.NewStyle().Foreground(colorMuted)
	}
}
