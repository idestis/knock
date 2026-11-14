package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Recent doors, and the code hashes kept for them, belong to this relay.
	store, err := state.ForRelay(cfg.Relay)
	if err != nil {
		return err
	}
	recent, err := store.LoadRecent()
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
	if inRecent && termio.CleanText(item.OwnerName) != "" {
		label = "Knocking on " + termio.CleanText(item.OwnerName) + "'s door (" + doorID + ")…"
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
		return explainKnockError(err, doorID, code, store, &recent, item)
	}

	st := termio.CleanStart(res.start)
	owner := st.Peer
	row := state.RecentItem{
		DoorID: doorID, OwnerName: owner.Name, OwnerEmail: owner.Email, Provider: owner.Provider, Verified: owner.Verified,
		Device: st.PeerDevice, CodeVersion: st.CodeVersion, CodeHash: res.hash, LastSessionAt: state.Now(),
	}
	warning := ""
	if inRecent && ownerChanged(*item, owner) {
		// Keep the owner you knew pinned, so this warning comes back every
		// time until you forget the door on purpose.
		row.OwnerName, row.OwnerEmail, row.Provider, row.Verified = item.OwnerName, item.OwnerEmail, item.Provider, item.Verified
		was, now := describeOwner(item.OwnerName, item.OwnerEmail, item.Provider, item.Verified), describeOwner(owner.Name, owner.Email, owner.Provider, owner.Verified)
		ui.Warning("This door's owner changed: was %s, now %s. If you didn't expect that, leave with Ctrl-] then q.", was, now)
		ui.Note("If it's expected, run knock recent rm %s and knock again with a new code.", doorID)
		warning = "Owner changed: was " + was + ", now " + now
	}
	recent.Upsert(row)
	if err := store.SaveRecent(recent); err != nil {
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
		Resize: resize, Warning: warning,
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
	_ = store.SaveRecent(recent)
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

// ownerChanged reports whether the person behind a door you've been in is
// someone else now. recent.json keeps no owner id, so a verified owner is
// matched on provider and email (or name, for GitHub without an email). An
// owner that was verified and now isn't counts as changed; for an owner who
// never signed in, the name they typed is all there is to compare.
func ownerChanged(prev state.RecentItem, now protocol.Identity) bool {
	if prev.OwnerName == "" && prev.OwnerEmail == "" && prev.Provider == "" {
		return false // nothing pinned yet
	}
	if !prev.Verified {
		return termio.CleanText(prev.OwnerName) != now.Name
	}
	if !now.Verified || prev.Provider != now.Provider {
		return true
	}
	if prev.OwnerEmail != "" || now.Email != "" {
		return !strings.EqualFold(termio.CleanText(prev.OwnerEmail), now.Email)
	}
	return termio.CleanText(prev.OwnerName) != now.Name
}

// describeOwner is "Maya (maya@example.com, google)" or "Eve (not verified)".
func describeOwner(name, email, provider string, verified bool) string {
	name = termio.CleanText(name)
	if name == "" {
		name = "someone"
	}
	if !verified {
		return name + " (not verified)"
	}
	detail := termio.CleanText(provider)
	if e := termio.CleanText(email); e != "" {
		detail = e + ", " + detail
	}
	if detail == "" {
		return name
	}
	return name + " (" + detail + ")"
}

func explainKnockError(err error, doorID, code string, store state.Store, recent *state.Recent, item *state.RecentItem) error {
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
		owner = termio.CleanText(item.OwnerName)
	}
	switch perr.Code {
	case protocol.ErrCodeMismatch:
		if code == "" && item != nil {
			now := state.Now()
			if item.MismatchSince == nil {
				item.MismatchSince = &now
			}
			_ = store.SaveRecent(*recent)
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
