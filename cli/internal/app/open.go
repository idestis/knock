package app

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
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
	store      state.Store // this relay's folder: the door is only ever sent here
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
	store, err := state.ForRelay(cfg.Relay)
	if err != nil {
		return err
	}
	door, err := store.LoadOrCreateDoor()
	if err != nil {
		return fmt.Errorf("couldn't set up your door: %w", err)
	}
	h := &host{cfg: cfg, store: store, door: door, doorMod: store.DoorModTime(), argv: argv,
		guestInput: !*viewOnly && cfg.GuestInput != "view-only"}

	// An old code is replaced before the door opens, so the relay only
	// ever hears the new one.
	aged := false
	if h.door.CodeExpired(cfg.CodeMaxAgeDays, time.Now()) {
		h.door.Rotate()
		if err := h.store.SaveDoor(h.door); err != nil {
			return fmt.Errorf("couldn't save your new code: %w", err)
		}
		h.doorMod, aged = h.store.DoorModTime(), true
	}

	if err := h.connect(ctx); err != nil {
		return err
	}
	defer func() { h.c.Close() }()
	ui.Say("%s", whoLine(h.c.Welcome.User))
	if aged {
		h.sayAgedOut()
	}
	h.showCard()
	return h.loop(ctx)
}

// errCodeStale: the relay holds a newer code for this door than door.json,
// even after reading door.json again and, when the relay said which version
// it holds, making a fresh code above it. Retrying won't change that.
var errCodeStale = errors.New("Your door on this relay has a newer code than this computer knows about, so Knock stopped. Update the relay, or delete this relay's door.json to start a new door")

func isCodeStale(err error) bool {
	var perr *protocol.Error
	return errors.As(err, &perr) && perr.Code == protocol.ErrCodeStale
}

// staleVersion is the code version a code_stale error says the relay
// holds. Older relays don't send one.
func staleVersion(err error) (int64, bool) {
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodeStale || perr.CodeVersion <= 0 {
		return 0, false
	}
	return int64(perr.CodeVersion), true
}

// sameDoor reports whether next is the same door as cur (same id and key).
func sameDoor(cur, next state.Door) bool {
	return next.DoorID == cur.DoorID && next.DoorKey == cur.DoorKey
}

// jumpCode makes a fresh code above both the relay's version and ours,
// the same way a rotation does, and saves it. It never goes back to an
// older code.
func jumpCode(store state.Store, d state.Door, relayVersion int64) (state.Door, error) {
	d.CodeVersion = max(d.CodeVersion, relayVersion)
	d.Rotate()
	if err := store.SaveDoor(d); err != nil {
		return d, fmt.Errorf("couldn't save your new code: %w", err)
	}
	return d, nil
}

// sayJumped tells the host their code was replaced because the relay
// held a newer one.
func sayJumped(d state.Door) {
	ui.Say("Your door had a newer code on the relay than this computer knew, so here's a new one: %s. Share it again.",
		ui.Brand.Render(d.ShareString()))
}

// openDoor sends door.open for d with its claim, and saves the claim the
// relay sends back. If the relay says the code is stale it reads door.json
// again (Knock.app may have just written a newer code) and tries once more.
// Still stale, and the relay said which version it holds: it makes a fresh
// code above that version, saves it, and tries one last time.
// It returns the door the relay accepted.
func openDoor(ctx context.Context, c *client.Client, store state.Store, d state.Door) (state.Door, error) {
	send := func(d state.Door) (protocol.DoorOpened, error) {
		var opened protocol.DoorOpened
		err := c.RequestInto(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: d.DoorID, DoorKey: d.DoorKey,
			Device: deviceName(), Code: d.Record(), Claim: d.Claim}, &opened)
		return opened, err
	}
	opened, err := send(d)
	if isCodeStale(err) {
		fresh, ok, lerr := store.LoadDoor()
		if lerr != nil || !ok || !sameDoor(d, fresh) {
			return d, errCodeStale
		}
		d = fresh
		opened, err = send(d)
	}
	jumped := false
	if isCodeStale(err) {
		v, ok := staleVersion(err)
		if !ok {
			return d, errCodeStale
		}
		if d, err = jumpCode(store, d, v); err != nil {
			return d, err
		}
		jumped = true
		opened, err = send(d)
		if isCodeStale(err) {
			return d, errCodeStale
		}
	}
	if err != nil {
		return d, err
	}
	if jumped {
		sayJumped(d)
	}
	if opened.Claim != "" && opened.Claim != d.Claim {
		d.Claim = opened.Claim
		if err := store.SaveClaim(d.DoorID, d.DoorKey, opened.Claim); err != nil {
			ui.Warning("Couldn't save your door's claim: %v", err)
		}
	}
	return d, nil
}

