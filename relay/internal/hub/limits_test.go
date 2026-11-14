package hub_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/hub"
)

const doorKey = "a-very-secret-door-key"

func relayWith(t *testing.T, cfg hub.Config) (string, *auth.Signer, *hub.Hub) {
	t.Helper()
	cfg.Signer = auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	cfg.AllowAnonymous = true
	if cfg.KnockTimeout == 0 {
		cfg.KnockTimeout = 2 * time.Second
	}
	h := hub.New(cfg)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/ws", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, cfg.Signer, h
}

// dialAs connects as a signed-in user, so limits key on the identity
// rather than the shared test IP.
func dialAs(t *testing.T, url string, signer *auth.Signer, id string) *client.Client {
	t.Helper()
	tok, _ := signer.Issue(auth.KindAccess, protocol.Identity{ID: "github:" + id, Name: id, Provider: "github", Verified: true}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, url, protocol.Hello{Token: tok, Device: id + "-mbp", Client: "cli", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func randHash() []byte {
	b := make([]byte, protocol.HashLen)
	_, _ = rand.Read(b)
	return b
}

func errCode(err error) string {
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return perr.Code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func openDoor(t *testing.T, c *client.Client, id string, rec protocol.CodeRecord) {
	t.Helper()
	if err := c.RequestInto(context.Background(), protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: id, DoorKey: doorKey, Code: rec}, nil); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, c *client.Client, items ...protocol.RecentItem) (protocol.RecentStatus, error) {
	t.Helper()
	var st protocol.RecentStatus
	err := c.RequestInto(context.Background(), protocol.TypeRecentCheck, protocol.RecentCheck{Items: items}, &st)
	return st, err
}

func knockWith(c *client.Client, door string, hash []byte) error {
	_, err := c.Request(context.Background(), protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: door, CodeHash: hash})
	return err
}

// recent.check spends the same wrong-code budget as knock.request, so it
// can't be used to test hashes without limit.
func TestRecentCheckSpendsWrongCodeBudget(t *testing.T) {
	url, _ := relay(t)
	host, guest := dial(t, url, "Maya"), dial(t, url, "Eve")
	rec := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya10", rec)

	// The same door twice in one message is refused.
	if _, err := check(t, guest, protocol.RecentItem{DoorID: "maya10", CodeHash: randHash()}, protocol.RecentItem{DoorID: "maya10", CodeHash: randHash()}); errCode(err) != protocol.ErrBadRequest {
		t.Fatalf("duplicate door ids: want bad_request, got %v", err)
	}

	// Ten wrong hashes on ten doors use up the knocker's budget.
	items := make([]protocol.RecentItem, 0, 10)
	for i := range 10 {
		items = append(items, protocol.RecentItem{DoorID: fmt.Sprintf("door%02d", i), CodeHash: randHash()})
	}
	st, err := check(t, guest, items...)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range st.Items {
		if e.Status != protocol.PresenceOffline || e.Owner != nil {
			t.Fatalf("wrong hash should read offline with no owner, got %+v", e)
		}
	}

	// Now even the right hash isn't checked, by either path.
	if _, err := check(t, guest, protocol.RecentItem{DoorID: "maya10", CodeHash: rec.CodeHash}); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("recent.check after 10 wrong hashes: want rate_limited, got %v", err)
	}
	if err := knockWith(guest, "maya10", rec.CodeHash); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("knock after 10 wrong hashes in recent.check: want rate_limited, got %v", err)
	}
}

