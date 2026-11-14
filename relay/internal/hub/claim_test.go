package hub_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/hub"
	"github.com/idestis/knock/relay/internal/store"
)

const otherKey = "someone-elses-door-key-entirely"

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// boot starts a relay on st with the test secret. stop ends it like a
// restart: every socket drops and the hub's memory is gone.
func boot(t *testing.T, st store.Store, cfg hub.Config) (url string, stop func()) {
	t.Helper()
	cfg.Signer = auth.NewSigner(testSecret)
	cfg.AllowAnonymous = true
	cfg.Store = st
	if cfg.KnockTimeout == 0 {
		cfg.KnockTimeout = 2 * time.Second
	}
	h := hub.New(cfg)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/ws", h)
	srv := httptest.NewServer(mux)
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			h.CloseAll()
			srv.Close()
		}
	}
	t.Cleanup(stop)
	return srv.URL, stop
}

func open(c *client.Client, id, key, claim string, rec protocol.CodeRecord) (protocol.DoorOpened, error) {
	var out protocol.DoorOpened
	err := c.RequestInto(context.Background(), protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: id, DoorKey: key, Code: rec, Claim: claim}, &out)
	return out, err
}

func mustOpen(t *testing.T, c *client.Client, id, key, claim string, rec protocol.CodeRecord) string {
	t.Helper()
	out, err := open(c, id, key, claim, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out.Claim == "" {
		t.Fatal("door.opened without a claim")
	}
	return out.Claim
}

func reason(t *testing.T, e client.Event) string {
	t.Helper()
	var se protocol.SessionEnd
	if err := e.Decode(&se); err != nil {
		t.Fatal(err)
	}
	return se.Reason
}

// startSession knocks guest on host's door and lets them in.
func startSession(t *testing.T, host, guest *client.Client, id string, hash []byte) protocol.SessionStart {
	t.Helper()
	ctx := context.Background()
	if err := guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: id, CodeHash: hash}, nil); err != nil {
		t.Fatal(err)
	}
	var inc protocol.KnockIncoming
	if err := next(t, host, protocol.TypeKnockIncoming).Decode(&inc); err != nil {
		t.Fatal(err)
	}
	if err := host.RequestInto(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: inc.KnockID, Allow: true}, nil); err != nil {
		t.Fatal(err)
	}
	var s protocol.SessionStart
	_ = next(t, host, protocol.TypeSessionStart).Decode(&s)
	next(t, guest, protocol.TypeSessionStart)
	return s
}

