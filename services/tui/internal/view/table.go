package view

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/caelum0x/ration/services/tui/internal/format"
)

const columnGap = " │ "

// padCell truncates s to width runes (appending an ellipsis if cut) and
// pads with trailing spaces so every cell in a column lines up exactly.
// Pure and terminal-independent, which is what makes it unit testable.
func padCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	t := format.Truncate(s, width)
	pad := width - len([]rune(t))
	if pad <= 0 {
		return t
	}
	return t + strings.Repeat(" ", pad)
}

// renderRow lays out one row's cells against the given column widths,
// joined by columnGap. Used for both the header row and data rows so
// alignment is always identical between them.
func renderRow(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		parts[i] = padCell(c, w)
	}
	return strings.Join(parts, columnGap)
}

// Table renders a header row (bold) followed by a muted divider and each
// data row, highlighting the row at cursor (clamped into range). statusCol,
// when >= 0, is colored per-row using statusStyle(rows[i][statusCol]).
func Table(headers []string, rows [][]string, widths []int, cursor int, statusCol int, statusStyle func(string) lipgloss.Style) string {
	var b strings.Builder

	b.WriteString(TableHeaderStyle.Render(renderRow(headers, widths)))
	b.WriteString("\n")
	b.WriteString(MutedStyle.Render(strings.Repeat("─", totalWidth(widths))))
	b.WriteString("\n")

	if len(rows) == 0 {
		b.WriteString(EmptyStyle.Render("(no rows yet)"))
		return b.String()
	}

	for i, row := range rows {
		line := renderRow(row, widths)
		if statusCol >= 0 && statusCol < len(row) && statusStyle != nil {
			line = colorizeStatusCell(row, widths, statusCol, statusStyle)
		}
		if i == cursor {
			line = SelectedRowStyle.Render(stripForSelection(renderRow(row, widths)))
		} else if statusCol < 0 || statusStyle == nil {
			line = RowStyle.Render(line)
		}
		b.WriteString(line)
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// stripForSelection re-renders a row's plain text for the selected-row
// background highlight (the whole line gets one style, so per-cell status
// coloring is dropped in favor of the selection highlight for that row).
func stripForSelection(plain string) string {
	return plain
}

// colorizeStatusCell re-renders a row with every cell in its normal style
// except statusCol, which is colored via statusStyle based on its own
// trimmed content.
func colorizeStatusCell(row []string, widths []int, statusCol int, statusStyle func(string) lipgloss.Style) string {
	parts := make([]string, len(row))
	for i, c := range row {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		cell := padCell(c, w)
		if i == statusCol {
			cell = statusStyle(strings.TrimSpace(c)).Render(cell)
		}
		parts[i] = cell
	}
	return strings.Join(parts, columnGap)
}

func totalWidth(widths []int) int {
	total := 0
	for i, w := range widths {
		total += w
		if i > 0 {
			total += len([]rune(columnGap))
		}
	}
	if total < 1 {
		return 1
	}
	return total
}
