package view

import "testing"

func TestPadCell(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"abc", 5, "abc  "},
		{"abc", 3, "abc"},
		{"abcdef", 3, "ab…"},
		{"", 4, "    "},
		{"abc", 0, ""},
		{"abc", -1, ""},
	}
	for _, tc := range cases {
		got := padCell(tc.s, tc.width)
		if got != tc.want {
			t.Errorf("padCell(%q, %d) = %q (len %d), want %q (len %d)", tc.s, tc.width, got, len([]rune(got)), tc.want, len([]rune(tc.want)))
		}
	}
}

func TestRenderRow(t *testing.T) {
	got := renderRow([]string{"a", "bb"}, []int{3, 3})
	want := "a  " + columnGap + "bb "
	if got != want {
		t.Errorf("renderRow = %q, want %q", got, want)
	}
}

func TestTotalWidth(t *testing.T) {
	cases := []struct {
		widths []int
		want   int
	}{
		{[]int{5}, 5},
		{[]int{5, 5}, 5 + len([]rune(columnGap)) + 5},
		{nil, 1},
	}
	for _, tc := range cases {
		got := totalWidth(tc.widths)
		if got != tc.want {
			t.Errorf("totalWidth(%v) = %d, want %d", tc.widths, got, tc.want)
		}
	}
}

func TestTableEmptyRows(t *testing.T) {
	got := Table([]string{"A", "B"}, nil, []int{3, 3}, 0, -1, nil)
	if got == "" {
		t.Fatal("Table with no rows should still render headers + placeholder")
	}
}
