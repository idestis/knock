// Package ui holds the Bubble Tea and Lip Gloss pieces: the door card, the
// "Let them in?" prompt, the knocking spinner, pickers and tables.
package ui

import (
	"fmt"
	"io"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// Accent is the logo blue.
var Accent = lipgloss.Color("#0043F3")

var (
	Bold    = lipgloss.NewStyle().Bold(true)
	Dim     = lipgloss.NewStyle().Faint(true)
	Brand   = lipgloss.NewStyle().Foreground(lipgloss.Color("#3D7BFF")).Bold(true)
	Good    = lipgloss.NewStyle().Foreground(lipgloss.Color("#22A55B"))
	Warn    = lipgloss.NewStyle().Foreground(lipgloss.Color("#D98E04"))
	Bad     = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5484D"))
	CodeTag = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(Accent).Padding(0, 1)
)

// Out is where messages go; stderr for warnings.
var (
	Out io.Writer = colorprofile.NewWriter(os.Stdout, os.Environ())
	Err io.Writer = colorprofile.NewWriter(os.Stderr, os.Environ())
	// tty is what Bubble Tea programs draw on; they downsample themselves.
	tty io.Writer = os.Stdout
)

// Say prints a line.
func Say(format string, a ...any) { _, _ = fmt.Fprintln(Out, fmt.Sprintf(format, a...)) }

// Note prints a dim line.
func Note(format string, a ...any) {
	_, _ = fmt.Fprintln(Out, Dim.Render(fmt.Sprintf(format, a...)))
}

// Warning prints a yellow line to stderr.
func Warning(format string, a ...any) {
	_, _ = fmt.Fprintln(Err, Warn.Render("! "+fmt.Sprintf(format, a...)))
}

// Failure prints a red line to stderr.
func Failure(msg string) { _, _ = fmt.Fprintln(Err, Bad.Render(msg)) }

// Print writes pre-rendered text. Out is a colorprofile writer, which
// downsamples colour for the terminal it detects (and strips it for pipes).
// Don't route through lipgloss.Fprintln here: it treats the wrapper as a
// non-terminal and drops every colour.
func Print(s string) { _, _ = fmt.Fprintln(Out, s) }