func (h *host) connect(ctx context.Context) error {
	c, err := connect(ctx, h.cfg)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d, err := openDoor(rctx, c, h.store, h.door)
	if err != nil {
		c.Close()
		if errors.Is(err, errCodeStale) {
			return err
		}
		var perr *protocol.Error
		if errors.As(err, &perr) && perr.Code == protocol.ErrDoorTaken {
			return fmt.Errorf("Your door id belongs to someone else on this relay. Delete %s to get a new one", h.store.DoorPath())
		}
		return fmt.Errorf("couldn't open your door: %w", relayError(err))
	}
	h.door, h.doorMod = d, h.store.DoorModTime()
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
	if h.cfg.GuestVerifiedOnly {
		mode += " Knocks from people who aren't signed in are turned away."
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
			if err := h.checkDoorFile(ctx); err != nil {
				return err
			}
			if err := h.checkCodeAge(ctx, time.Now()); err != nil {
				return err
			}
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
				var st protocol.SessionStart
				if err := ev.Decode(&st); err != nil {
					return err
				}
				if !sessionMatches(p.knock, h.door.DoorID, st) {
					// Only the person the host said yes to gets the shell.
					h.refuseSession(ctx, st)
					ui.Warning("The relay tried to start a session with someone you didn't let in, so Knock ended it.")
					h.waiting()
					return nil
				}
				return h.runSession(ctx, st)
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
		if ev.Decode(&k) != nil {
			return nil
		}
		// Names are whatever the relay or an anonymous knocker sent: strip
		// anything that could redraw the prompt before it's shown.
		k.From = termio.CleanIdentity(k.From)
		k.Device = termio.CleanText(k.Device)
		if turnAway(h.cfg, k) {
			h.decline(ctx, k)
			return nil
		}
		h.queue = append(h.queue, k)
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
		switch se.Reason {
		case "opened_elsewhere":
			ui.Warning("Your door was opened somewhere else (maybe Knock.app on this Mac), so it's closed here.")
		case "door_reclaimed":
			ui.Warning("The owner of this door id took it back on the relay, so it's closed here. If that wasn't you, delete this relay's door.json to make a new door.")
		default:
			ui.Warning("The relay closed your door.")
		}
		return exitError{1}
	case protocol.TypeRelayDraining:
		h.c.Close()
		return h.reconnect(ctx, "The relay is restarting.")
	}
	return nil
}

// turnAway reports whether guest.verified_only declines this knock before
// the host is asked. It only ever declines: letting someone in takes a "y".
func turnAway(cfg config.Config, k protocol.KnockIncoming) bool {
	return cfg.GuestVerifiedOnly && !k.From.Verified
}

// decline answers no for the host, in the background, and leaves a quiet note.
func (h *host) decline(ctx context.Context, k protocol.KnockIncoming) {
	c := h.c
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = c.RequestInto(rctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: k.KnockID, Allow: false}, nil)
	}()
	if h.prompting == nil {
		ui.Note("Turned away %s, who isn't signed in (guest.verified_only).", k.From.Name)
	}
}

// sessionMatches reports whether a session.start is for the knock the host
// just let in: same person, same door, host role.
func sessionMatches(k protocol.KnockIncoming, doorID string, st protocol.SessionStart) bool {
	return st.SessionID != "" && st.Role == protocol.RoleHost && st.Peer.ID != "" && st.Peer.ID == k.From.ID &&
		st.Peer.Verified == k.From.Verified && strings.EqualFold(st.DoorID, doorID)
}

// refuseSession ends a session the host didn't agree to.
func (h *host) refuseSession(ctx context.Context, st protocol.SessionStart) {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _ = h.c.Request(rctx, protocol.TypeSessionEnd, protocol.SessionEnd{SessionID: st.SessionID, Reason: protocol.EndHostEnded})
}

