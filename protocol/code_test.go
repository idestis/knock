package protocol

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Same vector as mac/Tests/GoVector.swift: the app and the CLI must agree.
func TestHashVector(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got := hex.EncodeToString(HashCode("4821 kq", salt, 1000))
	if got != "5dd629380958b6f23fef568d187d912c98ba15aca0f7a78d95e419648e4a77a1" {
		t.Fatalf("hash vector changed: %s", got)
	}
}

func TestNewCode(t *testing.T) {
	for range 500 {
		c := NewCode()
		if len(c) != 6 || strings.ContainsAny(c, "IO") || strings.ContainsAny(c[:4], "ABCDEFGHJKLMNPQRSTUVWXYZ") {
			t.Fatalf("bad code %q", c)
		}
	}
}
