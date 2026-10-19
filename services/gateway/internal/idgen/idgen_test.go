package idgen

import (
	"strings"
	"testing"
)

func TestNew_HasPrefixAndIsUnique(t *testing.T) {
	a := New("card")
	b := New("card")

	if !strings.HasPrefix(a, "card_") {
		t.Errorf("New(%q) = %q, want prefix %q", "card", a, "card_")
	}
	if a == b {
		t.Errorf("New() returned the same ID twice: %q", a)
	}
	// prefix_ + 32 hex chars
	if len(a) != len("card_")+32 {
		t.Errorf("len(New()) = %d, want %d", len(a), len("card_")+32)
	}
}

func TestNew_DifferentPrefixes(t *testing.T) {
	if got := New(""); !strings.HasPrefix(got, "_") {
		t.Errorf("New(\"\") = %q, want prefix %q", got, "_")
	}
}
