package state

import (
	"errors"
	"testing"
)

func TestParseShare(t *testing.T) {
	tests := []struct {
		args       []string
		door, code string
		err        error
	}{
		{[]string{"k7f3x9q2", "4821KQ"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"k7f3x9q2 4821KQ"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"k7f3x9q2-4821KQ"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"k7f3x9q24821KQ"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"K7F3X9Q2", "4821", "kq"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"k7f3x9q2", "4821-kq"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"knock k7f3x9q2 4821KQ"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"knock", "k7f3x9q2", "4821KQ"}, "k7f3x9q2", "4821KQ", nil},
		{[]string{"BVJY", "4821", "KQ"}, "bvjy", "4821KQ", nil},
		{[]string{"BVJY 4821 KQ"}, "bvjy", "4821KQ", nil},
		{[]string{"bvjy4821kq"}, "bvjy", "4821KQ", nil},
		{[]string{"bvjy"}, "bvjy", "", nil},
		{[]string{"k7f3x9q2"}, "k7f3x9q2", "", nil},
		{[]string{"maya01"}, "maya01", "", nil},
		{[]string{"maya01", "4821KQ"}, "maya01", "4821KQ", nil},
		{[]string{"maya-team-4821KQ"}, "mayateam", "4821KQ", nil},
		{nil, "", "", ErrNoDoor},
		{[]string{"k7f3x9q2", "48210K"}, "", "", ErrBadCode}, // O is not a code letter
		{[]string{"k7f3x9q2", "4821"}, "", "", ErrBadCode},
		{[]string{"k7f3x9q2", "ABCDEF"}, "", "", ErrBadCode},
		{[]string{"k7f3x9q2!", "4821KQ"}, "", "", ErrBadDoor},
		{[]string{"ab"}, "", "", ErrBadDoor},
	}
	for _, tt := range tests {
		door, code, err := ParseShare(tt.args)
		if !errors.Is(err, tt.err) {
			t.Errorf("ParseShare(%q) err = %v, want %v", tt.args, err, tt.err)
			continue
		}
		if door != tt.door || code != tt.code {
			t.Errorf("ParseShare(%q) = %q, %q; want %q, %q", tt.args, door, code, tt.door, tt.code)
		}
	}
}

func TestDisplayCode(t *testing.T) {
	if got := DisplayCode("4821kq"); got != "4821 KQ" {
		t.Fatalf("got %q", got)
	}
}

func TestNewDoorID(t *testing.T) {
	id := NewDoorID()
	if len(id) != DoorIDLen || !doorIDRe.MatchString(id) {
		t.Fatalf("bad door id %q", id)
	}
	if _, _, err := ParseShare([]string{id + strip("4821 KQ")}); err != nil {
		t.Fatalf("generated door id does not round-trip: %v", err)
	}
}

func strip(s string) string { return stripSpaces.Replace(s) }
