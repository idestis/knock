package hub_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/hub"
)

const iters = 100_000 // fast for tests; relay accepts >= 100k

func relay(t *testing.T) (string, *auth.Signer) {
	t.Helper()
	signer := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	h := hub.New(hub.Config{AllowAnonymous: true, KnockTimeout: 2 * time.Second, Signer: signer})
	mux := http.NewServeMux()
	mux.Handle("GET /v1/ws", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, signer
}

func dial(t *testing.T, url, name string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, url, protocol.Hello{Name: name, Device: name + "-mbp", Client: "cli", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func codeRecord(code string, version int64) protocol.CodeRecord {
	salt := protocol.NewSalt()
	return protocol.CodeRecord{Salt: salt, CodeHash: protocol.HashCode(code, salt, iters), CodeVersion: version,
		KDF: protocol.KDFPBKDF2SHA256, Iterations: iters}
}

func next(t *testing.T, c *client.Client, typ string) client.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-c.Events:
			if e.Type == typ {
				return e
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func TestKnockAcceptAndExchange(t *testing.T) {
	url, _ := relay(t)
	ctx := context.Background()
	host, guest := dial(t, url, "Maya"), dial(t, url, "Sam")

	if guest.Welcome.User.Verified {
		t.Fatal("anonymous user should not be verified")
	}
	if err := host.RequestInto(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "maya01", DoorKey: "a-very-secret-door-key", Code: codeRecord("4821KQ", 1)}, nil); err != nil {
		t.Fatal(err)
	}

	// wrong code
	bad, _ := guest.HashForDoor(ctx, "maya01", "0000AA")
	_, err := guest.Request(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya01", CodeHash: bad})
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodeMismatch {
		t.Fatalf("want code_mismatch, got %v", err)
	}

	hash, err := guest.HashForDoor(ctx, "maya01", "4821 kq")
	if err != nil {
		t.Fatal(err)
	}
	var st protocol.RecentStatus
	if err := guest.RequestInto(ctx, protocol.TypeRecentCheck, protocol.RecentCheck{Items: []protocol.RecentItem{{DoorID: "maya01", CodeHash: hash}}}, &st); err != nil {
		t.Fatal(err)
	}
	if st.Items[0].Status != protocol.PresenceOnline {
		t.Fatalf("want online, got %s", st.Items[0].Status)
	}

	if err := guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya01", CodeHash: hash}, nil); err != nil {
		t.Fatal(err)
	}
	var inc protocol.KnockIncoming
	if err := next(t, host, protocol.TypeKnockIncoming).Decode(&inc); err != nil {
		t.Fatal(err)
	}
	if inc.From.Name != "Sam" || inc.Device != "Sam-mbp" {
		t.Fatalf("unexpected knock %+v", inc)
	}
	if err := host.RequestInto(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: inc.KnockID, Allow: true}, nil); err != nil {
		t.Fatal(err)
	}
	next(t, host, protocol.TypeSessionStart)
	var gs protocol.SessionStart
	_ = next(t, guest, protocol.TypeSessionStart).Decode(&gs)
	if gs.Role != protocol.RoleGuest || gs.Peer.Name != "Maya" {
		t.Fatalf("bad guest start %+v", gs)
	}

	_ = host.WriteData(ctx, []byte("hello from pty"))
	if got := string(<-guest.Data); got != "hello from pty" {
		t.Fatalf("guest got %q", got)
	}
	_ = guest.WriteData(ctx, []byte("ls\r"))
	if got := string(<-host.Data); got != "ls\r" {
		t.Fatalf("host got %q", got)
	}

	// Hands off: guest input is dropped.
	_ = host.Send(ctx, protocol.TypeSessionMode, protocol.SessionMode{GuestInput: false})
	next(t, guest, protocol.TypeSessionMode)
	_ = guest.WriteData(ctx, []byte("rm -rf /"))
	select {
	case b := <-host.Data:
		t.Fatalf("view-only guest input leaked: %q", b)
	case <-time.After(200 * time.Millisecond):
	}

	// New code ends the session and tells the guest.
	if err := host.RequestInto(ctx, protocol.TypeDoorCodeSet, protocol.DoorCodeSet{Code: codeRecord("1111AB", 2)}, nil); err != nil {
		t.Fatal(err)
	}
	next(t, guest, protocol.TypeSessionEnd)
	next(t, guest, protocol.TypeDoorCodeChanged)
	_, err = guest.Request(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya01", CodeHash: hash})
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodeMismatch {
		t.Fatalf("old hash should fail, got %v", err)
	}
}

func TestDenyTimeoutAndDoorTaken(t *testing.T) {
	url, _ := relay(t)
	ctx := context.Background()
	host, guest, thief := dial(t, url, "Maya"), dial(t, url, "Sam"), dial(t, url, "Eve")
	_ = host.RequestInto(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "maya02", DoorKey: "a-very-secret-door-key", Code: codeRecord("4821KQ", 1)}, nil)

	_, err := thief.Request(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "maya02", DoorKey: "not-the-right-key-at-all", Code: codeRecord("9999ZZ", 1)})
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrDoorTaken {
		t.Fatalf("want door_taken, got %v", err)
	}

	hash, _ := guest.HashForDoor(ctx, "maya02", "4821KQ")
	_ = guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya02", CodeHash: hash}, nil)
	var inc protocol.KnockIncoming
	_ = next(t, host, protocol.TypeKnockIncoming).Decode(&inc)
	_ = host.RequestInto(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: inc.KnockID, Allow: false}, nil)
	var e protocol.Error
	_ = next(t, guest, protocol.TypeError).Decode(&e)
	if e.Code != protocol.ErrDenied {
		t.Fatalf("want denied, got %s", e.Code)
	}

	_ = guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya02", CodeHash: hash}, nil)
	next(t, host, protocol.TypeKnockIncoming)
	_ = next(t, guest, protocol.TypeError).Decode(&e)
	if e.Code != protocol.ErrTimeout {
		t.Fatalf("want timeout, got %s", e.Code)
	}
	next(t, host, protocol.TypeKnockCancelled)
}

func TestSignedInHello(t *testing.T) {
	url, signer := relay(t)
	tok, _ := signer.Issue(auth.KindAccess, protocol.Identity{ID: "github:1", Name: "Maya Patel", Provider: "github", Verified: true}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, url, protocol.Hello{Token: tok, Device: "maya-mbp", Client: "cli", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Welcome.User.Verified || c.Welcome.User.Name != "Maya Patel" {
		t.Fatalf("bad welcome %+v", c.Welcome.User)
	}
	if _, err := client.Dial(ctx, url, protocol.Hello{Token: tok + "x", Device: "d", Client: "cli"}); err == nil {
		t.Fatal("tampered token accepted")
	}
}
