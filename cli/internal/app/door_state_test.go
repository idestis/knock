package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

// scriptRelay is a fake relay: reply decides each answer and every
// request is kept for the test to look at.
type scriptRelay struct {
	mu    sync.Mutex
	seen  []protocol.Envelope
	reply func(env protocol.Envelope) (string, any, *protocol.Error)
	url   string
}

func newScriptRelay(t *testing.T, reply func(protocol.Envelope) (string, any, *protocol.Error)) *scriptRelay {
	t.Helper()
	r := &scriptRelay{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(req.Context())
			if err != nil {
				return
			}
			var env protocol.Envelope
			if json.Unmarshal(b, &env) != nil {
				return
			}
			r.mu.Lock()
			r.seen = append(r.seen, env)
			r.mu.Unlock()
			typ, data, perr := protocol.TypeOK, any(nil), (*protocol.Error)(nil)
			if env.Type == protocol.TypeHello {
				data = protocol.Welcome{User: protocol.Identity{ID: "anon:1", Name: "Maya"}}
			} else {
				typ, data, perr = r.reply(env)
			}
			var out []byte
			if perr != nil {
				out, _ = json.Marshal(protocol.Envelope{Type: protocol.TypeError, ID: env.ID, Error: perr})
			} else {
				out, _ = protocol.Encode(typ, env.ID, data)
			}
			if err := c.Write(req.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

func (r *scriptRelay) requests(typ string) []protocol.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []protocol.Envelope
	for _, e := range r.seen {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (r *scriptRelay) dial(t *testing.T) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, r.url, protocol.Hello{Name: "Maya", Client: protocol.ClientCLI, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func stale() *protocol.Error {
	return &protocol.Error{Code: protocol.ErrCodeStale, Message: "Your door has a newer code than this one."}
}

func TestOpenDoorSendsAndSavesClaim(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	relay := newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) {
		return protocol.TypeDoorOpened, protocol.DoorOpened{DoorID: "bvjy", Claim: "claim-2"}, nil
	})
	store, _ := state.ForRelay(relay.url)
	d, _ := store.LoadOrCreateDoor()
	d.Claim = "claim-1"
	_ = store.SaveDoor(d)

	got, err := openDoor(context.Background(), relay.dial(t), store, d)
	if err != nil {
		t.Fatal(err)
	}
	var sent protocol.DoorOpen
	_ = json.Unmarshal(relay.requests(protocol.TypeDoorOpen)[0].Data, &sent)
	if sent.Claim != "claim-1" || sent.DoorKey != d.DoorKey {
		t.Fatalf("door.open didn't carry the stored claim: %+v", sent)
	}
	if got.Claim != "claim-2" {
		t.Fatalf("returned door claim %q", got.Claim)
	}
	if onDisk, _, _ := store.LoadDoor(); onDisk.Claim != "claim-2" {
		t.Fatalf("claim not saved: %q", onDisk.Claim)
	}
}

func TestOpenDoorStaleReloadsOnce(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	var newer []byte
	relay := newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) {
		var o protocol.DoorOpen
		_ = json.Unmarshal(env.Data, &o)
		if !bytes.Equal(o.Code.CodeHash, newer) {
			return "", nil, stale()
		}
		return protocol.TypeDoorOpened, protocol.DoorOpened{DoorID: o.DoorID}, nil
	})
	store, _ := state.ForRelay(relay.url)
	old, _ := store.LoadOrCreateDoor()
	// Knock.app writes a newer code after knock open read door.json.
	fresh := old
	fresh.Rotate()
	newer = fresh.CodeHash
	_ = store.SaveDoor(fresh)

	got, err := openDoor(context.Background(), relay.dial(t), store, old)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.CodeHash, fresh.CodeHash) || len(relay.requests(protocol.TypeDoorOpen)) != 2 {
		t.Fatalf("expected one retry with the reloaded code, got %d opens", len(relay.requests(protocol.TypeDoorOpen)))
	}

	// Still stale after reloading: stop with a plain message, no more tries.
	newer = []byte("nothing matches")
	before := len(relay.requests(protocol.TypeDoorOpen))
	_, err = openDoor(context.Background(), relay.dial(t), store, fresh)
	if !errors.Is(err, errCodeStale) {
		t.Fatalf("err = %v, want errCodeStale", err)
	}
	if n := len(relay.requests(protocol.TypeDoorOpen)) - before; n != 2 {
		t.Fatalf("%d door.open attempts, want 2", n)
	}
}

