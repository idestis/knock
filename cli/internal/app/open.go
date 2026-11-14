package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/idestis/knock/cli/internal/auth"
	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/session"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

// host is the state of "knock open" between sessions.
type host struct {
	cfg        config.Config
	door       state.Door
	doorMod    int64
	c          *client.Client
	argv       []string
	guestInput bool

	queue     []protocol.KnockIncoming
	prompting *prompt
}

type prompt struct {
	knock  protocol.KnockIncoming
	cancel context.CancelFunc
	result chan ui.Answer
	gone   string // why the knock went away, if it did
}

func runOpen(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tmux := fs.Bool("tmux", false, "share a tmux session named knock")
	viewOnly := fs.Bool("view-only", false, "start with Hands off")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: knock open [--tmux] [--view-only]")
	}
	if !termio.Std().IsTTY() {
		return errors.New("knock open needs an interactive terminal")
	}
	argv, err := session.ShellCommand(cfg.ShellCommand, *tmux)
	if err != nil {
		return err
	}
	door, err := state.LoadOrCreateDoor()
	if err != nil {
		return fmt.Errorf("couldn't set up your door: %w", err)
	}
	h := &host{cfg: cfg, door: door, doorMod: state.DoorModTime(), argv: argv,
		guestInput: !*viewOnly && cfg.GuestInput != "view-only"}

	if err := h.connect(ctx); err != nil {
		return err
	}
	defer func() { h.c.Close() }()
	ui.Say("%s", whoLine(h.c.Welcome.User))
	h.showCard()
	return h.loop(ctx)
}

func (h *host) connect(ctx context.Context) error {
	c, err := connect(ctx, h.cfg)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = c.RequestInto(rctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: h.door.DoorID, DoorKey: h.door.DoorKey,
		Device: deviceName(), Code: h.door.Record()}, nil)
	if err != nil {
		c.Close()
		var perr *protocol.Error
		if errors.As(err, &perr) && perr.Code == protocol.ErrDoorTaken {
			return errors.New("Your door id belongs to someone else on this relay. Delete ~/.knock/door.json to get a new one")
		}
		return fmt.Errorf("couldn't open your door: %w", relayError(err))
	}
	h.c = c
	return nil
}

func (h *host) showCard() {
	rec := ""
	if m := h.cfg.SaveMode(protocol.RoleHost); m != config.SaveOff {
		rec = m
	}
	ui.Print(ui.DoorCard(h.c.Welcome.User.Name, h.door.ShareString(), rec))
	h.waiting()
}

func (h *host) waiting() {
	mode := ""
	if !h.guestInput {
		mode = " Guests start in Hands off (view-only)."
	}
	ui.Note("Waiting for a knock. Ctrl-C closes your door.%s", mode)
}

func (h *host) loop(ctx context.Context) error {
	watch := time.NewTicker(2 * time.Second)
	defer watch.Stop()
	var promptResult chan ui.Answer
	for {
		if h.prompting == nil {
			promptResult = nil
			h.nextPrompt(ctx)
		}
		if h.prompting != nil {
			promptResult = h.prompting.result
		}
		select {
		case <-ctx.Done():
			h.cancelPrompt("")
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = h.c.Request(cctx, protocol.TypeDoorClose, nil)
			cancel()
			ui.Say("Your door is closed.")
			return nil
		case <-h.c.Done():
			if err := h.reconnect(ctx, "Lost the relay."); err != nil {
				return err
			}
		case <-h.c.Data:
			// stray bytes after a session ended
		case ans := <-promptResult:
			p := h.prompting
			h.prompting = nil
			if err := h.answer(ctx, p, ans); err != nil {
				return err
			}
		case <-watch.C:
			h.checkDoorFile(ctx)
		case ev := <-h.c.Events:
			if err := h.handle(ctx, ev); err != nil {
				return err
			}
		}
	}
}

