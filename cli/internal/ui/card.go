package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

// DoorCard renders the host's door and the one code to share ("BVJY 1696 ME").
func DoorCard(who, share string, recording string) string {
	rows := []string{
		Bold.Render("Your door is open") + Dim.Render("  ·  "+who),
		"",
		Dim.Render("Your code"),
		CodeTag.Render(share),
		"",
		Dim.Render("They knock with:  ") + Brand.Render("knock "+share),
	}
	if recording != "" {
		rows = append(rows, "", Dim.Render("Sessions are recorded ("+recording+")."))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Accent).
		Padding(1, 3).
		Render(strings.Join(rows, "\n"))
}

// Table renders rows with a header in the Knock style.
func Table(headers []string, rows [][]string, style func(row, col int) lipgloss.Style) string {
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).
		BorderStyle(Dim).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			base := lipgloss.NewStyle().PaddingRight(3)
			if row == table.HeaderRow {
				return base.Inherit(Dim).Bold(true)
			}
			if style != nil {
				return base.Inherit(style(row, col))
			}
			return base
		})
	return t.Render()
}