func (h *host) runSession(ctx context.Context, st protocol.SessionStart) error {
	st = termio.CleanStart(st)
	peer := st.Peer.Name
	if !st.Peer.Verified {
		peer += " (not verified)"
	}
	ui.Say("%s is in. This shell is now shared.", ui.Bold.Render(peer))
	resize, stopResize := session.ResizeSignals()
	defer stopResize()
	// Watch door.json while the shell is shared, so knock new-code in
	// another terminal ends this session too (rule 4).
	ended := make(chan string, 1)
	stopWatch := make(chan struct{})
	watched := make(chan doorSnapshot, 1)
	go func() { watched <- h.watchDoorDuringSession(ctx, stopWatch, ended) }()
	hs := &session.Host{
		Common: session.Common{Conn: h.c, Events: h.c.Events, Data: h.c.Data, Term: termio.Std(), Start: st,
			SaveMode: h.cfg.SaveMode(protocol.RoleHost), SessionsDir: h.cfg.SessionsDir, Device: deviceName(), Version: Version},
		Command: h.argv, GuestInput: h.guestInput, Resize: resize, End: ended,
	}
	res := hs.Run(ctx)
	close(stopWatch) // a code push already under way still finishes
	snap := <-watched
	codeChanged := doorChanged(h.door, snap.door)
	h.door, h.doorMod = snap.door, snap.mod
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
	if codeChanged {
		ui.Say("Your code changed. Anyone with the old code is cut off.")
		h.showCard()
		return nil
	}
	// A code that got too old during the session is replaced now that
	// it's over, never while someone is in.
	if h.door.CodeExpired(h.cfg.CodeMaxAgeDays, time.Now()) {
		return h.rotateAged(ctx)
	}
	h.waiting()
	return nil
}

// doorWatchEvery is how often knock open looks at door.json.
var doorWatchEvery = 2 * time.Second

type doorSnapshot struct {
	door state.Door
	mod  int64
}

// watchDoorDuringSession polls door.json until stop closes. When the code
// changes it sends the new code to the relay, which ends the session and
// cancels every knock, and asks the session to end here as well. It
// returns the door the relay now has; if the relay couldn't be told, that
// is still the old one, so the next check after the session retries.
func (h *host) watchDoorDuringSession(ctx context.Context, stop <-chan struct{}, end chan<- string) doorSnapshot {
	cur := doorSnapshot{door: h.door, mod: h.doorMod}
	t := time.NewTicker(doorWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return cur
		case <-ctx.Done():
			return cur
		case <-t.C:
		}
		mod := h.store.DoorModTime()
		if mod == cur.mod {
			continue
		}
		d, ok, err := h.store.LoadDoor()
		if err != nil || !ok {
			continue
		}
		if !doorChanged(cur.door, d) {
			cur.mod = mod
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = h.c.RequestInto(rctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: d.Record()}, nil)
		cancel()
		if err == nil {
			cur = doorSnapshot{door: d, mod: mod}
		}
		// The old code must not keep anyone in, whether or not the relay heard.
		select {
		case end <- "code_changed":
		default:
		}
		return cur
	}
}

// doorChanged reports whether next holds a different code for the same
// door than cur.
func doorChanged(cur, next state.Door) bool {
	return next.DoorID == cur.DoorID &&
		(next.CodeVersion != cur.CodeVersion || !bytes.Equal(next.CodeHash, cur.CodeHash) || !bytes.Equal(next.Salt, cur.Salt))
}

// forceNewCode rotates the code because the relay saw too many wrong ones.
func (h *host) forceNewCode(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "Someone tried a lot of wrong codes on your door."
	}
	h.door.Rotate()
	if err := h.store.SaveDoor(h.door); err != nil {
		return fmt.Errorf("couldn't save your new code: %w", err)
	}
	h.doorMod = h.store.DoorModTime()
	if err := h.pushCode(ctx); err != nil {
		return err
	}
	ui.Warning("%s To be safe, Knock made you a new code. Only people you give it to can knock.", sentence(reason))
	h.showCard()
	return nil
}

