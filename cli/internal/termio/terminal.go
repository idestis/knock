package termio

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// Terminal is the local terminal during a session. Writes are serialised so
// the status line never lands inside a chunk of shell output.
type Terminal struct {
	In  io.Reader
	Out io.Writer

	// SizeFunc overrides size detection (tests). Nil uses the real terminal.
	SizeFunc func() (cols, rows int)

	mu    sync.Mutex
	saved *term.State
}

// Std is the process's own terminal.
func Std() *Terminal { return &Terminal{In: os.Stdin, Out: os.Stdout} }

func fdOf(v any) (int, bool) {
	f, ok := v.(*os.File)
	if !ok {
		return 0, false
	}
	fd := int(f.Fd())
	return fd, term.IsTerminal(fd)
}

// IsTTY reports whether input is an interactive terminal.
func (t *Terminal) IsTTY() bool {
	_, ok := fdOf(t.In)
	return ok
}

// Size returns columns and rows, 80x24 when unknown.
func (t *Terminal) Size() (cols, rows int) {
	if t.SizeFunc != nil {
		return t.SizeFunc()
	}
	if fd, ok := fdOf(t.Out); ok {
		if c, r, err := term.GetSize(fd); err == nil && c > 0 && r > 0 {
			return c, r
		}
	}
	if fd, ok := fdOf(t.In); ok {
		if c, r, err := term.GetSize(fd); err == nil && c > 0 && r > 0 {
			return c, r
		}
	}
	return 80, 24
}

// MakeRaw switches input to raw mode. A no-op when input is not a terminal.
func (t *Terminal) MakeRaw() error {
	fd, ok := fdOf(t.In)
	if !ok {
		return nil
	}
	st, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("switch the terminal to raw mode: %w", err)
	}
	t.saved = st
	return nil
}

// Restore undoes MakeRaw. Safe to call more than once.
func (t *Terminal) Restore() {
	if t.saved == nil {
		return
	}
	if fd, ok := fdOf(t.In); ok {
		_ = term.Restore(fd, t.saved)
	}
	t.saved = nil
}

// Write sends p to the terminal as one unit.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Out.Write(p)
}

// Printf writes formatted text as one unit.
func (t *Terminal) Printf(format string, a ...any) {
	_, _ = t.Write(fmt.Appendf(nil, format, a...))
}

// Reader returns a reader over In that Cancel can interrupt, so a session
// can stop reading the keyboard without stealing the next keystroke.
func (t *Terminal) Reader() (cancelreader.CancelReader, error) {
	return cancelreader.NewReader(t.In)
}
