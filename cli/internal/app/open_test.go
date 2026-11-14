package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

func TestDoorChanged(t *testing.T) {
	d := state.Door{DoorID: "bvjy", CodeVersion: 3, Salt: []byte{1}, CodeHash: []byte{2}}
	same := d
	if doorChanged(d, same) {
		t.Error("an identical door is not a change")
	}
	bumped := d
	bumped.CodeVersion = 4
	if !doorChanged(d, bumped) {
		t.Error("a new version is a change")
	}
	// Knock.app and the CLI can both write version N+1: the hash decides.
	rehashed := d
	rehashed.CodeHash = []byte{9}
	if !doorChanged(d, rehashed) {
		t.Error("a new hash at the same version is a change")
	}
	resalted := d
	resalted.Salt = []byte{9}
	if !doorChanged(d, resalted) {
		t.Error("a new salt at the same version is a change")
	}
	other := bumped
	other.DoorID = "zzzz"
	if doorChanged(d, other) {
		t.Error("a different door is not this door's code changing")
	}
}

func TestSessionMatchesOnlyTheKnockLetIn(t *testing.T) {
	k := protocol.KnockIncoming{KnockID: "k_1", From: protocol.Identity{ID: "anon:sam", Name: "Sam"}}
	ok := protocol.SessionStart{SessionID: "s_1", Role: protocol.RoleHost, DoorID: "BVJY", Peer: protocol.Identity{ID: "anon:sam", Name: "Sam"}}
	if !sessionMatches(k, "bvjy", ok) {
		t.Fatal("the knock the host let in should start")
	}
	bad := map[string]func(*protocol.SessionStart){
		"someone else":     func(s *protocol.SessionStart) { s.Peer.ID = "anon:eve" },
		"no peer id":       func(s *protocol.SessionStart) { s.Peer.ID = "" },
		"now verified":     func(s *protocol.SessionStart) { s.Peer.Verified = true },
		"another door":     func(s *protocol.SessionStart) { s.DoorID = "zzzz" },
		"guest role":       func(s *protocol.SessionStart) { s.Role = protocol.RoleGuest },
		"no session id":    func(s *protocol.SessionStart) { s.SessionID = "" },
		"empty everything": func(s *protocol.SessionStart) { *s = protocol.SessionStart{} },
	}
	for name, mutate := range bad {
		st := ok
		mutate(&st)
		if sessionMatches(k, "bvjy", st) {
			t.Errorf("%s: session.start should be refused", name)
		}
	}
}

func TestOwnerChanged(t *testing.T) {
	maya := state.RecentItem{DoorID: "bvjy", OwnerName: "Maya", OwnerEmail: "maya@example.com", Provider: "google", Verified: true}
	tests := []struct {
		name string
		prev state.RecentItem
		now  protocol.Identity
		want bool
	}{
		{"same owner", maya, protocol.Identity{ID: "g:1", Name: "Maya", Email: "Maya@Example.com", Provider: "google", Verified: true}, false},
		{"renamed, same email", maya, protocol.Identity{ID: "g:1", Name: "Maya K", Email: "maya@example.com", Provider: "google", Verified: true}, false},
		{"other email", maya, protocol.Identity{ID: "g:2", Name: "Maya", Email: "eve@example.com", Provider: "google", Verified: true}, true},
		{"verified to unverified", maya, protocol.Identity{ID: "anon:x", Name: "Maya", Provider: "anonymous"}, true},
		{"other provider", maya, protocol.Identity{ID: "gh:1", Name: "Maya", Email: "maya@example.com", Provider: "github", Verified: true}, true},
		{"github without email, same name", state.RecentItem{OwnerName: "Sam", Provider: "github", Verified: true},
			protocol.Identity{Name: "Sam", Provider: "github", Verified: true}, false},
		{"unverified, other name", state.RecentItem{OwnerName: "Sam", Provider: "anonymous"},
			protocol.Identity{Name: "Eve", Provider: "anonymous"}, true},
		{"unverified, same name", state.RecentItem{OwnerName: "Sam", Provider: "anonymous"},
			protocol.Identity{Name: "Sam", Provider: "anonymous"}, false},
		{"nothing pinned", state.RecentItem{DoorID: "bvjy"}, protocol.Identity{Name: "Eve"}, false},
	}
	for _, tt := range tests {
		if got := ownerChanged(tt.prev, tt.now); got != tt.want {
			t.Errorf("%s: ownerChanged = %v, want %v", tt.name, got, tt.want)
		}
	}
	// The escape byte goes; what's left can't move the cursor.
	if got := describeOwner("Ma\x1b[2Jya", "maya@example.com", "google", true); got != "Ma[2Jya (maya@example.com, google)" {
		t.Errorf("describeOwner = %q", got)
	}
	if got := describeOwner("Eve", "", "anonymous", false); got != "Eve (not verified)" {
		t.Errorf("describeOwner = %q", got)
	}
}

