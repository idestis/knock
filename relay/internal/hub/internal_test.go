package hub

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	proxies, err := ParseTrustedProxies(" 10.0.0.0/8, 192.168.1.7 ,fd00::/8")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		trusted bool
		remote  string
		xff     []string
		want    string
	}{
		{"no proxies trusted: header ignored", false, "203.0.113.9:5555", []string{"1.2.3.4"}, "203.0.113.9"},
		{"untrusted socket: header ignored", true, "203.0.113.9:5555", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted proxy, no header", true, "10.1.2.3:5555", nil, "10.1.2.3"},
		{"trusted proxy appends the client", true, "10.1.2.3:5555", []string{"1.2.3.4, 198.51.100.7"}, "198.51.100.7"},
		{"forged hops left of the client are skipped", true, "10.1.2.3:5555", []string{"9.9.9.9", "1.2.3.4, 198.51.100.7, 10.9.9.9"}, "198.51.100.7"},
		{"single trusted address", true, "192.168.1.7:5555", []string{"198.51.100.7"}, "198.51.100.7"},
		{"every hop trusted: leftmost", true, "10.1.2.3:5555", []string{"10.4.4.4, 10.5.5.5"}, "10.4.4.4"},
		{"garbage stops the walk", true, "10.1.2.3:5555", []string{"1.2.3.4, nonsense, 10.5.5.5"}, "10.5.5.5"},
		{"hop with port", true, "10.1.2.3:5555", []string{"198.51.100.7:4000"}, "198.51.100.7"},
		{"ipv6", true, "[fd00::1]:5555", []string{"2001:db8::1"}, "2001:db8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hub{}
			if tc.trusted {
				h.cfg.TrustedProxies = proxies
			}
			r := httptest.NewRequest("GET", "/v1/ws", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := h.clientIP(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := ParseTrustedProxies("10.0.0.0/8,not-an-ip"); err == nil {
		t.Fatal("bad entry accepted")
	}
	if p, err := ParseTrustedProxies(""); err != nil || len(p) != 0 {
		t.Fatalf("empty: %v %v", p, err)
	}
}

func testClient(n int) *client {
	ctx, cancel := context.WithCancel(context.Background())
	return &client{ctx: ctx, cancel: cancel, out: make(chan frame, n), drained: make(chan struct{}, 1), watching: map[string]*door{}}
}

// A congested host loses knock notices, not its connection.
func TestSendSoftShedsInsteadOfDropping(t *testing.T) {
	c := testClient(4)
	c.send("ok", "", nil)
	c.send("ok", "", nil)
	if !c.congested() {
		t.Fatal("half-full queue should count as congested")
	}
	c.sendSoft("knock.cancelled", "", nil)
	if len(c.out) != 2 || c.ctx.Err() != nil {
		t.Fatalf("sendSoft should shed: len %d, err %v", len(c.out), c.ctx.Err())
	}
}

// Session bytes queued for one client stay within maxQueuedBytes; the
// sender waits until the peer drains.
func TestForwardByteBudget(t *testing.T) {
	h := &Hub{}
	host, guest := testClient(1024), testClient(1024)
	s := &session{host: host, guest: guest, guestInput: true}
	host.session, guest.session = s, s

	chunk := make([]byte, 1<<20)
	for range 4 {
		h.forward(host, chunk)
	}
	if q := guest.queued.Load(); q != 4<<20 {
		t.Fatalf("queued %d", q)
	}
	done := make(chan struct{})
	go func() {
		h.forward(host, chunk)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("forward went over the byte budget")
	case <-time.After(200 * time.Millisecond):
	}
	if len(guest.out) != 4 {
		t.Fatalf("queue grew to %d frames", len(guest.out))
	}
	guest.sent(<-guest.out) // the write loop sends one
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forward didn't resume after the peer drained")
	}
	if len(guest.out) != 4 || guest.queued.Load() != 4<<20 || guest.ctx.Err() != nil {
		t.Fatalf("after drain: %d frames, %d bytes, %v", len(guest.out), guest.queued.Load(), guest.ctx.Err())
	}

	// A frame that never makes it into the queue gives its bytes back.
	full := testClient(1)
	full.send("ok", "", nil)
	s.guest, full.session = full, s
	full.cancel()
	h.forward(host, make([]byte, 10))
	if q := full.queued.Load(); q != 0 {
		t.Fatalf("bytes leaked: %d", q)
	}
}
