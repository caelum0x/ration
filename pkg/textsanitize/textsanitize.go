// Package textsanitize strips terminal-escape-sequence injection risk
// out of free text before it is ever written to an operator's
// terminal.
//
// The risk this closes is documented in docs/THREAT-MODEL.md: LLM-
// authored, attacker-influenceable text (an agent decision's
// Rationale/Trigger, ultimately derived from a trigger->prompt->
// rationale chain an adversarial deposit/webhook payload could steer)
// is printed verbatim in more than one place now — the CLI's `status`
// command, and services/tui's "Agent Decisions" tab. A sufficiently
// adversarial chain could, in principle, inject ANSI/VT100 escape
// sequences (cursor manipulation, hidden text, even OSC 8 hyperlink or
// terminal-title tricks) into whichever terminal eventually renders it.
//
// Originally lived only in services/cli/cmd/ration/status.go
// (sanitizeControlChars); extracted here so every new surface that
// renders this same LLM-authored text — starting with the TUI's
// decisions tab — gets the identical protection applied before
// rendering, not bolted on after the fact.
package textsanitize

import (
	"strings"
	"unicode"
)

// StripControlChars removes C0 and C1 control characters (including
// ESC, so no ANSI/VT100 escape sequence can survive) from s.
//
// Ordinary whitespace (tab, newline, carriage return) is preserved
// since typical terminal rendering (a tabwriter, a line-wrapped
// textarea) relies on it for layout; every other control character
// (0x00-0x1F, 0x7F, and the UTF-8-encoded C1 range U+0080-U+009F) is
// dropped.
func StripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