func TestReconnectStopsOnStaleCode(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	relay := newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) { return "", nil, stale() })
	store, _ := state.ForRelay(relay.url)
	d, _ := store.LoadOrCreateDoor()
	h := &host{cfg: config.Config{Relay: relay.url, KnockTimeout: 60}, store: store, door: d, doorMod: store.DoorModTime()}
	done := make(chan error, 1)
	go func() { done <- h.reconnect(context.Background(), "Lost the relay.") }()
	select {
	case err := <-done:
		if !errors.Is(err, errCodeStale) {
			t.Fatalf("err = %v, want errCodeStale", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconnect kept retrying a stale code")
	}
}

// codeSetRelay accepts door.code.set and records it.
func codeSetRelay(t *testing.T) *scriptRelay {
	return newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) {
		if env.Type == protocol.TypeDoorOpen {
			return protocol.TypeDoorOpened, protocol.DoorOpened{}, nil
		}
		return protocol.TypeOK, nil, nil
	})
}

func agedHost(t *testing.T, relay *scriptRelay, maxAge int, age time.Duration) *host {
	t.Helper()
	store, _ := state.ForRelay(relay.url)
	d, err := store.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	d.CodeCreatedAt = state.NewTime(time.Now().Add(-age))
	if err := store.SaveDoor(d); err != nil {
		t.Fatal(err)
	}
	return &host{cfg: config.Config{Relay: relay.url, KnockTimeout: 60, SessionsSave: config.SaveOff, CodeMaxAgeDays: maxAge},
		store: store, door: d, doorMod: store.DoorModTime(), c: relay.dial(t)}
}

func TestIdleCodeRotatesWhenTooOld(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	relay := codeSetRelay(t)
	ctx := context.Background()

	// Young code: nothing happens.
	h := agedHost(t, relay, 7, 6*24*time.Hour)
	if err := h.checkCodeAge(ctx, time.Now()); err != nil || len(relay.requests(protocol.TypeDoorCodeSet)) != 0 {
		t.Fatalf("a 6-day-old code was replaced: %v", err)
	}

	// Old code, but someone is knocking: wait.
	h = agedHost(t, relay, 7, 8*24*time.Hour)
	oldCode := h.door.Code
	h.prompting = &prompt{}
	if err := h.checkCodeAge(ctx, time.Now()); err != nil || len(relay.requests(protocol.TypeDoorCodeSet)) != 0 {
		t.Fatalf("rotated with a knock waiting: %v", err)
	}

	// Idle: replaced, saved with a fresh clock, and sent to the relay.
	h.prompting = nil
	if err := h.checkCodeAge(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	sets := relay.requests(protocol.TypeDoorCodeSet)
	if len(sets) != 1 {
		t.Fatalf("%d door.code.set, want 1", len(sets))
	}
	onDisk, _, _ := h.store.LoadDoor()
	if onDisk.Code == oldCode || onDisk.Code != h.door.Code || time.Since(onDisk.CodeCreatedAt.Time) > time.Minute {
		t.Fatalf("door.json not rotated: %+v", onDisk)
	}
	var set protocol.DoorCodeSet
	_ = json.Unmarshal(sets[0].Data, &set)
	if !bytes.Equal(set.Code.CodeHash, onDisk.CodeHash) {
		t.Fatal("the relay got a different code than door.json")
	}

	// 0 means never.
	h = agedHost(t, relay, 0, 400*24*time.Hour)
	if err := h.checkCodeAge(ctx, time.Now()); err != nil || len(relay.requests(protocol.TypeDoorCodeSet)) != 1 {
		t.Fatalf("door.code_max_age_days = 0 still rotated: %v", err)
	}
}

func TestPushCodeStale(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	var accept []byte
	relay := newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) {
		var set protocol.DoorCodeSet
		_ = json.Unmarshal(env.Data, &set)
		if !bytes.Equal(set.Code.CodeHash, accept) {
			return "", nil, stale()
		}
		return protocol.TypeOK, nil, nil
	})
	h := agedHost(t, relay, 7, 0)
	// Another client wrote a newer code; ours is behind.
	newer := h.door
	newer.Rotate()
	_ = h.store.SaveDoor(newer)
	accept = newer.CodeHash
	if err := h.pushCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.door.CodeHash, newer.CodeHash) {
		t.Fatal("knock open didn't adopt the newer code from door.json")
	}
	accept = nil
	if err := h.pushCode(context.Background()); !errors.Is(err, errCodeStale) {
		t.Fatalf("err = %v, want errCodeStale", err)
	}
}

