package session

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/protocol"
)

// Guest runs the guest side: keyboard to the relay, relay to the screen.
//
// The bottom row is a status line (like the host's). Notices go there and
// never into the mirrored shell output, so the host's prompt and cursor stay
// exactly where the host's shell put them.
type Guest struct {
	Common
	Resize <-chan os.Signal
}

// Run returns when the session ends.
func (g *Guest) Run(ctx context.Context) Result {
	t := g.Term
	if err := t.MakeRaw(); err != nil {
		return Result{Reason: ReasonYouEnded, Err: err}
	}
	defer t.Restore()

	status := termio.NewStatusLine(t)
	cols, rows := t.Size()
	status.Setup(cols, rows)

	started := time.Now()
	res := Result{}
	peer := peerFirstName(g.Start.Peer)
	hostCols, hostRows := g.Start.Cols, g.Start.Rows
	canType := g.Start.GuestInput

	var notice string
	var noticeTill time.Time
	tooSmall := func() bool {
		if hostCols == 0 || hostRows == 0 {
			return false
		}
		c, r := t.Size()
		return c < hostCols || r-1 < hostRows // one row is the status line
	}
	draw := func() {
		var b strings.Builder
		b.WriteString(statusOn + " " + g.Start.Peer.Name + "'s shell")
		if !g.Start.Peer.Verified {
			b.WriteString(statusDim + " (not verified)")
		}
		b.WriteString(statusDim + " · " + termio.Elapsed(int(time.Since(started)/time.Second)))
		if !canType {
			b.WriteString(" · " + statusOn + "Hands off: watching" + statusDim)
		}
		if notice != "" && time.Now().Before(noticeTill) {
			b.WriteString(" · " + statusOn + notice + statusDim)
		} else if tooSmall() {
			c, r := t.Size()
			fmt.Fprintf(&b, " · %s's terminal is %d×%d, yours shows %d×%d. Make the window bigger", peer, hostCols, hostRows, c, r-1)
		}
		b.WriteString(" · Ctrl-] then q to leave ")
		status.Draw(b.String())
	}
	flash := func(msg string) {
		notice, noticeTill = msg, time.Now().Add(5*time.Second)
		draw()
	}
	draw()

	rec, recErr := g.startRecording(protocol.RoleGuest, hostCols, hostRows)
	if rec != nil {
		res.SavedTo = rec.Dir
	}
	if recErr != nil {
		res.Err = fmt.Errorf("couldn't start the recording: %w", recErr)
	}

	input, stopInput := readInput(t)
	defer stopInput()
	esc := &termio.Escaper{Commands: "q"}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	end := func(reason string) Result {
		stopInput()
		status.Teardown()
		t.Restore()
		t.Printf("\x1b[0m\r\n")
		res.Reason = reason
		res.Duration = time.Since(started)
		if rec != nil {
			if err := rec.Finish(reason); err != nil && res.Err == nil {
				res.Err = fmt.Errorf("the recording may be incomplete: %w", err)
			}
		}
		return res
	}
	leave := func() Result {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, _ = g.Conn.Request(rctx, protocol.TypeSessionEnd, protocol.SessionEnd{SessionID: g.Start.SessionID})
		return end(ReasonYouEnded)
	}

	for {
		select {
		case <-ctx.Done():
			return leave()
		case <-g.Conn.Done():
			return end(ReasonRelayLost)
		case <-tick.C:
			draw()
		case <-g.Resize:
			cols, rows = t.Size()
			status.Resize(cols, rows)
			draw()
		case b, ok := <-input:
			if !ok {
				input = nil
				continue
			}
			for _, c := range esc.Feed(b) {
				if c.Cmd == 'q' {
					return leave()
				}
				if !canType {
					flash(peer + " has Hands off on, so your typing isn't sent")
					continue
				}
				_ = g.Conn.WriteData(ctx, c.Data)
				rec.Input("guest", c.Data)
			}
		case b := <-g.Data:
			_, _ = t.Write(b)
			if rec != nil {
				rec.Cast.Output(b)
			}
		case ev := <-g.Events:
			switch ev.Type {
			case protocol.TypeSessionResize:
				var sr protocol.SessionResize
				if ev.Decode(&sr) == nil {
					hostCols, hostRows = sr.Cols, sr.Rows
					if rec != nil {
						rec.Cast.Resize(sr.Cols, sr.Rows)
					}
					draw()
				}
			case protocol.TypeSessionMode:
				var sm protocol.SessionMode
				if ev.Decode(&sm) == nil && sm.GuestInput != canType {
					canType = sm.GuestInput
					if canType {
						flash(peer + " gave you the keyboard back")
					} else {
						flash(peer + " turned on Hands off. You can watch, not type")
					}
				}
			case protocol.TypeSessionEnd:
				var se protocol.SessionEnd
				_ = ev.Decode(&se)
				if se.SessionID == "" || se.SessionID == g.Start.SessionID {
					return end(se.Reason)
				}
			default:
				res.Pending = append(res.Pending, ev)
			}
		}
	}
}
