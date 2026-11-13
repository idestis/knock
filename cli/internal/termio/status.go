package termio

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// StatusLine reserves the bottom row of the terminal: the rows above it are
// a scroll region the PTY draws into.
type StatusLine struct {
	t    *Terminal
	rows int
	cols int
	on   bool
}

// NewStatusLine returns a status line for t. It does nothing until Setup.
func NewStatusLine(t *Terminal) *StatusLine { return &StatusLine{t: t} }

// Setup clears the screen and limits scrolling to rows-1. It returns the
// number of rows the PTY gets. Terminals under 3 rows get no status line.
func (s *StatusLine) Setup(cols, rows int) int {
	s.cols, s.rows = cols, rows
	if rows < 3 {
		s.on = false
		s.t.Printf("\x1b[2J\x1b[H")
		return rows
	}
	s.on = true
	s.t.Printf("\x1b[2J\x1b[1;%dr\x1b[H", rows-1)
	return rows - 1
}

// Resize applies a new terminal size and returns the PTY rows.
func (s *StatusLine) Resize(cols, rows int) int {
	if !s.on || rows < 3 {
		s.cols, s.rows = cols, rows
		return rows
	}
	s.cols, s.rows = cols, rows
	// DECSTBM homes the cursor, so save and restore around it.
	s.t.Printf("\x1b7\x1b[1;%dr\x1b8", rows-1)
	return rows - 1
}

// Draw paints text (which may hold ANSI styles) on the bottom row.
func (s *StatusLine) Draw(text string) {
	if !s.on {
		return
	}
	text = ansi.Truncate(text, s.cols, "…")
	pad := s.cols - ansi.StringWidth(text)
	if pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	s.t.Printf("\x1b7\x1b[%d;1H\x1b[2K%s\x1b[0m\x1b8", s.rows, text)
}

// Teardown restores full-screen scrolling and clears the status row.
func (s *StatusLine) Teardown() {
	if !s.on {
		return
	}
	s.on = false
	s.t.Printf("\x1b7\x1b[r\x1b8\x1b[%d;1H\x1b[2K\r\n", s.rows)
}

// Elapsed formats a duration in seconds as mm:ss (or h:mm:ss).
func Elapsed(secs int) string {
	if secs >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	}
	return fmt.Sprintf("%02d:%02d", secs/60, secs%60)
}