// checkCodeAge replaces the code if it's too old and the door is idle:
// no session (the loop doesn't tick during one) and nobody knocking.
func (h *host) checkCodeAge(ctx context.Context, now time.Time) error {
	if h.prompting != nil || len(h.queue) > 0 || !h.door.CodeExpired(h.cfg.CodeMaxAgeDays, now) {
		return nil
	}
	return h.rotateAged(ctx)
}

// rotateAged replaces a code older than door.code_max_age_days, the same
// way knock new-code does, and asks the host to share the new one. Callers
// make sure nobody is in a session or waiting at the door.
func (h *host) rotateAged(ctx context.Context) error {
	h.door.Rotate()
	if err := h.store.SaveDoor(h.door); err != nil {
		return fmt.Errorf("couldn't save your new code: %w", err)
	}
	h.doorMod = h.store.DoorModTime()
	if err := h.pushCode(ctx); err != nil {
		if errors.Is(err, errCodeStale) {
			return err
		}
		select {
		case <-h.c.Done():
			// Reconnecting opens the door with the new code.
		default:
			return err
		}
	}
	h.sayAgedOut()
	h.showCard()
	return nil
}

// sayAgedOut tells the host their code was replaced for its age.
func (h *host) sayAgedOut() {
	ui.Say("Your code was over %s old, so here's a new one: %s. Share it again.",
		codeAge(h.cfg.CodeMaxAgeDays), ui.Brand.Render(h.door.ShareString()))
}

// codeAge phrases door.code_max_age_days: "a day", "a week", "2 weeks", "10 days".
func codeAge(days int) string {
	switch {
	case days == 1:
		return "a day"
	case days == 7:
		return "a week"
	case days > 0 && days%7 == 0:
		return fmt.Sprintf("%d weeks", days/7)
	}
	return fmt.Sprintf("%d days", days)
}

// pushCode sends the door's code to the relay. If the relay says it's
// stale, door.json is read again and sent once more. Still stale, and the
// relay said which version it holds: a fresh code above that version is
// made, saved and sent one last time. Still stale after that is
// errCodeStale.
func (h *host) pushCode(ctx context.Context) error {
	send := func() error {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return h.c.RequestInto(rctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: h.door.Record()}, nil)
	}
	err := send()
	if isCodeStale(err) {
		fresh, ok, lerr := h.store.LoadDoor()
		if lerr != nil || !ok || !sameDoor(h.door, fresh) {
			return errCodeStale
		}
		h.door, h.doorMod = fresh, h.store.DoorModTime()
		err = send()
	}
	if isCodeStale(err) {
		v, ok := staleVersion(err)
		if !ok {
			return errCodeStale
		}
		d, jerr := jumpCode(h.store, h.door, v)
		if jerr != nil {
			return jerr
		}
		h.door, h.doorMod = d, h.store.DoorModTime()
		if err = send(); err == nil {
			sayJumped(h.door)
			return nil
		}
		if isCodeStale(err) {
			return errCodeStale
		}
	}
	if err != nil {
		return fmt.Errorf("couldn't send your new code to the relay: %w", relayError(err))
	}
	return nil
}

// checkDoorFile picks up "knock new-code" run in another terminal.
func (h *host) checkDoorFile(ctx context.Context) error {
	mod := h.store.DoorModTime()
	if mod == h.doorMod {
		return nil
	}
	h.doorMod = mod
	d, ok, err := h.store.LoadDoor()
	if err != nil || !ok || !doorChanged(h.door, d) {
		return nil
	}
	h.door = d
	if err := h.pushCode(ctx); err != nil {
		if errors.Is(err, errCodeStale) {
			return err
		}
		ui.Warning("%s", sentence(err.Error()))
		return nil
	}
	if h.prompting == nil {
		ui.Say("Your code changed. Anyone with the old code is cut off.")
		h.showCard()
	}
	return nil
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
		err := h.connect(ctx)
		if err == nil {
			ui.Say("Back online. Your door is open again.")
			h.waiting()
			return nil
		}
		if errors.Is(err, errCodeStale) {
			return err // trying again in a minute won't fix it
		}
		wait = min(wait*2, 15*time.Second)
	}
	return errors.New("Couldn't get back to the relay. Run knock open to try again")
}
