package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/session"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

type knocked struct {
	start protocol.SessionStart
	hash  []byte
}

func runJoin(ctx context.Context, cfg config.Config, args []string) error {
	doorID, code, err := state.ParseShare(args)
	if err != nil {
		return err
	}
	recent, err := state.LoadRecent()
	if err != nil {
		return err
	}
	item, inRecent := recent.Find(doorID)
	if code == "" && !inRecent {
		return fmt.Errorf("You haven't been in %s before, so you need its code: knock %s <code>", doorID, doorID)
	}
	if !termio.Std().IsTTY() {
		return errors.New("Knocking needs an interactive terminal")
	}

	c, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	ui.Say("%s", whoLine(c.Welcome.User))

	label := "Knocking on " + doorID + "…"
	if inRecent && item.OwnerName != "" {
		label = "Knocking on " + item.OwnerName + "'s door (" + doorID + ")…"
	}
	label += ui.Dim.Render("  Ctrl-C to stop")

	res, err := ui.Spin(ctx, label, func(ctx context.Context) (knocked, error) {
		var hash []byte
		if code != "" {
			h, err := c.HashForDoor(ctx, doorID, code)
			if err != nil {
				return knocked{}, err
			}
			hash = h
		} else {
			hash = item.CodeHash
		}
		st, err := knock(ctx, c, doorID, hash)
		return knocked{start: st, hash: hash}, err
	})
	if err != nil {
		return explainKnockError(err, doorID, code, &recent, item)
	}

	st := res.start
	owner := st.Peer
	recent.Upsert(state.RecentItem{
		DoorID: doorID, OwnerName: owner.Name, OwnerEmail: owner.Email, Provider: owner.Provider, Verified: owner.Verified,
		Device: st.PeerDevice, CodeVersion: st.CodeVersion, CodeHash: res.hash, LastSessionAt: state.Now(),
	})
	if err := state.SaveRecent(recent); err != nil {
		ui.Warning("Couldn't save %s to your recent doors: %v", doorID, err)
	}

	name := ui.Bold.Render(owner.Name)
	if !owner.Verified {
		name += ui.Warn.Render(" (not verified)")
	}
	ui.Say("%s let you in. %s", name, ui.Dim.Render("Ctrl-] then q to leave."))
	if !st.GuestInput {
		ui.Note("Hands off is on: you can watch but not type until %s turns it off.", owner.Name)
	}

	resize, stopResize := session.ResizeSignals()
	defer stopResize()
	g := &session.Guest{
		Common: session.Common{Conn: c, Events: c.Events, Data: c.Data, Term: termio.Std(), Start: st,
			SaveMode: cfg.SaveMode(protocol.RoleGuest), SessionsDir: cfg.SessionsDir, Device: deviceName(), Version: Version},
		Resize: resize,
	}
	out := g.Run(ctx)
	ui.Say("%s %s", session.Explain(out.Reason, protocol.RoleGuest, owner), ui.Dim.Render("("+out.Duration.Round(time.Second).String()+")"))
	if out.SavedTo != "" {
		ui.Note("Saved the recording to %s", out.SavedTo)
	}
	if out.Err != nil {
		ui.Warning("%s", sentence(out.Err.Error()))
	}

	if out.Reason == "code_changed" {
		if it, ok := recent.Find(doorID); ok {
			now := state.Now()
			it.MismatchSince = &now
		}
	} else if it, ok := recent.Find(doorID); ok {
		it.LastSessionAt = state.Now()
	}
	_ = state.SaveRecent(recent)
	return nil
}

// knock sends knock.request and waits for session.start or a refusal.
func knock(ctx context.Context, c *client.Client, doorID string, hash []byte) (protocol.SessionStart, error) {
	var st protocol.SessionStart
	var pending protocol.KnockPending
	if err := c.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: doorID, CodeHash: hash}, &pending); err != nil {
		return st, err
	}
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = c.Request(cctx, protocol.TypeKnockCancel, nil)
			cancel()
			return st, ctx.Err()
		case <-c.Done():
			return st, errors.New("Lost the connection to the relay while knocking")
		case ev := <-c.Events:
			switch ev.Type {
			case protocol.TypeSessionStart:
				err := ev.Decode(&st)
				return st, err
			case protocol.TypeError:
				var perr protocol.Error
				if ev.Decode(&perr) == nil {
					return st, &perr
				}
			case protocol.TypeDoorCodeChanged:
				var r protocol.DoorRef
				if ev.Decode(&r) == nil && r.DoorID == doorID {
					return st, &protocol.Error{Code: protocol.ErrCodeMismatch, Message: "The code changed. Ask for a new one."}
				}
			case protocol.TypeRelayDraining:
				return st, errors.New("The relay is restarting. Knock again in a moment")
			}
		}
	}
}

func explainKnockError(err error, doorID, code string, recent *state.Recent, item *state.RecentItem) error {
	if errors.Is(err, ui.ErrCancelled) || errors.Is(err, context.Canceled) {
		ui.Note("You stopped knocking.")
		return exitError{130}
	}
	var perr *protocol.Error
	if !errors.As(err, &perr) {
		return err
	}
	owner := "they"
	if item != nil && item.OwnerName != "" {
		owner = item.OwnerName
	}
	switch perr.Code {
	case protocol.ErrCodeMismatch:
		if code == "" && item != nil {
			now := state.Now()
			if item.MismatchSince == nil {
				item.MismatchSince = &now
			}
			_ = state.SaveRecent(*recent)
			return fmt.Errorf("The code for %s changed. Ask %s for the new one, then run knock %s <code>", doorID, owner, doorID)
		}
		return errors.New("That code doesn't work. Ask for a new one")
	case protocol.ErrDoorOffline:
		if item != nil {
			return fmt.Errorf("%s isn't open to knocks right now", owner)
		}
		return errors.New("They're not open to knocks right now")
	case protocol.ErrDenied:
		return errors.New("Not now. They didn't let you in")
	case protocol.ErrTimeout:
		return errors.New("No answer. Try again in a bit")
	case protocol.ErrDoorBusy:
		return errors.New("They're in a session with someone else. Try again later")
	case protocol.ErrRateLimited:
		return errors.New("Too many wrong codes. Wait ten minutes and try again")
	}
	if perr.Message != "" {
		return errors.New(perr.Message)
	}
	return fmt.Errorf("The relay said %s", perr.Code)
}
