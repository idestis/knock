package record

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idestis/knock/cli/internal/state"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func lines(t *testing.T, b []byte) [][]any {
	t.Helper()
	var out [][]any
	for i, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if i == 0 {
			continue
		}
		var ev []any
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("line %d is not JSON: %q", i, l)
		}
		out = append(out, ev)
	}
	return out
}

func TestCastWriter(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var buf bytes.Buffer
	cw, err := NewCastWriter(&buf, Header{Width: 80, Height: 23, Title: "t"}, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	cw.Output([]byte("hello\r\n"))
	clk.t = clk.t.Add(1500 * time.Millisecond)
	euro := []byte("€") // three bytes, split across two chunks
	cw.Output(append([]byte("price "), euro[:1]...))
	cw.Output(euro[1:])
	clk.t = clk.t.Add(250 * time.Millisecond)
	cw.Resize(100, 30)
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}

	first, _, _ := strings.Cut(buf.String(), "\n")
	var h Header
	if err := json.Unmarshal([]byte(first), &h); err != nil || h.Version != 2 || h.Width != 80 || h.Height != 23 || h.Timestamp != 1_700_000_000 {
		t.Fatalf("bad header %q", first)
	}
	evs := lines(t, buf.Bytes())
	want := [][]any{{0.0, "o", "hello\r\n"}, {1.5, "o", "price "}, {1.5, "o", "€"}, {1.75, "r", "100x30"}}
	if len(evs) != len(want) {
		t.Fatalf("got %d events: %v", len(evs), evs)
	}
	for i := range want {
		for j := range want[i] {
			if evs[i][j] != want[i][j] {
				t.Fatalf("event %d = %v, want %v", i, evs[i], want[i])
			}
		}
	}
}

func TestCastWriterInvalidTail(t *testing.T) {
	var buf bytes.Buffer
	cw, _ := NewCastWriter(&buf, Header{Width: 1, Height: 1}, nil)
	cw.Output([]byte{'a', 0xE2}) // never completed
	_ = cw.Close()
	evs := lines(t, buf.Bytes())
	if len(evs) != 2 || evs[0][2] != "a" || evs[1][2] != "�" {
		t.Fatalf("unexpected events %v", evs)
	}
}

func TestPlayRoundTrip(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	cw, _ := NewCastWriter(&buf, Header{Width: 80, Height: 24}, clk.now)
	cw.Output([]byte("one "))
	clk.t = clk.t.Add(10 * time.Second)
	cw.Output([]byte("two"))
	_ = cw.Close()

	var out bytes.Buffer
	var slept []time.Duration
	_, err := Play(context.Background(), &buf, &out, PlayOptions{Speed: 2, MaxIdle: 3 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "one two" {
		t.Fatalf("played %q", out.String())
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("slept %v, want one 3s pause (10s at 2x, capped)", slept)
	}
}

func TestSessionFilesAndCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	old := time.Now().Add(-40 * 24 * time.Hour)
	for i, when := range []time.Time{old, time.Now()} {
		s, err := Start(root, Meta{SessionID: "s_abcdef" + string(rune('0'+i)), Role: "host", Peer: Person{Name: "Sam Carter"},
			StartedAt: state.NewTime(when.Add(time.Duration(i) * time.Second))}, i == 1)
		if err != nil {
			t.Fatal(err)
		}
		s.Cast.Output([]byte("x"))
		s.Input("guest", []byte("ls\r"))
		if err := s.Finish("host_ended"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(filepath.Base(s.Dir), "_sam-carter_abcd") {
			t.Fatalf("bad folder name %s", s.Dir)
		}
		for _, f := range []string{"", "meta.json", "session.cast"} {
			fi, err := os.Stat(filepath.Join(s.Dir, f))
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(state.FileMode)
			if f == "" {
				want = state.DirMode
			}
			if fi.Mode().Perm() != want {
				t.Fatalf("%s mode %v, want %v", f, fi.Mode().Perm(), want)
			}
		}
		_, keysErr := os.Stat(filepath.Join(s.Dir, "keys.jsonl"))
		if (i == 1) != (keysErr == nil) {
			t.Fatalf("keys.jsonl present=%v, want %v", keysErr == nil, i == 1)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "not-ours"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := Cleanup(root, 30, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("cleanup removed %d (%v), want 1", n, err)
	}
	left, _ := List(root)
	if len(left) != 1 {
		t.Fatalf("%d sessions left", len(left))
	}
	if _, err := os.Stat(filepath.Join(root, "not-ours")); err != nil {
		t.Fatal("cleanup removed a folder it did not make")
	}
	if e, err := Find(root, "1"); err != nil || e.Name != left[0].Name {
		t.Fatalf("find by number: %v", err)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Sam Carter": "sam-carter", "  ": "guest", "Zoë O'Neil!": "zo-o-neil"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
