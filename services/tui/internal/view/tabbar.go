package view

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	tabActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorAccent).Padding(0, 2)
	tabInactiveStyle = lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)
)

// TabBar renders the "tenants | deposits | cards | activity" switcher with
// the active tab highlighted and its 1-based number shown, so both number
// keys and tab/shift+tab are discoverable from the UI itself.
func TabBar(names []string, active int) string {
	cells := make([]string, len(names))
	for i, name := range names {
		label := fmt.Sprintf("%d %s", i+1, name)
		if i == active {
			cells[i] = tabActiveStyle.Render(label)
		} else {
			cells[i] = tabInactiveStyle.Render(label)
		}
	}
	return strings.Join(cells, " ")
}