func TestCodeAgePhrase(t *testing.T) {
	for days, want := range map[int]string{1: "a day", 3: "3 days", 7: "a week", 14: "2 weeks", 30: "30 days"} {
		if got := codeAge(days); got != want {
			t.Errorf("codeAge(%d) = %q, want %q", days, got, want)
		}
	}
}

// Pointing knock at another relay never sends this relay's door key.
func TestDoorKeyStaysWithItsRelay(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	a, b := codeSetRelay(t), codeSetRelay(t)
	sa, _ := state.ForRelay(a.url)
	da, _ := sa.LoadOrCreateDoor()
	sb, _ := state.ForRelay(b.url)
	db, _ := sb.LoadOrCreateDoor()
	if _, err := openDoor(context.Background(), b.dial(t), sb, db); err != nil {
		t.Fatal(err)
	}
	var sent protocol.DoorOpen
	_ = json.Unmarshal(b.requests(protocol.TypeDoorOpen)[0].Data, &sent)
	if sent.DoorKey == da.DoorKey || bytes.Equal(sent.Code.CodeHash, da.CodeHash) {
		t.Fatal("relay a's door reached relay b")
	}
}

// versionRelay holds a code at version held and refuses anything not above
// it with code_stale, saying which version it holds when tell is set.
func versionRelay(t *testing.T, held int64, tell bool) *scriptRelay {
	return newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) {
		var rec protocol.CodeRecord
		var o protocol.DoorOpen
		var set protocol.DoorCodeSet
		switch env.Type {
		case protocol.TypeDoorOpen:
			_ = json.Unmarshal(env.Data, &o)
			rec = o.Code
		case protocol.TypeDoorCodeSet:
			_ = json.Unmarshal(env.Data, &set)
			rec = set.Code
		}
		if rec.CodeVersion <= held {
			e := stale()
			if tell {
				e.CodeVersion = int(held)
			}
			return "", nil, e
		}
		if env.Type == protocol.TypeDoorOpen {
			return protocol.TypeDoorOpened, protocol.DoorOpened{DoorID: o.DoorID}, nil
		}
		return protocol.TypeOK, nil, nil
	})
}

func captureSay(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	prev := ui.Out
	ui.Out = &out
	t.Cleanup(func() { ui.Out = prev })
	return &out
}