func (h *host) nextPrompt(ctx context.Context) {
	if len(h.queue) == 0 {
		return
	}
	k := h.queue[0]
	h.queue = h.queue[1:]
	pctx, cancel := context.WithCancel(ctx)
	p := &prompt{knock: k, cancel: cancel, result: make(chan ui.Answer, 1)}
	h.prompting = p
	timeout := time.Duration(k.ExpiresIn)*time.Second - time.Second
	if timeout <= 0 || timeout > time.Duration(h.cfg.KnockTimeout)*time.Second {
		timeout = time.Duration(h.cfg.KnockTimeout) * time.Second
	}
	// Everything here comes from the relay's verified token, or is marked
	// not verified; nothing the knocker typed passes as verified.
	who := ui.Knocker{Name: k.From.Name, Detail: auth.Describe(k.From), Provider: k.From.Provider, Verified: k.From.Verified, Device: k.Device}
	go func() { p.result <- ui.AskLetIn(pctx, who, timeout) }()
}

func (h *host) cancelPrompt(why string) {
	if h.prompting != nil {
		h.prompting.gone = why
		h.prompting.cancel()
	}
}

func (h *host) answer(ctx context.Context, p *prompt, ans ui.Answer) error {
	p.cancel()
	name := p.knock.From.Name
	if ans == ui.AnswerGone {
		if p.gone != "" {
			ui.Note("%s", p.gone)
		}
		return nil
	}
	allow := ans == ui.AnswerYes
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := h.c.RequestInto(rctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: p.knock.KnockID, Allow: allow}, nil)
	if err != nil {
		var perr *protocol.Error
		if errors.As(err, &perr) && perr.Code == protocol.ErrNotFound {
			ui.Note("%s stopped knocking before you answered.", name)
			return nil
		}
		ui.Warning("%s", sentence(relayError(err).Error()))
		return nil
	}
	if !allow {
		h.waiting()
		return nil
	}
	// The relay sends session.start right after the ok.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.c.Events:
			if ev.Type == protocol.TypeSessionStart {
				return h.runSession(ctx, ev)
			}
			if err := h.handle(ctx, ev); err != nil {
				return err
			}
		case <-h.c.Data:
		case <-h.c.Done():
			return h.reconnect(ctx, "Lost the relay before the session started.")
		case <-deadline:
			ui.Warning("The session didn't start. Ask %s to knock again.", name)
			return nil
		}
	}
}

func (h *host) handle(ctx context.Context, ev client.Event) error {
	switch ev.Type {
	case protocol.TypeKnockIncoming:
		var k protocol.KnockIncoming
		if ev.Decode(&k) == nil {
			h.queue = append(h.queue, k)
		}
	case protocol.TypeKnockCancelled:
		var r protocol.KnockRef
		if ev.Decode(&r) != nil {
			return nil
		}
		for i, k := range h.queue {
			if k.KnockID == r.KnockID {
				h.queue = append(h.queue[:i], h.queue[i+1:]...)
				break
			}
		}
		if h.prompting != nil && h.prompting.knock.KnockID == r.KnockID {
			name := h.prompting.knock.From.Name
			msg := name + " stopped knocking."
			switch r.Reason {
			case "timeout":
				msg = "No answer in time, so " + name + " wasn't let in."
			case "code_changed":
				msg = name + "'s knock was cancelled because your code changed."
			case "busy":
				msg = name + "'s knock was cancelled because you're in a session."
			}
			h.cancelPrompt(msg)
		}
	case protocol.TypeSessionStart:
		// Only expected right after an answer; handled there.
	case protocol.TypeDoorForceNew:
		var f protocol.DoorForceNewCode
		_ = ev.Decode(&f)
		h.cancelPrompt("")
		return h.forceNewCode(ctx, f.Reason)
	case protocol.TypeDoorClose:
		var se protocol.SessionEnd
		_ = ev.Decode(&se)
		h.cancelPrompt("")
		if se.Reason == "opened_elsewhere" {
			ui.Warning("Your door was opened somewhere else (maybe Knock.app on this Mac), so it's closed here.")
		} else {
			ui.Warning("The relay closed your door.")
		}
		return exitError{1}
	case protocol.TypeRelayDraining:
		h.c.Close()
		return h.reconnect(ctx, "The relay is restarting.")
	}
	return nil
}

