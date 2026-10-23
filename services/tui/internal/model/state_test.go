package model

import (
	"testing"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/view"
)

func TestNextTabWraps(t *testing.T) {
	cases := []struct {
		from, want Tab
	}{
		{TabTenants, TabDeposits},
		{TabDeposits, TabCards},
		{TabCards, TabDecisions},
		{TabDecisions, TabActivity},
		{TabActivity, TabTenants},
	}
	for _, tc := range cases {
		if got := NextTab(tc.from); got != tc.want {
			t.Errorf("NextTab(%v) = %v, want %v", tc.from, got, tc.want)
		}
	}
}

func TestPrevTabWraps(t *testing.T) {
	cases := []struct {
		from, want Tab
	}{
		{TabDeposits, TabTenants},
		{TabCards, TabDeposits},
		{TabDecisions, TabCards},
		{TabActivity, TabDecisions},
		{TabTenants, TabActivity},
	}
	for _, tc := range cases {
		if got := PrevTab(tc.from); got != tc.want {
			t.Errorf("PrevTab(%v) = %v, want %v", tc.from, got, tc.want)
		}
	}
}

func TestTabString(t *testing.T) {
	if got := TabTenants.String(); got != "Tenants" {
		t.Errorf("TabTenants.String() = %q, want %q", got, "Tenants")
	}
	if got := Tab(99).String(); got != "?" {
		t.Errorf("out-of-range Tab.String() = %q, want %q", got, "?")
	}
}

func TestClampCursor(t *testing.T) {
	cases := []struct {
		cursor, length, want int
	}{
		{0, 0, 0},
		{5, 0, 0},
		{-1, 10, 0},
		{3, 10, 3},
		{20, 10, 9},
		{9, 10, 9},
	}
	for _, tc := range cases {
		if got := clampCursor(tc.cursor, tc.length); got != tc.want {
			t.Errorf("clampCursor(%d, %d) = %d, want %d", tc.cursor, tc.length, got, tc.want)
		}
	}
}

func TestPrependActivity(t *testing.T) {
	var rows []view.ActivityRow
	for i := 0; i < 5; i++ {
		rows = prependActivity(rows, view.ActivityRow{ID: string(rune('a' + i))}, 3)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	// Newest (last appended, id "e") must be at the front.
	if rows[0].ID != "e" {
		t.Errorf("rows[0].ID = %q, want %q (newest first)", rows[0].ID, "e")
	}
	if rows[1].ID != "d" || rows[2].ID != "c" {
		t.Errorf("rows = %+v, want newest-first [e d c]", rows)
	}
}

func TestPrependActivityUnderCap(t *testing.T) {
	var rows []view.ActivityRow
	rows = prependActivity(rows, view.ActivityRow{ID: "a"}, 10)
	rows = prependActivity(rows, view.ActivityRow{ID: "b"}, 10)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].ID != "b" || rows[1].ID != "a" {
		t.Errorf("rows = %+v, want [b a]", rows)
	}
}

func TestBuildTenantNames(t *testing.T) {
	tenants := []*ledgerpb.Tenant{
		{Id: "t1", Name: "Acme"},
		{Id: "t2", Name: "Globex"},
	}
	names := buildTenantNames(tenants)
	if names["t1"] != "Acme" || names["t2"] != "Globex" {
		t.Errorf("buildTenantNames = %+v, want t1=Acme t2=Globex", names)
	}
	if len(names) != 2 {
		t.Errorf("len(names) = %d, want 2", len(names))
	}
}

func TestResolveTenantName(t *testing.T) {
	names := map[string]string{"t1": "Acme"}
	if got := resolveTenantName(names, "t1"); got != "Acme" {
		t.Errorf("resolveTenantName known = %q, want %q", got, "Acme")
	}
	if got := resolveTenantName(names, "unknown-id"); got != "unknown-id" {
		t.Errorf("resolveTenantName unknown = %q, want fallback to raw id", got)
	}
}

func TestTabLabelsOrderMatchesConstants(t *testing.T) {
	labels := TabLabels()
	if len(labels) != tabCount {
		t.Fatalf("len(TabLabels()) = %d, want %d", len(labels), tabCount)
	}
	if labels[TabTenants] != "Tenants" || labels[TabActivity] != "Activity" {
		t.Errorf("TabLabels() = %v, order must match Tab constants", labels)
	}
}