// The audit case: the relay restarts with no memory of the door, a past
// guest who knows the id and the code opens it first and poses as the
// host. The host's claim takes the door back and cuts the squatter off.
func TestClaimTakesBackSquattedDoor(t *testing.T) {
	ctx := context.Background()
	rec := codeRecord("4821KQ", 1)

	url, stop := boot(t, store.NewMemory(time.Hour), hub.Config{})
	host := dial(t, url, "Maya")
	claim := mustOpen(t, host, "maya30", doorKey, "", rec)
	stop()

	// A fresh relay with an empty store: the squatter gets there first and
	// registers the host's own code.
	url, _ = boot(t, store.NewMemory(time.Hour), hub.Config{})
	time.Sleep(5 * time.Millisecond) // claims are timed to the millisecond
	squatter, guest := dial(t, url, "Eve"), dial(t, url, "Sam")
	squatClaim := mustOpen(t, squatter, "maya30", otherKey, "", rec)
	startSession(t, squatter, guest, "maya30", rec.CodeHash)
	watcher := dial(t, url, "Lee")
	if st, err := check(t, watcher, protocol.RecentItem{DoorID: "maya30", CodeHash: rec.CodeHash}); err != nil || st.Items[0].Status != protocol.PresenceBusy {
		t.Fatalf("watcher: %+v %v", st, err)
	}

	host = dial(t, url, "Maya")
	// No claim, a forged one, one for another door, or the squatter's own
	// claim with the host's key: all refused.
	forged := claim[:len(claim)-4] + "AAAA"
	otherDoor := mustOpen(t, dial(t, url, "Maya2"), "maya31", doorKey, "", rec)
	for name, c := range map[string]string{"none": "", "forged": forged, "other door": otherDoor, "other key": squatClaim, "garbage": "c1.1.x"} {
		if _, err := open(host, "maya30", doorKey, c, rec); errCode(err) != protocol.ErrDoorTaken {
			t.Fatalf("%s claim: want door_taken, got %v", name, err)
		}
	}

	// The real claim wins.
	newClaim := mustOpen(t, host, "maya30", doorKey, claim, rec)
	if !strings.HasPrefix(newClaim, "c1.") {
		t.Fatalf("claim format %q", newClaim)
	}
	if r := reason(t, next(t, squatter, protocol.TypeDoorClose)); r != "door_reclaimed" {
		t.Fatalf("squatter told %q", r)
	}
	if r := reason(t, next(t, guest, protocol.TypeSessionEnd)); r != "door_reclaimed" {
		t.Fatalf("guest told %q", r)
	}
	next(t, watcher, protocol.TypeDoorCodeChanged)

	// Knocks now reach the host, not the squatter.
	if err := guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya30", CodeHash: rec.CodeHash}, nil); err != nil {
		t.Fatal(err)
	}
	next(t, host, protocol.TypeKnockIncoming)
	_ = guest.Send(ctx, protocol.TypeKnockCancel, nil)

	// The squatter's newer claim can't take it back, with its key or the host's.
	if _, err := open(squatter, "maya30", otherKey, squatClaim, rec); errCode(err) != protocol.ErrDoorTaken {
		t.Fatalf("squatter's claim: want door_taken, got %v", err)
	}
	// The host keeps its original claim time across the takeover.
	if _, err := open(host, "maya30", doorKey, newClaim, rec); err != nil {
		t.Fatalf("host reopen: %v", err)
	}
}

// With a persistent store, a restart doesn't hand out the id at all: the
// store remembers which key holds it, its code and earlier codes.
func TestStoreKeepsOwnershipAcrossRestart(t *testing.T) {
	t.Run("memory", func(t *testing.T) { keepsOwnership(t, store.NewMemory(time.Hour)) })
	t.Run("valkey", func(t *testing.T) {
		url := os.Getenv("KNOCK_TEST_VALKEY_URL")
		if url == "" {
			t.Skip("KNOCK_TEST_VALKEY_URL not set")
		}
		st, err := store.NewValkey(url, time.Hour, "knocktest:"+hex.EncodeToString(randHash()[:6])+":")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		keepsOwnership(t, st)
	})
}