func (h *host) runSession(ctx context.Context, ev client.Event) error {
	var st protocol.SessionStart
	if err := ev.Decode(&st); err != nil {
		return err
	}
	peer := st.Peer.Name
	if !st.Peer.Verified {
		peer += " (not verified)"
	}
	ui.Say("%s is in. This shell is now shared.", ui.Bold.Render(peer))
	resize, stopResize := session.ResizeSignals()
	defer stopResize()
	hs := &session.Host{
		Common: session.Common{Conn: h.c, Events: h.c.Events, Data: h.c.Data, Term: termio.Std(), Start: st,
			SaveMode: h.cfg.SaveMode(protocol.RoleHost), SessionsDir: h.cfg.SessionsDir, Device: deviceName(), Version: Version},
		Command: h.argv, GuestInput: h.guestInput, Resize: resize,
	}
	res := hs.Run(ctx)
	ui.Say("%s %s", session.Explain(res.Reason, protocol.RoleHost, st.Peer), ui.Dim.Render("("+res.Duration.Round(time.Second).String()+")"))
	if res.SavedTo != "" {
		ui.Note("Saved the recording to %s", res.SavedTo)
	}
	if res.Err != nil {
		ui.Warning("%s", sentence(res.Err.Error()))
	}
	for _, e := range res.Pending {
		if err := h.handle(ctx, e); err != nil {
			return err
		}
	}
	if res.Reason == session.ReasonRelayLost {
		return h.reconnect(ctx, "Lost the relay.")
	}
	if ctx.Err() != nil {
		return nil
	}
	h.waiting()
	return nil
}

// forceNewCode rotates the code because the relay saw too many wrong ones.
func (h *host) forceNewCode(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "Someone tried a lot of wrong codes on your door."
	}
	h.door.Rotate()
	if err := state.SaveDoor(h.door); err != nil {
		return fmt.Errorf("couldn't save your new code: %w", err)
	}
	h.doorMod = state.DoorModTime()
	if err := h.pushCode(ctx); err != nil {
		return err
	}
	ui.Warning("%s To be safe, Knock made you a new code. Only people you give it to can knock.", sentence(reason))
	h.showCard()
	return nil
}

func (h *host) pushCode(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.c.RequestInto(rctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: h.door.Record()}, nil); err != nil {
		return fmt.Errorf("couldn't send your new code to the relay: %w", relayError(err))
	}
	return nil
}

// checkDoorFile picks up "knock new-code" run in another terminal.
func (h *host) checkDoorFile(ctx context.Context) {
	mod := state.DoorModTime()
	if mod == h.doorMod {
		return
	}
	h.doorMod = mod
	d, ok, err := state.LoadDoor()
	if err != nil || !ok || d.DoorID != h.door.DoorID || d.CodeVersion == h.door.CodeVersion {
		return
	}
	h.door = d
	if err := h.pushCode(ctx); err != nil {
		ui.Warning("%s", sentence(err.Error()))
		return
	}
	if h.prompting == nil {
		ui.Say("Your code changed. Anyone with the old code is cut off.")
		h.showCard()
	}
}

// reconnect retries the relay with backoff for about two minutes.
func (h *host) reconnect(ctx context.Context, why string) error {
	h.cancelPrompt("")
	h.queue = nil
	ui.Warning("%s Reconnecting…", why)
	wait := time.Second
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if err := h.connect(ctx); err == nil {
			ui.Say("Back online. Your door is open again.")
			h.waiting()
			return nil
		}
		wait = min(wait*2, 15*time.Second)
	}
	return errors.New("Couldn't get back to the relay. Run knock open to try again")
}
