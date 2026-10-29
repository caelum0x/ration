package view

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// FormFieldRow is the gRPC/tea-free shape model.cardForm's fields are
// converted to before rendering, so CardForm stays a pure data-in/
// string-out renderer like every other view.* function (see package
// doc comment).
type FormFieldRow struct {
	Label   string
	Value   string
	Picker  bool
	Focused bool
}

var formBoxStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorAccent).
	Padding(1, 2)

var formLabelStyle = lipgloss.NewStyle().Foreground(colorMuted)
var formValueStyle = lipgloss.NewStyle().Foreground(colorFg)
var formFocusedValueStyle = lipgloss.NewStyle().Foreground(colorFg).Background(colorSelected).Bold(true)

// CardForm renders the TUI's one interactive modal — the "issue card"/
// "adjust limit" form (model/cardform.go) — as a bordered box over the
// Cards tab. infoLine is shown read-only above the fields when non-empty
// (the adjust-limit form's "which card" context); errMsg, when non-empty,
// is shown below the fields in the same error color every other tab
// already uses for a connection error.
func CardForm(fields []FormFieldRow, infoLine, errMsg string, submitting bool) string {
	var b strings.Builder

	if infoLine != "" {
		b.WriteString(MutedStyle.Render(infoLine))
		b.WriteString("\n\n")
	}

	for _, f := range fields {
		valueStyle := formValueStyle
		cursor := " "
		if f.Focused {
			valueStyle = formFocusedValueStyle
			cursor = "›"
		}
		value := f.Value
		if value == "" {
			value = " "
		}
		if f.Picker {
			value = "‹ " + value + " ›"
		}
		b.WriteString(fmt.Sprintf("%s %s  %s\n", cursor, formLabelStyle.Render(padCell(f.Label, 32)), valueStyle.Render(value)))
	}

	if submitting {
		b.WriteString("\n")
		b.WriteString(MutedStyle.Render("submitting…"))
	}
	if errMsg != "" {
		b.WriteString("\n")
		b.WriteString(ErrorStyle.Render("error: " + errMsg))
	}

	return formBoxStyle.Render(b.String())
}