func keepsOwnership(t *testing.T, st store.Store) {
	ctx := context.Background()
	v1, v2 := codeRecord("4821KQ", 1), codeRecord("1111AB", 2)

	url, stop := boot(t, st, hub.Config{})
	host := dial(t, url, "Maya")
	mustOpen(t, host, "maya32", doorKey, "", v1)
	if err := host.RequestInto(ctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: v2}, nil); err != nil {
		t.Fatal(err)
	}
	stop()

	url, _ = boot(t, st, hub.Config{})
	guest, thief := dial(t, url, "Sam"), dial(t, url, "Eve")

	// Before the host is back, guests see the door as it was: the real
	// salt, offline with the current code, code changed with the old one.
	var salt protocol.DoorSalt
	if err := guest.RequestInto(ctx, protocol.TypeDoorSaltGet, protocol.DoorSaltGet{DoorID: "maya32"}, &salt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(salt.Salt, v2.Salt) {
		t.Fatal("salt not restored from the store")
	}
	st2, err := check(t, guest, protocol.RecentItem{DoorID: "maya32", CodeHash: v2.CodeHash})
	if err != nil || st2.Items[0].Status != protocol.PresenceOffline || st2.Items[0].Owner == nil {
		t.Fatalf("current code: %+v %v", st2, err)
	}
	if err := knockWith(guest, "maya32", v2.CodeHash); errCode(err) != protocol.ErrDoorOffline {
		t.Fatalf("knock while away: want door_offline, got %v", err)
	}
	other := dial(t, url, "Lee")
	if st, err := check(t, other, protocol.RecentItem{DoorID: "maya32", CodeHash: v1.CodeHash}); err != nil || st.Items[0].Status != protocol.PresenceCodeChanged {
		t.Fatalf("old code: %+v %v", st, err)
	}

	// Someone with the code but not the key can't have the door.
	if _, err := open(thief, "maya32", otherKey, "", v2); errCode(err) != protocol.ErrDoorTaken {
		t.Fatalf("thief: want door_taken, got %v", err)
	}
	// Nor can the host bring back the code it replaced.
	host = dial(t, url, "Maya")
	if _, err := open(host, "maya32", doorKey, "", v1); errCode(err) != protocol.ErrCodeStale || staleVersion(err) != 2 {
		t.Fatalf("old code: want code_stale with the store's version 2, got %v (version %d)", err, staleVersion(err))
	}
	mustOpen(t, host, "maya32", doorKey, "", v2)
	// The guest watching from Recent hears the door is back.
	var e protocol.RecentEntry
	_ = next(t, guest, protocol.TypePresenceUpdate).Decode(&e)
	if e.Status != protocol.PresenceOnline {
		t.Fatalf("presence %+v", e)
	}
}

// A takeover in the store is a conditional write: if another relay hands
// the id to a third key first, the claim holder's open fails closed.
func TestStoreConflictFailsClosed(t *testing.T) {
	st := store.NewMemory(time.Hour)
	url, _ := boot(t, st, hub.Config{})
	host := dial(t, url, "Maya")
	if _, err := open(host, "maya33", doorKey, "", codeRecord("4821KQ", 1)); err != nil {
		t.Fatal(err)
	}
	// Another relay on the same store has handed the id to someone else.
	ctx := context.Background()
	got, _ := st.Load(ctx, []string{"maya33"})
	cur := got["maya33"]
	stolen := cur
	stolen.KeyHash = make([]byte, 32)
	if err := st.Save(ctx, stolen, cur.KeyHash); err != nil {
		t.Fatal(err)
	}
	// This relay finds out on its next write and closes the door.
	if err := host.RequestInto(ctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: codeRecord("1111AB", 2)}, nil); err != nil {
		t.Fatal(err)
	}
	if r := reason(t, next(t, host, protocol.TypeDoorClose)); r != "door_reclaimed" {
		t.Fatalf("host told %q", r)
	}
}

type brokenStore struct{ store.Store }

var errDown = errors.New("store down")

func (brokenStore) Load(context.Context, []string) (map[string]store.Door, error) {
	return nil, errDown
}
func (brokenStore) Save(context.Context, store.Door, []byte) error { return errDown }
func (brokenStore) Touch(context.Context, string, []byte) error    { return errDown }

// A store that can't answer never hands out a door, and guests aren't
// charged wrong guesses for doors it couldn't look up.
func TestStoreDownFailsClosed(t *testing.T) {
	ctx := context.Background()
	url, _ := boot(t, brokenStore{store.NewMemory(time.Hour)}, hub.Config{})
	host, guest := dial(t, url, "Maya"), dial(t, url, "Sam")
	if _, err := open(host, "maya34", doorKey, "", codeRecord("4821KQ", 1)); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("open: want rate_limited, got %v", err)
	}
	if _, err := guest.Request(ctx, protocol.TypeDoorSaltGet, protocol.DoorSaltGet{DoorID: "maya34"}); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("salt: want rate_limited, got %v", err)
	}
	if err := knockWith(guest, "maya34", randHash()); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("knock: want rate_limited, got %v", err)
	}
	for range 12 {
		st, err := check(t, guest, protocol.RecentItem{DoorID: "maya34", CodeHash: randHash()})
		if err != nil || st.Items[0].Status != protocol.PresenceOffline {
			t.Fatalf("recent: %+v %v", st, err)
		}
	}
	// None of that spent the wrong-code budget (10 per knocker).
	if _, err := check(t, guest, protocol.RecentItem{DoorID: "maya34", CodeHash: randHash()}); err != nil {
		t.Fatalf("budget spent while the store was down: %v", err)
	}
}

