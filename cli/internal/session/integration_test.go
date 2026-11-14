package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/record"
	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

// startRelay builds and runs the real knockd. The hub is internal to relay/,
// so the CLI tests talk to the binary like the CLI does.
func startRelay(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	bin := filepath.Join(t.TempDir(), "knockd")
	build := exec.Command(goBin, "build", "-o", bin, "github.com/idestis/knock/relay/cmd/knockd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build knockd: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(bin, "-addr", addr)
	cmd.Env = append(os.Environ(), "KNOCK_ALLOW_ANONYMOUS=true", "KNOCK_KNOCK_TIMEOUT_SECONDS=10")
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

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nextEvent(t *testing.T, c *client.Client, typ string) client.Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
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

func TestSharedShellThroughRelay(t *testing.T) {
	url := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dial := func(name string) *client.Client {
		c, err := client.Dial(ctx, url, protocol.Hello{Name: name, Device: strings.ToLower(name) + "-mbp", Client: protocol.ClientCLI, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	host, guest := dial("Maya"), dial("Sam")

	salt := protocol.NewSalt()
	rec := protocol.CodeRecord{Salt: salt, CodeHash: protocol.HashCode("4821KQ", salt, 100_000), CodeVersion: 1,
		KDF: protocol.KDFPBKDF2SHA256, Iterations: 100_000}
	if err := host.RequestInto(ctx, protocol.TypeDoorOpen, protocol.DoorOpen{DoorID: "k7f3x9q2", DoorKey: "a-very-secret-door-key", Code: rec}, nil); err != nil {
		t.Fatal(err)
	}
	hash, err := guest.HashForDoor(ctx, "k7f3x9q2", "4821 kq")
	if err != nil {
		t.Fatal(err)
	}
	if err := guest.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: "k7f3x9q2", CodeHash: hash}, nil); err != nil {
		t.Fatal(err)
	}
	var inc protocol.KnockIncoming
	_ = nextEvent(t, host, protocol.TypeKnockIncoming).Decode(&inc)
	if inc.From.Verified {
		t.Fatal("anonymous knocker must not be verified")
	}
	if err := host.RequestInto(ctx, protocol.TypeKnockAnswer, protocol.KnockAnswer{KnockID: inc.KnockID, Allow: true}, nil); err != nil {
		t.Fatal(err)
	}
	var hs, gs protocol.SessionStart
	_ = nextEvent(t, host, protocol.TypeSessionStart).Decode(&hs)
	_ = nextEvent(t, guest, protocol.TypeSessionStart).Decode(&gs)

	sessions := t.TempDir()
	size := func() (int, int) { return 80, 24 }
	hostIn, hostKeys := io.Pipe()
	guestIn, guestKeys := io.Pipe()
	hostOut, guestOut := &syncBuf{}, &syncBuf{}

	h := &Host{Common: Common{Conn: host, Events: host.Events, Data: host.Data, Start: hs,
		Term:     &termio.Terminal{In: hostIn, Out: hostOut, SizeFunc: size},
		SaveMode: config.SaveOutput, SessionsDir: sessions, Version: "test"},
		Command: []string{"/bin/sh"}, GuestInput: true}
	g := &Guest{Common: Common{Conn: guest, Events: guest.Events, Data: guest.Data, Start: gs,
		Term:     &termio.Terminal{In: guestIn, Out: guestOut, SizeFunc: size},
		SaveMode: config.SaveOff, Version: "test"}}

	hostRes, guestRes := make(chan Result, 1), make(chan Result, 1)
	go func() { hostRes <- h.Run(ctx) }()
	go func() { guestRes <- g.Run(ctx) }()

	// The host told the guest about the recording and the PTY size.
	waitFor(t, "recording notice at the guest", func() bool { return strings.Contains(guestOut.String(), "being recorded") })

	_, _ = guestKeys.Write([]byte("echo guest-$((40+2))\r"))
	waitFor(t, "guest command output at the guest", func() bool { return strings.Contains(guestOut.String(), "guest-42") })
	waitFor(t, "guest command output at the host", func() bool { return strings.Contains(hostOut.String(), "guest-42") })
	// The host's shell can't set the guest's clipboard: OSC 52 is dropped,
	// the text around it arrives.
	_, _ = guestKeys.Write([]byte(`printf 'clip-\033]52;c;aGk=\007%s\n' ok` + "\r"))
	waitFor(t, "output around an OSC 52 at the guest", func() bool { return strings.Contains(guestOut.String(), "clip-ok") })
	if strings.Contains(guestOut.String(), "\x1b]52") {
		t.Fatal("OSC 52 reached the guest's terminal")
	}
	if !strings.Contains(hostOut.String(), "\x1b]52") {
		t.Fatal("the host's own terminal should still get the shell's output untouched")
	}
	if !strings.Contains(hostOut.String(), "is in your shell") {
		t.Fatalf("host status line missing: %q", hostOut.String())
	}

	// Keys the shell doesn't echo still show on the host's status line (rule 5).
	_, _ = guestKeys.Write([]byte("stty -echo; read x; stty echo; echo read-$((1+2))\r"))
	time.Sleep(200 * time.Millisecond)
	_, _ = guestKeys.Write([]byte("hush42\r"))
	waitFor(t, "the read to finish", func() bool { return strings.Contains(guestOut.String(), "read-3") })
	waitFor(t, "non-echoed keys on the host's status line", func() bool { return strings.Contains(hostOut.String(), "hush42⏎") })
	if strings.Contains(guestOut.String(), "hush42") {
		t.Fatal("the shell echoed the hidden input, so this test proves nothing")
	}

	// Hands off: guest typing no longer reaches the shell.
	_, _ = hostKeys.Write([]byte{termio.EscapeKey, 'h'})
	waitFor(t, "hands-off notice", func() bool { return strings.Contains(guestOut.String(), "Hands off") })
	_, _ = guestKeys.Write([]byte("echo leak-$((1+1))\r"))
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(hostOut.String(), "leak-2") {
		t.Fatal("view-only guest input reached the shell")
	}

	// Guest leaves with Ctrl-] q.
	_, _ = guestKeys.Write([]byte{termio.EscapeKey, 'q'})
	gr := <-guestRes
	if gr.Reason != ReasonYouEnded {
		t.Fatalf("guest reason %q", gr.Reason)
	}
	select {
	case hr := <-hostRes:
		if hr.Reason != "guest_left" {
			t.Fatalf("host reason %q", hr.Reason)
		}
		if hr.SavedTo == "" {
			t.Fatal("host should record by default")
		}
		cast, err := os.ReadFile(filepath.Join(hr.SavedTo, "session.cast"))
		if err != nil || !strings.Contains(string(cast), "guest-42") {
			t.Fatalf("recording missing output: %v", err)
		}
		if _, err := os.Stat(filepath.Join(hr.SavedTo, "keys.jsonl")); err == nil {
			t.Fatal("input must not be recorded by default")
		}
		var buf bytes.Buffer
		f, _ := os.Open(filepath.Join(hr.SavedTo, "session.cast"))
		defer f.Close()
		if _, err := record.Play(ctx, f, &buf, record.PlayOptions{Speed: 100, MaxIdle: time.Millisecond}); err != nil || !strings.Contains(buf.String(), "guest-42") {
			t.Fatalf("replay: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host session did not end")
	}
	_ = hostKeys.Close()
	_ = guestKeys.Close()
	if got := Explain(gr.Reason, protocol.RoleGuest, gs.Peer); got != "You left." {
		t.Fatalf("guest explanation %q", got)
	}
}
