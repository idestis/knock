package termio

import (
	"reflect"
	"testing"
)

func TestEscaper(t *testing.T) {
	e := &Escaper{Commands: "qh"}
	got := e.Feed([]byte("ls\x1dq"))
	want := []Chunk{{Data: []byte("ls")}, {Cmd: 'q'}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	// Split across reads, upper case, literal Ctrl-], unknown key.
	if got := e.Feed([]byte("a\x1d")); !reflect.DeepEqual(got, []Chunk{{Data: []byte("a")}}) || !e.Armed() {
		t.Fatalf("got %+v armed=%v", got, e.Armed())
	}
	if got := e.Feed([]byte("H")); !reflect.DeepEqual(got, []Chunk{{Cmd: 'h'}}) {
		t.Fatalf("got %+v", got)
	}
	if got := e.Feed([]byte("\x1d\x1d\x1dx")); !reflect.DeepEqual(got, []Chunk{{Data: []byte{0x1d, 0x1d, 'x'}}}) {
		t.Fatalf("got %+v", got)
	}
}