// fakeSaltRelay answers hello and door.salt.get with the given rounds.
func fakeSaltRelay(t *testing.T, iterations int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var env protocol.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				return
			}
			var reply any = protocol.Welcome{User: protocol.Identity{ID: "anon:1", Name: "Sam"}}
			if env.Type == protocol.TypeDoorSaltGet {
				reply = protocol.DoorSalt{DoorID: "bvjy", Salt: make([]byte, protocol.SaltLen), KDF: protocol.KDFPBKDF2SHA256, Iterations: iterations}
			}
			out, _ := protocol.Encode(protocol.TypeOK, env.ID, reply)
			if err := c.Write(r.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHashForDoorRefusesOddIterations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, it := range []int{0, 1, 99_999, 10_000_001, 1 << 40} {
		c, err := client.Dial(ctx, fakeSaltRelay(t, it), protocol.Hello{Name: "Sam", Client: protocol.ClientCLI, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.HashForDoor(ctx, "bvjy", "4821KQ"); err == nil {
			t.Errorf("%d rounds should be refused", it)
		}
		c.Close()
	}
	c, err := client.Dial(ctx, fakeSaltRelay(t, client.MinIterations), protocol.Hello{Name: "Sam", Client: protocol.ClientCLI, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if h, err := c.HashForDoor(ctx, "bvjy", "4821KQ"); err != nil || len(h) != protocol.HashLen {
		t.Fatalf("%d rounds should work: %v", client.MinIterations, err)
	}
}

// startKnockd builds and runs the real relay on loopback.
func startKnockd(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	bin := filepath.Join(t.TempDir(), "knockd")
	if out, err := exec.Command(goBin, "build", "-o", bin, "github.com/idestis/knock/relay/cmd/knockd").CombinedOutput(); err != nil {
		t.Fatalf("build knockd: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(bin, "-addr", addr)
	cmd.Env = append(os.Environ(), "KNOCK_ALLOW_ANONYMOUS=true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	url := "http://" + addr
	for range 100 {
		if resp, err := http.Get(url + "/healthz"); err == nil {
			resp.Body.Close()
			return url
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("relay did not start")
	return ""
}

// knock new-code in another terminal while knock open has a live session
// must end that session and make the old code stop working (rule 4).
func TestNewCodeEndsLiveSession(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	url := startKnockd(t)
	old := doorWatchEvery
	doorWatchEvery = 100 * time.Millisecond
	t.Cleanup(func() { doorWatchEvery = old })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dial := func(name string) *client.Client {
		c, err := client.Dial(ctx, url, protocol.Hello{Name: name, Device: name + "-mbp", Client: protocol.ClientCLI, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	store, err := state.ForRelay(url)
	if err != nil {
		t.Fatal(err)
	}
	door, err := store.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	oldCode := door.Code
	hc := dial("maya")
	if err := hc.RequestInto(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: door.DoorID, DoorKey: door.DoorKey, Device: "maya-mbp", Code: door.Record()}, nil); err != nil {
		t.Fatal(err)
	}
	h := &host{cfg: config.Config{Relay: url, KnockTimeout: 60, SessionsSave: config.SaveOff}, store: store, door: door, doorMod: store.DoorModTime(),
		c: hc, argv: []string{"/bin/sh"}, guestInput: true}

	gc := dial("sam")
	hash, err := gc.HashForDoor(ctx, door.DoorID, oldCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := gc.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: door.DoorID, CodeHash: hash}, nil); err != nil {
		t.Fatal(err)
	}
	var inc protocol.KnockIncoming
	for ev := range hc.Events {
		if ev.Type == protocol.TypeKnockIncoming {
			_ = ev.Decode(&inc)
			break
		}
	}
	// What follows the host's "y" (the prompt itself is skipped).
	p := &prompt{knock: inc, cancel: func() {}, result: make(chan ui.Answer, 1)}
	hostDone := make(chan error, 1)
	go func() { hostDone <- h.answer(ctx, p, ui.AnswerYes) }()
	for ev := range gc.Events {
		if ev.Type == protocol.TypeSessionStart {
			break
		}
	}
	var out bytes.Buffer
	_ = gc.WriteData(ctx, []byte("echo in-$((40+2))\r"))
	waitData(t, gc, &out, "in-42")

	// knock new-code, as runNewCode does it.
	d, _, _ := store.LoadDoor()
	d.Rotate()
	if err := store.SaveDoor(d); err != nil {
		t.Fatal(err)
	}

	// The guest is out within a few watch intervals, with the right reason.
	deadline := time.After(5 * time.Second)
	for ended := false; !ended; {
		select {
		case ev := <-gc.Events:
			if ev.Type == protocol.TypeSessionEnd {
				var se protocol.SessionEnd
				_ = ev.Decode(&se)
				if se.Reason != "code_changed" {
					t.Fatalf("session ended with %q, want code_changed", se.Reason)
				}
				ended = true
			}
		case <-gc.Data:
		case <-deadline:
			t.Fatal("the guest is still in the shell after knock new-code")
		}
	}
	select {
	case err := <-hostDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("knock open's session did not end")
	}
	if h.door.CodeVersion != d.CodeVersion || !bytes.Equal(h.door.CodeHash, d.CodeHash) {
		t.Fatalf("knock open still holds code_version %d, want %d", h.door.CodeVersion, d.CodeVersion)
	}

	// The old code no longer works at the relay; the new one does.
	g2 := dial("eve")
	h2, err := g2.HashForDoor(ctx, door.DoorID, oldCode)
	if err != nil {
		t.Fatal(err)
	}
	err = g2.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: door.DoorID, CodeHash: h2}, nil)
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodeMismatch {
		t.Fatalf("knock with the old code: %v, want %s", err, protocol.ErrCodeMismatch)
	}
}

func waitData(t *testing.T, c *client.Client, buf *bytes.Buffer, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(buf.String(), want) {
		select {
		case b := <-c.Data:
			buf.Write(b)
		case <-deadline:
			t.Fatalf("timed out waiting for %q in %q", want, buf.String())
		}
	}
}
