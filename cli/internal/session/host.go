package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/protocol"
)

// Host runs the host side of a session.
type Host struct {
	Common
	Command    []string // what the PTY runs
	GuestInput bool     // false starts in Hands off (view-only)
	Resize     <-chan os.Signal
	// End, when it receives a reason, ends the session from this side
	// (for example "code_changed" when knock new-code ran elsewhere).
	End <-chan string
}

// keysShownFor is how long the guest's last keys stay on the status line.
const keysShownFor = 5 * time.Second

// ribbonKeys is how many of the guest's keys the status line shows.
const ribbonKeys = 24

// statusStyle is a blue bar in 256 colours, which every terminal we care
// about supports (Terminal.app has no truecolor).
const (
	statusOn  = "\x1b[0;1;97;48;5;27m"
	statusDim = "\x1b[0;97;48;5;27m"
	statusRec = "\x1b[0;1;97;48;5;160m"
)

type hostState struct {
	mu         sync.Mutex
	guestInput bool
	lastGuest  time.Time
	keys       termio.KeyReader
	ribbon     termio.KeyRibbon
	lastDraw   time.Time
	notice     string
	noticeTill time.Time
}

// Run spawns the shell, shares it, and returns when the session ends.
func (h *Host) Run(ctx context.Context) Result {
	t := h.Term
	// Names come from the relay: strip anything that could redraw the
	// terminal before the status line or the shell sees them.
	h.Start = termio.CleanStart(h.Start)
	cols, rows := t.Size()
	status := termio.NewStatusLine(t)

	cmd := exec.Command(h.Command[0], h.Command[1:]...)
	cmd.Env = append(os.Environ(), "KNOCK_SESSION="+h.Start.SessionID, "KNOCK_PEER="+h.Start.Peer.Name)
	if os.Getenv("TERM") == "" {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}

	if err := t.MakeRaw(); err != nil {
		return Result{Reason: ReasonYouEnded, Err: err}
	}
	defer t.Restore()
	ptyRows := status.Setup(cols, rows)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(ptyRows)})
	if err != nil {
		status.Teardown()
		_ = h.Conn.Send(ctx, protocol.TypeSessionEnd, protocol.SessionEnd{SessionID: h.Start.SessionID, Reason: "host_ended"})
		return Result{Reason: ReasonYouEnded, Err: fmt.Errorf("start %s: %w", h.Command[0], err)}
	}

	rec, recErr := h.startRecording(protocol.RoleHost, cols, ptyRows)
	st := &hostState{guestInput: h.GuestInput, ribbon: termio.KeyRibbon{Max: ribbonKeys}}
	started := time.Now()
	res := Result{}
	if rec != nil {
		res.SavedTo = rec.Dir
	}
	if recErr != nil {
		res.Err = fmt.Errorf("couldn't start the recording: %w", recErr)
	}

	_ = h.Conn.Send(ctx, protocol.TypeSessionResize, protocol.SessionResize{Cols: cols, Rows: ptyRows})
	if !h.GuestInput {
		_ = h.Conn.Send(ctx, protocol.TypeSessionMode, protocol.SessionMode{GuestInput: false})
	}

	// Tell both people about the recording, in both terminals so they match.
	peer := peerFirstName(h.Start.Peer)
	if rec != nil {
		note := []byte("\x1b[2m[knock] This session is being recorded on the host's Mac.\x1b[0m\r\n")
		_, _ = t.Write(note)
		_ = h.Conn.WriteData(ctx, note)
	}

	draw := func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.lastDraw = time.Now()
		secs := int(time.Since(started) / time.Second)
		var b strings.Builder
		b.WriteString(statusOn + " " + h.Start.Peer.Name)
		if !h.Start.Peer.Verified {
			b.WriteString(statusDim + " (not verified)")
		}
		// Every key the guest sends shows here, echoed by the shell or not.
		keys := st.ribbon.String()
		showKeys := keys != "" && time.Since(st.lastGuest) < keysShownFor
		if showKeys {
			b.WriteString(statusDim + " · " + termio.Elapsed(secs))
		} else {
			b.WriteString(statusDim + " is in your shell · " + termio.Elapsed(secs))
			if time.Since(st.lastGuest) < 1500*time.Millisecond && st.guestInput {
				b.WriteString(" · " + statusOn + peer + " is typing…" + statusDim)
			}
		}
		if !st.guestInput {
			b.WriteString(" · " + statusOn + "Hands off" + statusDim)
		}
		if rec != nil {
			b.WriteString(" · " + statusRec + " ● recording " + statusDim)
		}
		if st.notice != "" && time.Now().Before(st.noticeTill) {
			b.WriteString(" · " + statusOn + st.notice + statusDim)
		}
		if showKeys {
			// The newest keys matter most: cut the oldest ones to fit.
			b.WriteString(" · typed ")
			room := status.Width() - ansi.StringWidth(b.String()) - 1
			if w := ansi.StringWidth(keys); room > 0 && w > room {
				keys = ansi.TruncateLeft(keys, w-room+1, "…")
			}
			b.WriteString(statusOn + keys + statusDim + " ")
		} else {
			b.WriteString(" · Ctrl-] then q to end, h for Hands off ")
		}
		status.Draw(b.String())
	}
	flash := func(msg string) {
		st.mu.Lock()
		st.notice, st.noticeTill = msg, time.Now().Add(4*time.Second)
		st.mu.Unlock()
		draw()
	}
	draw()

	// PTY output: local terminal, relay, recording.
	shellDone := make(chan struct{})
	go func() {
		defer close(shellDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				b := buf[:n]
				_, _ = t.Write(b)
				if rec != nil {
					rec.Cast.Output(b)
				}
				_ = h.Conn.WriteData(ctx, append([]byte(nil), b...))
			}
			if err != nil {
				return
			}
		}
	}()

	input, stopInput := readInput(t)
	defer stopInput()
	esc := &termio.Escaper{Commands: "qh"}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	end := func(reason string) Result {
		stopInput()
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGHUP)
			waited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
		status.Teardown()
		t.Restore()
		res.Reason = reason
		res.Duration = time.Since(started)
		if rec != nil {
			if err := rec.Finish(reason); err != nil && res.Err == nil {
				res.Err = fmt.Errorf("the recording may be incomplete: %w", err)
			}
		}
		return res
	}
	hostEnds := func(reason string) Result {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		why := protocol.EndHostEnded
		if reason == ReasonShellExited {
			why = protocol.EndShellExited
		}
		_, _ = h.Conn.Request(rctx, protocol.TypeSessionEnd, protocol.SessionEnd{SessionID: h.Start.SessionID, Reason: why})
		return end(reason)
	}

	for {
		select {
		case <-ctx.Done():
			return hostEnds(ReasonYouEnded)
		case <-h.Conn.Done():
			return end(ReasonRelayLost)
		case <-shellDone:
			return hostEnds(ReasonShellExited)
		case <-ticker.C:
			draw()
		case why := <-h.End:
			return hostEnds(why)
		case <-h.Resize:
			cols, rows = t.Size()
			ptyRows = status.Resize(cols, rows)
			_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(ptyRows)})
			_ = h.Conn.Send(ctx, protocol.TypeSessionResize, protocol.SessionResize{Cols: cols, Rows: ptyRows})
			if rec != nil {
				rec.Cast.Resize(cols, ptyRows)
			}
			draw()
		case b, ok := <-input:
			if !ok {
				input = nil // keyboard closed; the guest can still type
				continue
			}
			for _, c := range esc.Feed(b) {
				switch c.Cmd {
				case 'q':
					return hostEnds(ReasonYouEnded)
				case 'h':
					st.mu.Lock()
					st.guestInput = !st.guestInput
					on := st.guestInput
					st.mu.Unlock()
					_ = h.Conn.Send(ctx, protocol.TypeSessionMode, protocol.SessionMode{GuestInput: on})
					if on {
						flash(peer + " can type again")
					} else {
						flash("Hands off: " + peer + " can only watch")
					}
				case 0:
					_, _ = ptmx.Write(c.Data)
					rec.Input("host", c.Data)
				}
			}
		case b := <-h.Data:
			st.mu.Lock()
			allowed := st.guestInput
			redraw := false
			if allowed {
				st.lastGuest = time.Now()
				st.ribbon.Add(st.keys.Feed(b))
				redraw = time.Since(st.lastDraw) > 50*time.Millisecond
			}
			st.mu.Unlock()
			if allowed { // the relay enforces this too
				_, _ = ptmx.Write(b)
				rec.Input("guest", b)
			}
			if redraw {
				draw()
			}
		case ev := <-h.Events:
			switch ev.Type {
			case protocol.TypeSessionEnd:
				var se protocol.SessionEnd
				_ = ev.Decode(&se)
				if se.SessionID == "" || se.SessionID == h.Start.SessionID {
					return end(se.Reason)
				}
			case protocol.TypeDoorForceNew:
				res.Pending = append(res.Pending, ev)
				flash("Too many wrong codes on your door: you'll get a new code when this ends")
			default:
				res.Pending = append(res.Pending, ev)
			}
		}
	}
}

// ResizeSignals returns a channel of SIGWINCH and a stop function.
func ResizeSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}

// ShellCommand returns the argv for the PTY: config shell.command, tmux, or
// the login shell.
func ShellCommand(configured string, tmux bool) ([]string, error) {
	if tmux {
		if _, err := exec.LookPath("tmux"); err != nil {
			return nil, errors.New("--tmux needs tmux, and it isn't installed. Try brew install tmux")
		}
		return []string{"tmux", "new-session", "-A", "-s", "knock"}, nil
	}
	if configured != "" {
		f := strings.Fields(configured)
		return f, nil
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/zsh"
	}
	return []string{sh, "-l"}, nil
}