// Wrong hashes in recent.check count against the door too, and once its
// budget is spent the rest of the message isn't checked.
func TestRecentCheckSpendsDoorBudget(t *testing.T) {
	url, signer, _ := relayWith(t, hub.Config{})
	host := dial(t, url, "Maya")
	rec := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya11", rec)

	eve := dialAs(t, url, signer, "eve")
	for range 5 {
		st, err := check(t, eve, protocol.RecentItem{DoorID: "maya11", CodeHash: randHash()})
		if err != nil || st.Items[0].Status != protocol.PresenceOffline {
			t.Fatalf("wrong hash: got %+v, %v", st, err)
		}
	}
	st, err := check(t, eve, protocol.RecentItem{DoorID: "maya11", CodeHash: rec.CodeHash})
	if err != nil || st.Items[0].Status != protocol.PresenceOffline || st.Items[0].Owner != nil {
		t.Fatalf("right hash after the door budget is spent should read offline, got %+v, %v", st, err)
	}

	// Someone else with the code still gets through.
	sam := dialAs(t, url, signer, "sam")
	st, err = check(t, sam, protocol.RecentItem{DoorID: "maya11", CodeHash: rec.CodeHash})
	if err != nil || st.Items[0].Status != protocol.PresenceOnline {
		t.Fatalf("code holder: want online, got %+v, %v", st, err)
	}
}

func TestRecentCheckMessageLimit(t *testing.T) {
	url, _ := relay(t)
	guest := dial(t, url, "Sam")
	for i := range 60 {
		if _, err := check(t, guest); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	if _, err := check(t, guest); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("61st check: want rate_limited, got %v", err)
	}
}

// An unknown door and a wrong hash answer the same, so recent.check can't
// find doors. A hash from an earlier code says the code changed, and isn't
// counted as a guess.
func TestRecentCheckHidesDoors(t *testing.T) {
	url, _ := relay(t)
	host, guest := dial(t, url, "Maya"), dial(t, url, "Sam")
	old := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya12", old)

	st, err := check(t, guest,
		protocol.RecentItem{DoorID: "maya12", CodeHash: randHash()},
		protocol.RecentItem{DoorID: "nope12", CodeHash: randHash()})
	if err != nil {
		t.Fatal(err)
	}
	a, b := st.Items[0], st.Items[1]
	if a.Status != b.Status || a.Owner != nil || b.Owner != nil || a.Device != b.Device || a.CodeVersion != b.CodeVersion {
		t.Fatalf("known and unknown doors answer differently: %+v vs %+v", a, b)
	}

	cur := codeRecord("1111AB", 2)
	if err := host.RequestInto(context.Background(), protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: cur}, nil); err != nil {
		t.Fatal(err)
	}
	// Way past any wrong-code budget: a stale hash is not a guess.
	for range 12 {
		st, err := check(t, guest, protocol.RecentItem{DoorID: "maya12", CodeHash: old.CodeHash})
		if err != nil || st.Items[0].Status != protocol.PresenceCodeChanged {
			t.Fatalf("old hash: want code_changed, got %+v, %v", st, err)
		}
		if err := knockWith(guest, "maya12", old.CodeHash); errCode(err) != protocol.ErrCodeMismatch {
			t.Fatalf("knock with old hash: want code_mismatch, got %v", err)
		}
	}
	st, err = check(t, guest, protocol.RecentItem{DoorID: "maya12", CodeHash: cur.CodeHash})
	if err != nil || st.Items[0].Status != protocol.PresenceOnline {
		t.Fatalf("new hash: want online, got %+v, %v", st, err)
	}
}

// One person's wrong guesses don't lock out a signed-in guest who has the
// code. The door as a whole still refuses everyone after 30 in an hour.
func TestWrongCodesDontLockOutOthers(t *testing.T) {
	url, signer, _ := relayWith(t, hub.Config{})
	host := dial(t, url, "Maya")
	rec := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya13", rec)

	eve := dialAs(t, url, signer, "eve")
	for range 5 {
		if err := knockWith(eve, "maya13", randHash()); errCode(err) != protocol.ErrCodeMismatch {
			t.Fatalf("want code_mismatch, got %v", err)
		}
	}
	if err := knockWith(eve, "maya13", rec.CodeHash); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("eve after 5 wrong codes: want rate_limited, got %v", err)
	}

	sam := dialAs(t, url, signer, "sam")
	if err := knockWith(sam, "maya13", rec.CodeHash); err != nil {
		t.Fatalf("sam has the code and should get through: %v", err)
	}
	next(t, host, protocol.TypeKnockIncoming)
	_ = sam.Send(context.Background(), protocol.TypeKnockCancel, nil)

	// 25 more wrong codes from five more people: 30 on the door.
	forced := false
	for i := range 5 {
		c := dialAs(t, url, signer, fmt.Sprintf("bot%d", i))
		for range 5 {
			if err := knockWith(c, "maya13", randHash()); errCode(err) != protocol.ErrCodeMismatch {
				t.Fatalf("want code_mismatch, got %v", err)
			}
		}
	}
	timeout := time.After(5 * time.Second)
	for !forced {
		select {
		case e := <-host.Events:
			forced = e.Type == protocol.TypeDoorForceNew
		case <-timeout:
			t.Fatal("host never got door.force_new_code")
		}
	}
	late := dialAs(t, url, signer, "late")
	if err := knockWith(late, "maya13", rec.CodeHash); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("after 30 wrong codes the door should refuse everyone, got %v", err)
	}
}