func doorAtVersion(t *testing.T, store state.Store, v int64) state.Door {
	t.Helper()
	d, err := store.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	d.CodeVersion = v
	if err := store.SaveDoor(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func checkJumped(t *testing.T, store state.Store, old, got state.Door, sent protocol.CodeRecord, out string) {
	t.Helper()
	if got.CodeVersion != 6 || sent.CodeVersion != 6 || !bytes.Equal(sent.CodeHash, got.CodeHash) {
		t.Fatalf("registered version %d (door %d), want 6", sent.CodeVersion, got.CodeVersion)
	}
	if got.Code == old.Code || bytes.Equal(got.Salt, old.Salt) || bytes.Equal(got.CodeHash, old.CodeHash) {
		t.Fatal("the jump reused the old code")
	}
	onDisk, _, _ := store.LoadDoor()
	if onDisk.CodeVersion != 6 || onDisk.Code != got.Code || time.Since(onDisk.CodeCreatedAt.Time) > time.Minute {
		t.Fatalf("door.json not saved with the new code: version %d", onDisk.CodeVersion)
	}
	if !strings.Contains(out, "newer code on the relay") || !strings.Contains(out, got.ShareString()) {
		t.Fatalf("host wasn't told about the new code: %q", out)
	}
}

func TestOpenDoorJumpsPastRelayVersion(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	out := captureSay(t)
	relay := versionRelay(t, 5, true)
	store, _ := state.ForRelay(relay.url)
	old := doorAtVersion(t, store, 2)

	got, err := openDoor(context.Background(), relay.dial(t), store, old)
	if err != nil {
		t.Fatal(err)
	}
	opens := relay.requests(protocol.TypeDoorOpen)
	if len(opens) != 3 {
		t.Fatalf("%d door.open attempts, want 3", len(opens))
	}
	var sent protocol.DoorOpen
	_ = json.Unmarshal(opens[2].Data, &sent)
	checkJumped(t, store, old, got, sent.Code, out.String())
}

func TestOpenDoorStaleWithoutVersionStops(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	relay := versionRelay(t, 5, false)
	store, _ := state.ForRelay(relay.url)
	old := doorAtVersion(t, store, 2)

	_, err := openDoor(context.Background(), relay.dial(t), store, old)
	if !errors.Is(err, errCodeStale) || strings.Contains(err.Error(), "new-code") {
		t.Fatalf("err = %v, want errCodeStale without knock new-code", err)
	}
	if n := len(relay.requests(protocol.TypeDoorOpen)); n != 2 {
		t.Fatalf("%d door.open attempts, want 2", n)
	}
	if onDisk, _, _ := store.LoadDoor(); onDisk.CodeVersion != 2 || onDisk.Code != old.Code {
		t.Fatal("door.json changed without a version from the relay")
	}
}

func TestOpenDoorJumpStillStaleStops(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	// The relay claims version 5 but refuses everything.
	relay := newScriptRelay(t, func(env protocol.Envelope) (string, any, *protocol.Error) {
		e := stale()
		e.CodeVersion = 5
		return "", nil, e
	})
	store, _ := state.ForRelay(relay.url)
	old := doorAtVersion(t, store, 2)
	if _, err := openDoor(context.Background(), relay.dial(t), store, old); !errors.Is(err, errCodeStale) {
		t.Fatalf("err = %v, want errCodeStale", err)
	}
	if n := len(relay.requests(protocol.TypeDoorOpen)); n != 3 {
		t.Fatalf("%d door.open attempts, want 3", n)
	}
}

func TestPushCodeJumpsPastRelayVersion(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	out := captureSay(t)
	relay := versionRelay(t, 5, true)
	h := agedHost(t, relay, 7, 0)
	h.door.CodeVersion = 2
	_ = h.store.SaveDoor(h.door)
	old := h.door

	if err := h.pushCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	sets := relay.requests(protocol.TypeDoorCodeSet)
	if len(sets) != 3 {
		t.Fatalf("%d door.code.set, want 3", len(sets))
	}
	var sent protocol.DoorCodeSet
	_ = json.Unmarshal(sets[2].Data, &sent)
	checkJumped(t, h.store, old, h.door, sent.Code, out.String())
	if h.doorMod != h.store.DoorModTime() {
		t.Fatal("knock open would see its own save as a new code")
	}

	// An older relay that doesn't say its version: stop, change nothing.
	relay2 := versionRelay(t, 50, false)
	h.c = relay2.dial(t)
	before := h.door
	if err := h.pushCode(context.Background()); !errors.Is(err, errCodeStale) {
		t.Fatalf("err = %v, want errCodeStale", err)
	}
	if onDisk, _, _ := h.store.LoadDoor(); onDisk.Code != before.Code || onDisk.CodeVersion != before.CodeVersion {
		t.Fatal("door.json changed without a version from the relay")
	}
}
