package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetKeepsCommentsAndTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "# my settings\n[relay]\nurl = \"https://old.example\"  # keep me\n\n[sessions]\nsave = \"output\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := [][2]string{
		{"relay.url", "https://new.example"},
		{"sessions.retention_days", "14"},
		{"guest.input", "view-only"},
		{"door.code_max_age_days", "30"},
		{"sessions.save", ""}, // remove
	}
	for _, s := range steps {
		if err := Set(path, s[0], s[1]); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, want := range []string{"# my settings", `url = "https://new.example"`, "retention_days = 14", "[guest]", `input = "view-only"`, "[door]", "code_max_age_days = 30"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "save =") {
		t.Errorf("sessions.save should be gone:\n%s", got)
	}
	c, err := Load(Inputs{File: path, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if c.Relay != "https://new.example" || c.RetentionDays != 14 || c.GuestInput != "view-only" || c.CodeMaxAgeDays != 30 {
		t.Fatalf("round trip failed: %+v", c)
	}
}

func TestValidate(t *testing.T) {
	ok := map[string]string{"relay.url": "https://r.example/", "sessions.save": "output+input", "knock.notch": "false", "knock.timeout_seconds": "30",
		"door.code_max_age_days": "0"}
	for k, v := range ok {
		if _, err := Validate(k, v); err != nil {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
	bad := map[string]string{"relay.url": "ftp://x", "sessions.save": "all", "guest.input": "maybe", "knock.timeout_seconds": "0", "nope.key": "1",
		"door.code_max_age_days": "-3"}
	for k, v := range bad {
		if _, err := Validate(k, v); err == nil {
			t.Errorf("%s=%s should fail", k, v)
		}
	}
}

func TestUnsetDropsEmptyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = Set(path, "relay.url", "https://r.example")
	_ = Set(path, "guest.input", "allow")
	if err := Set(path, "relay.url", ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if got := string(b); strings.Contains(got, "[relay]") || !strings.Contains(got, "[guest]") {
		t.Fatalf("want only [guest] left, got:\n%s", got)
	}
}