// A guest repeating the same wrong hash (a stale Recent row, say) spends
// the budget once.
func TestRepeatedWrongHashCountsOnce(t *testing.T) {
	url, signer, _ := relayWith(t, hub.Config{})
	host := dial(t, url, "Maya")
	rec := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya14", rec)
	sam := dialAs(t, url, signer, "sam")
	same := randHash()
	for range 8 {
		if err := knockWith(sam, "maya14", same); errCode(err) != protocol.ErrCodeMismatch {
			t.Fatalf("want code_mismatch, got %v", err)
		}
	}
	if err := knockWith(sam, "maya14", rec.CodeHash); err != nil {
		t.Fatalf("knock with the right code: %v", err)
	}
}

// A code holder knocking over and over is cut off before the host's queue
// fills, and the host stays connected.
func TestKnockLoopIsLimited(t *testing.T) {
	url, _ := relay(t)
	host, guest := dial(t, url, "Maya"), dial(t, url, "Sam")
	rec := codeRecord("4821KQ", 1)
	openDoor(t, host, "maya15", rec)
	for i := range 12 {
		if err := knockWith(guest, "maya15", rec.CodeHash); err != nil {
			t.Fatalf("knock %d: %v", i, err)
		}
	}
	if err := knockWith(guest, "maya15", rec.CodeHash); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("13th knock: want rate_limited, got %v", err)
	}
	if err := host.RequestInto(context.Background(), protocol.TypeDoorClose, nil, nil); err != nil {
		t.Fatalf("host should still be connected: %v", err)
	}
}

// A host who lets someone in stops waiting at another door, and so does a
// knocker who opens their own door.
func TestBecomingHostCancelsOwnKnock(t *testing.T) {
	url, _ := relay(t)
	ctx := context.Background()
	x, y, z := dial(t, url, "Xan"), dial(t, url, "Yui"), dial(t, url, "Zed")
	recA, recB := codeRecord("4821KQ", 1), codeRecord("1111AB", 1)
	openDoor(t, x, "doora1", recA)
	openDoor(t, y, "doorb1", recB)

	// X knocks on Y's door; Z knocks on X's; X lets Z in.
	if err := knockWith(x, "doorb1", recB.CodeHash); err != nil {
		t.Fatal(err)
	}
	var yInc protocol.KnockIncoming
	_ = next(t, y, protocol.TypeKnockIncoming).Decode(&yInc)
	if err := knockWith(z, "doora1", recA.CodeHash); err != nil {
		t.Fatal(err)
	}
	var xInc protocol.KnockIncoming
	_ = next(t, x, protocol.TypeKnockIncoming).Decode(&xInc)
	if err := x.RequestInto(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: xInc.KnockID, Allow: true}, nil); err != nil {
		t.Fatal(err)
	}
	var e protocol.Error
	_ = next(t, x, protocol.TypeError).Decode(&e)
	if e.Code != protocol.ErrDoorBusy {
		t.Fatalf("x's own knock: want door_busy, got %+v", e)
	}
	next(t, y, protocol.TypeKnockCancelled)
	if _, err := y.Request(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: yInc.KnockID, Allow: true}); errCode(err) != protocol.ErrNotFound {
		t.Fatalf("y letting in a host mid-session: want not_found, got %v", err)
	}

	// W knocks on Y's door, then opens its own.
	w := dial(t, url, "Wes")
	if err := knockWith(w, "doorb1", recB.CodeHash); err != nil {
		t.Fatal(err)
	}
	_ = next(t, y, protocol.TypeKnockIncoming).Decode(&yInc)
	openDoor(t, w, "doorw1", codeRecord("2222CD", 1))
	_ = next(t, w, protocol.TypeError).Decode(&e)
	if e.Code != protocol.ErrDoorBusy {
		t.Fatalf("w's knock after opening its door: want door_busy, got %+v", e)
	}
	if _, err := y.Request(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: yInc.KnockID, Allow: true}); errCode(err) != protocol.ErrNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

