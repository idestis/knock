package main

import (
	"strings"
	"testing"
	"time"
)

func TestDoorTTL(t *testing.T) {
	if d, err := doorTTL(""); err != nil || d != 24*time.Hour {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := doorTTL("2h"); err != nil || d != 2*time.Hour {
		t.Fatalf("2h: %v %v", d, err)
	}
	for _, bad := range []string{"soon", "10s", "-1h"} {
		if _, err := doorTTL(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestOpenStore(t *testing.T) {
	if s, err := openStore("", "", time.Hour); err != nil || s == nil {
		t.Fatalf("default: %v", err)
	}
	if _, err := openStore("valkey", "", time.Hour); err == nil {
		t.Fatal("valkey without a URL accepted")
	}
	if _, err := openStore("dynamodb", "", time.Hour); err == nil {
		t.Fatal("unknown store accepted")
	}
	// A URL that doesn't parse must not show up in the error: it may hold a password.
	const secretURL = "redis://:hunter2-secret@[::1"
	_, err := openStore("valkey", secretURL, time.Hour)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("bad URL: %v", err)
	}
}
