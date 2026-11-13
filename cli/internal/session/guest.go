package session

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/protocol"
)

// Guest runs the guest side: keyboard to the relay, relay to the screen.
type Guest struct {
	Common
	Resize <-chan os.Signal
}

const (
	noteStart = "\r\n\x1b[0;2m[knock] "
	noteEnd   = "\x1b[0m\r\n"
)

// Run returns when the session ends.
func (g *Guest) Run(ctx context.Context) Result {
	t := g.Term
	if err := t.MakeRaw(); err != nil {
		return Result{Reason: ReasonYouEnded, Err: err}
	}
	defer t.Restore()

	started := time.Now()
	res := Result{}
	peer := peerFirstName(g.Start.Peer)
	hostCols, hostRows := g.Start.Cols, g.Start.Rows
	tooSmall := false
	checkSize := func() {
		if hostCols == 0 || hostRows == 0 {
			return
		}
		c, r := t.Size()
		small := c < hostCols || r < hostRows
		if small && !tooSmall {
			t.Printf(noteStart+"%s's terminal is %d×%d and yours is %d×%d. Make your window bigger to see everything."+noteEnd,
				peer, hostCols, hostRows, c, r)
		}
		tooSmall = small
	}

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
	canType := g.Start.GuestInput
	warnedHandsOff := false

	end := func(reason string) Result {
		stopInput()
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
		case <-g.Resize:
			checkSize()
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
					if !warnedHandsOff {
						t.Printf("%s", noteStart+peer+" has Hands off on, so your typing isn't sent. Ctrl-] then q to leave."+noteEnd)
						warnedHandsOff = true
					}
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
					checkSize()
				}
			case protocol.TypeSessionMode:
				var sm protocol.SessionMode
				if ev.Decode(&sm) == nil && sm.GuestInput != canType {
					canType, warnedHandsOff = sm.GuestInput, false
					if canType {
						t.Printf("%s", noteStart+peer+" gave you the keyboard back. You can type again."+noteEnd)
					} else {
						t.Printf("%s", noteStart+peer+" turned on Hands off. You can watch but not type."+noteEnd)
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