// Open doors keep their store record alive; a door whose host left lapses
// after the TTL and its id is free again.
func TestHeartbeatKeepsOpenDoorsAlive(t *testing.T) {
	ttl := 400 * time.Millisecond
	st := store.NewMemory(ttl)
	url, _ := boot(t, st, hub.Config{DoorTTL: ttl, Heartbeat: 100 * time.Millisecond})
	host := dial(t, url, "Maya")
	mustOpen(t, host, "maya35", doorKey, "", codeRecord("4821KQ", 1))
	held := func() bool {
		got, _ := st.Load(context.Background(), []string{"maya35"})
		return len(got) == 1
	}
	time.Sleep(3 * ttl)
	if !held() {
		t.Fatal("open door lapsed")
	}
	host.Close()
	waitFor(t, func() bool { return !held() })
}

// session.end with a session_id only ends that session.
func TestSessionEndMatchesID(t *testing.T) {
	ctx := context.Background()
	url, _ := relay(t)
	host, guest := dial(t, url, "Maya"), dial(t, url, "Sam")
	rec := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya36", rec)
	s := startSession(t, host, guest, "maya36", rec.CodeHash)

	if err := guest.RequestInto(ctx, protocol.TypeSessionEnd, protocol.SessionEnd{SessionID: "s_not_this_one"}, nil); err != nil {
		t.Fatal(err)
	}
	_ = host.WriteData(ctx, []byte("still here"))
	select {
	case b := <-guest.Data:
		if string(b) != "still here" {
			t.Fatalf("guest got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a mismatched session_id ended the session")
	}

	if err := guest.RequestInto(ctx, protocol.TypeSessionEnd, protocol.SessionEnd{SessionID: s.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	if r := reason(t, next(t, host, protocol.TypeSessionEnd)); r != protocol.EndGuestLeft {
		t.Fatalf("host told %q", r)
	}
}

func staleVersion(err error) int {
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return perr.CodeVersion
	}
	return -1
}

// code_stale tells the key holder which version the relay holds, from
// memory too, so it can make a fresh code above it. No other error does.
func TestCodeStaleCarriesVersion(t *testing.T) {
	ctx := context.Background()
	url, _ := relay(t)
	app, cli := dial(t, url, "App"), dial(t, url, "Cli")
	openDoor(t, app, "maya37", codeRecord("4821KQ", 1))
	openDoor(t, cli, "maya37", codeRecord("1111AB", 5))
	if _, err := open(app, "maya37", doorKey, "", codeRecord("2222CD", 3)); errCode(err) != protocol.ErrCodeStale || staleVersion(err) != 5 {
		t.Fatalf("door.open: want code_stale version 5, got %v (version %d)", err, staleVersion(err))
	}
	if _, err := cli.Request(ctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: codeRecord("3333EF", 4)}); errCode(err) != protocol.ErrCodeStale || staleVersion(err) != 5 {
		t.Fatalf("door.code.set: want code_stale version 5, got %v (version %d)", err, staleVersion(err))
	}
	if _, err := open(dial(t, url, "Eve"), "maya37", otherKey, "", codeRecord("9999ZZ", 1)); errCode(err) != protocol.ErrDoorTaken || staleVersion(err) != 0 {
		t.Fatalf("door_taken must not carry a version: %v (version %d)", err, staleVersion(err))
	}
}
