package view

import "testing"

func TestVisibleWindow(t *testing.T) {
	cases := []struct {
		name                  string
		total, cursor, height int
		wantStart, wantEnd    int
	}{
		{"fits entirely", 3, 0, 10, 0, 3},
		{"zero total", 0, 0, 10, 0, 0},
		{"zero height", 5, 0, 0, 0, 0},
		{"cursor at top", 20, 0, 5, 0, 5},
		{"cursor in middle", 20, 10, 5, 8, 13},
		{"cursor near end clamps window", 20, 19, 5, 15, 20},
		{"cursor beyond total clamps", 20, 50, 5, 15, 20},
		{"negative cursor clamps to 0", 20, -5, 5, 0, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := visibleWindow(tc.total, tc.cursor, tc.height)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("visibleWindow(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.total, tc.cursor, tc.height, start, end, tc.wantStart, tc.wantEnd)
			}
			if end-start > tc.height && tc.height > 0 {
				t.Errorf("window size %d exceeds height %d", end-start, tc.height)
			}
		})
	}
}

func TestActivityEmpty(t *testing.T) {
	got := Activity(nil, 0, 10)
	if got == "" {
		t.Fatal("Activity with no rows should render a waiting placeholder")
	}
}

func TestActivityRendersRows(t *testing.T) {
	rows := []ActivityRow{
		{ID: "1", TenantName: "Acme", Kind: "deposit", Summary: "received $100", AtUnix: 1609459200},
	}
	got := Activity(rows, 0, 10)
	if got == "" {
		t.Fatal("expected non-empty render")
	}
}