// door.open and door.code.set can't bring back a code the host replaced.
func TestStaleCodeRecordRefused(t *testing.T) {
	url, _ := relay(t)
	ctx := context.Background()
	app, cli, guest := dial(t, url, "App"), dial(t, url, "Cli"), dial(t, url, "Sam")
	v1, v2 := codeRecord("4821KQ", 1), codeRecord("1111AB", 2)
	openDoor(t, app, "maya16", v1)
	openDoor(t, cli, "maya16", v2)

	if _, err := app.Request(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "maya16", DoorKey: doorKey, Code: v1}); errCode(err) != "code_stale" {
		t.Fatalf("older version: want code_stale, got %v", err)
	}
	same := codeRecord("3333EF", 2)
	if _, err := app.Request(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "maya16", DoorKey: doorKey, Code: same}); errCode(err) != "code_stale" {
		t.Fatalf("same version, other hash: want code_stale, got %v", err)
	}
	if _, err := cli.Request(ctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: v1}); errCode(err) != "code_stale" {
		t.Fatalf("door.code.set older version: want code_stale, got %v", err)
	}
	if _, err := cli.Request(ctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: same}); errCode(err) != "code_stale" {
		t.Fatalf("door.code.set same version, other hash: want code_stale, got %v", err)
	}
	// The same record again is fine (a reconnect).
	openDoor(t, cli, "maya16", v2)

	if err := knockWith(guest, "maya16", v1.CodeHash); errCode(err) != protocol.ErrCodeMismatch {
		t.Fatalf("old code: want code_mismatch, got %v", err)
	}
	if err := knockWith(guest, "maya16", v2.CodeHash); err != nil {
		t.Fatalf("current code: %v", err)
	}
	next(t, cli, protocol.TypeKnockIncoming)
}

// Closed doors nobody watches are forgotten after DoorTTL.
func TestClosedDoorsArePruned(t *testing.T) {
	url, _, h := relayWith(t, hub.Config{DoorTTL: 200 * time.Millisecond})
	rec := codeRecord("4821KQ", 1)
	host, guest := dial(t, url, "Maya"), dial(t, url, "Sam")
	openDoor(t, host, "maya17", rec)
	openDoor(t, dial(t, url, "Lee"), "lee017", codeRecord("1111AB", 1))
	if st, err := check(t, guest, protocol.RecentItem{DoorID: "maya17", CodeHash: rec.CodeHash}); err != nil || st.Items[0].Status != protocol.PresenceOnline {
		t.Fatalf("want online, got %+v, %v", st, err)
	}
	host.Close()
	waitFor(t, func() bool { return h.Stats()["open_doors"] == 1 })
	time.Sleep(600 * time.Millisecond)
	if n := h.Stats()["doors"]; n != 2 {
		t.Fatalf("a watched door and an open door must stay: %d doors", n)
	}
	guest.Close()
	waitFor(t, func() bool { return h.Stats()["doors"] == 1 })
}

func TestDoorCreationLimit(t *testing.T) {
	url, _ := relay(t)
	c := dial(t, url, "Maya")
	for i := range 20 {
		openDoor(t, c, fmt.Sprintf("many%02d", i), codeRecord("4821KQ", 1))
	}
	if _, err := c.Request(context.Background(), protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "many20", DoorKey: doorKey, Code: codeRecord("4821KQ", 1)}); errCode(err) != protocol.ErrRateLimited {
		t.Fatalf("21st new door: want rate_limited, got %v", err)
	}
	// Reopening one you have is fine.
	openDoor(t, c, "many00", codeRecord("4821KQ", 2))
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
