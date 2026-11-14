package termio

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// StatusLine reserves the bottom row of the terminal: the rows above it are
// a scroll region the PTY draws into.
type StatusLine struct {
	t    *Terminal
	rows int // the status line sits on this row
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

// Fit is for a guest mirroring a host PTY of contentRows rows. The scroll
// region becomes exactly those rows (or as many as fit) and the status line
// sits right under it, so the host's cursor moves and scrolling land on the
// same rows they do on the host. Rows below the status line are cleared.
func (s *StatusLine) Fit(cols, termRows, contentRows int) {
	s.cols = cols
	if termRows < 3 {
		s.on, s.rows = false, termRows
		return
	}
	region := termRows - 1
	if contentRows > 0 && contentRows < region {
		region = contentRows
	}
	s.on, s.rows = true, region+1
	// DECSTBM homes the cursor, so save and restore around it; then clear
	// everything below the status row so stale output doesn't linger there.
	s.t.Printf("\x1b7\x1b[1;%dr\x1b[%d;1H\x1b[J\x1b8", region, region+1)
}

// ContentRows is how many rows the mirrored program may scroll in.
func (s *StatusLine) ContentRows() int {
	if !s.on {
		return s.rows
	}
	return s.rows - 1
}

// Reassert puts the scroll region back after the mirrored program reset it.
func (s *StatusLine) Reassert() {
	if s.on {
		s.t.Printf("\x1b7\x1b[1;%dr\x1b8", s.rows-1)
	}
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

// Width is the status line's width in columns.
func (s *StatusLine) Width() int { return s.cols }

// Elapsed formats a duration in seconds as mm:ss (or h:mm:ss).
func Elapsed(secs int) string {
	if secs >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	}
	return fmt.Sprintf("%02d:%02d", secs/60, secs%60)
}

// ResetsScrollRegion reports whether b holds a DECSTBM (CSI [top;bottom] r)
// whose bottom is missing or below row maxRow. A full-screen program sends
// "CSI r" to reset to the whole screen, which on the guest would scroll the
// status line away; a region inside maxRow (vim, less) is left alone.
// A sequence split across two chunks is missed; the next one catches up.
func ResetsScrollRegion(b []byte, maxRow int) bool {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != 0x1b || b[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == ';') {
			j++
		}
		if j >= len(b) || b[j] != 'r' {
			continue
		}
		params := string(b[i+2 : j])
		semi := strings.LastIndexByte(params, ';')
		if semi < 0 {
			return true // no bottom: the whole screen
		}
		bottom, err := strconv.Atoi(params[semi+1:])
		if err != nil || bottom > maxRow {
			return true
		}
	}
	return false
}
