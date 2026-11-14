package hub_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/hub"
)

// knock.incoming shows the host who the relay verified, never what the
// knocker typed, and marks anonymous knockers as such.
func TestKnockIncomingIdentity(t *testing.T) {
	url, signer := relay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host := dial(t, url, "Maya")
	if err := host.RequestInto(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "maya02", DoorKey: "a-very-secret-door-key", Code: codeRecord("4821KQ", 1)}, nil); err != nil {
		t.Fatal(err)
	}
	verified := protocol.Identity{ID: "github:42", Name: "Sam Carter", Email: "sam@acme.dev", Login: "samc", Provider: "github", Verified: true}
	tok, _ := signer.Issue(auth.KindAccess, verified, time.Minute)

	for _, tc := range []struct {
		name  string
		hello protocol.Hello
		want  protocol.Identity
	}{
		{"signed in, typed name ignored", protocol.Hello{Token: tok, Name: "The CEO", Device: "sam-mbp", Client: "cli"}, verified},
		{"anonymous", protocol.Hello{Name: "Sam \x1b[31mfrom IT", Device: "sam-mbp", Client: "cli"},
			protocol.Identity{Name: "Sam [31mfrom IT", Provider: "anonymous", Verified: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest, err := client.Dial(ctx, url, tc.hello)
			if err != nil {
				t.Fatal(err)
			}
			defer guest.Close()
			hash, err := guest.HashForDoor(ctx, "maya02", "4821KQ")
			if err != nil {
				t.Fatal(err)
			}
			if err := guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "maya02", CodeHash: hash}, nil); err != nil {
				t.Fatal(err)
			}
			var inc protocol.KnockIncoming
			if err := next(t, host, protocol.TypeKnockIncoming).Decode(&inc); err != nil {
				t.Fatal(err)
			}
			got := inc.From
			if !tc.want.Verified {
				if !strings.HasPrefix(got.ID, "anon:") {
					t.Fatalf("anonymous id %q", got.ID)
				}
				got.ID = ""
			}
			if got != tc.want {
				t.Fatalf("host saw %+v, want %+v", got, tc.want)
			}
			if err := host.RequestInto(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: inc.KnockID, Allow: false}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A relay that requires sign-in refuses anonymous hellos with a code the
// clients turn into "sign in first".
func TestSignInRequired(t *testing.T) {
	signer := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	mux := http.NewServeMux()
	mux.Handle("GET /v1/ws", hub.New(hub.Config{AllowAnonymous: false, KnockTimeout: 2 * time.Second, Signer: signer}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := srv.URL
	asProto := func(err error, target **protocol.Error) bool { return errors.As(err, target) }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Dial(ctx, url, protocol.Hello{Name: "Sam", Device: "d", Client: "cli"})
	var perr *protocol.Error
	if !asProto(err, &perr) || perr.Code != protocol.ErrAnonForbidden {
		t.Fatalf("want %s, got %v", protocol.ErrAnonForbidden, err)
	}
	tok, _ := signer.Issue(auth.KindAccess, protocol.Identity{ID: "google:1", Name: "Sam", Provider: "google", Verified: true}, time.Minute)
	c, err := client.Dial(ctx, url, protocol.Hello{Token: tok, Device: "d", Client: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Welcome.AllowsAnon {
		t.Fatal("welcome says anonymous is allowed")
	}
	expired, _ := signer.Issue(auth.KindAccess, protocol.Identity{ID: "google:1", Name: "Sam", Provider: "google", Verified: true}, -time.Second)
	if _, err := client.Dial(ctx, url, protocol.Hello{Token: expired, Device: "d", Client: "cli"}); !asProto(err, &perr) || perr.Code != protocol.ErrUnauthorised {
		t.Fatalf("want %s for an expired token, got %v", protocol.ErrUnauthorised, err)
	}
}
