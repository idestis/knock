package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

//go:generate sh -c "cd ../../.. && swift cli/internal/ui/gen/mark.swift > cli/internal/ui/mark_grid.go"

// Gradient stops for the banner, top-left to bottom-right: a light sky blue
// into the logo blue.
var bannerStops = [][3]float64{
	{0x9C, 0xC0, 0xFF},
	{0x4D, 0x86, 0xFF},
	{0x00, 0x43, 0xF3},
}

func bannerColor(t float64) string {
	if t <= 0 {
		t = 0
	}
	if t >= 1 {
		t = 1
	}
	seg := t * float64(len(bannerStops)-1)
	i := int(seg)
	if i >= len(bannerStops)-1 {
		i = len(bannerStops) - 2
	}
	f := seg - float64(i)
	a, b := bannerStops[i], bannerStops[i+1]
	mix := func(k int) int { return int(a[k] + (b[k]-a[k])*f + 0.5) }
	return fmt.Sprintf("#%02X%02X%02X", mix(0), mix(1), mix(2))
}

// Banner draws the K mark with half blocks: each text row holds two pixel rows.
func Banner(indent int) string {
	h := len(markGrid)
	w := len(markGrid[0])
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	for y := 0; y < h; y += 2 {
		b.WriteString(pad)
		for x := 0; x < w; x++ {
			top := markGrid[y][x] == '#'
			bottom := y+1 < h && markGrid[y+1][x] == '#'
			ch := " "
			switch {
			case top && bottom:
				ch = "█"
			case top:
				ch = "▀"
			case bottom:
				ch = "▄"
			}
			if ch == " " {
				b.WriteString(ch)
				continue
			}
			t := (float64(x)/float64(w-1) + float64(y)/float64(h-1)) / 2
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(bannerColor(t))).Render(ch))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// HelpSection is one block of the help screen.
type HelpSection struct {
	Title string
	Rows  [][2]string // usage, description
}

// HelpScreen renders the help with the banner for an interactive terminal.
func HelpScreen(title, tagline string, sections []HelpSection, footer string) string {
	head := lipgloss.NewStyle().Foreground(lipgloss.Color("#3D7BFF")).Bold(true)
	usage := lipgloss.NewStyle().Bold(true)
	width := 0
	for _, s := range sections {
		for _, r := range s.Rows {
			width = max(width, lipgloss.Width(r[0]))
		}
	}
	var b strings.Builder
	b.WriteString("\n" + Banner(2) + "\n\n")
	b.WriteString("  " + Bold.Render(title) + "\n")
	b.WriteString("  " + Dim.Render(tagline) + "\n")
	for _, s := range sections {
		b.WriteString("\n  " + head.Render(s.Title) + "\n")
		for _, r := range s.Rows {
			gap := strings.Repeat(" ", width-lipgloss.Width(r[0])+3)
			b.WriteString("    " + usage.Render(r[0]) + gap + Dim.Render(r[1]) + "\n")
		}
	}
	if footer != "" {
		b.WriteString("\n  " + Dim.Render(footer) + "\n")
	}
	return b.String()
}

// HelpPlain renders the same help as plain text, for pipes and files.
func HelpPlain(tagline string, sections []HelpSection, footer string) string {
	width := 0
	for _, s := range sections {
		for _, r := range s.Rows {
			width = max(width, len(r[0]))
		}
	}
	var b strings.Builder
	b.WriteString(tagline + "\n")
	for _, s := range sections {
		b.WriteString("\n" + s.Title + ":\n")
		for _, r := range s.Rows {
			b.WriteString(fmt.Sprintf("  %-*s   %s\n", width, r[0], r[1]))
		}
	}
	if footer != "" {
		b.WriteString("\n" + footer + "\n")
	}
	return b.String()
}
