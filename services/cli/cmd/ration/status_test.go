package main

import (
	"strings"
	"testing"
)

// Regression test for the threat-model gap documented in
// docs/THREAT-MODEL.md ("A sufficiently adversarial trigger->prompt->
// rationale chain could, in principle, inject ANSI escapes into an
// operator's terminal"): LLM-authored rationale text must never reach the
// terminal with raw C0/C1 control characters still in it.
func TestSanitizeControlChars_StripsANSIEscapes(t *testing.T) {
	// ESC [ 2 J is "clear screen"; ESC [ 8 ; 1 H is "move cursor".
	malicious := "approved\x1b[2Jhidden\x1b[8;1Hinjected"

	got := sanitizeControlChars(malicious)

	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("sanitizeControlChars(%q) = %q, still contains ESC (0x1b)", malicious, got)
	}
	if strings.Contains(got, "approved") == false || strings.Contains(got, "hidden") == false || strings.Contains(got, "injected") == false {
		t.Errorf("sanitizeControlChars(%q) = %q, lost legitimate text", malicious, got)
	}
}

func TestSanitizeControlChars_StripsC1AndKeepsNewlineTab(t *testing.T) {
	// C1 control range (0x80-0x9f encoded as UTF-8, e.g. U+0085 NEL) must
	// also be stripped, while ordinary whitespace used by the tabwriter
	// (newline, tab) must survive.
	s := "line1\u0085line2\tcol2\nline3"

	got := sanitizeControlChars(s)

	if strings.ContainsRune(got, 0x85) {
		t.Errorf("sanitizeControlChars(%q) = %q, still contains C1 NEL (U+0085)", s, got)
	}
	if !strings.Contains(got, "\t") || !strings.Contains(got, "\n") {
		t.Errorf("sanitizeControlChars(%q) = %q, stripped tab/newline too", s, got)
	}
}

func TestSanitizeControlChars_LeavesPlainTextUntouched(t *testing.T) {
	s := "partial bridge grant: 24h velocity cap engaged"
	if got := sanitizeControlChars(s); got != s {
		t.Errorf("sanitizeControlChars(%q) = %q, want unchanged", s, got)
	}
}

func TestTruncate_SanitizesBeforeSlicing(t *testing.T) {
	// truncate is what status.go's printDecisions actually calls on
	// Rationale before writing it to the tabwriter — the fix must be
	// effective at that call site, not just in a helper nothing uses.
	malicious := "ok\x1b[2J" + strings.Repeat("x", 100)

	got := truncate(sanitizeControlChars(malicious), 80)

	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("truncate(sanitizeControlChars(%q), 80) = %q, still contains ESC", malicious, got)
	}
}
