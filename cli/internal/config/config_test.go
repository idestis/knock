package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRelayPrecedence(t *testing.T) {
	file := writeConfig(t, "[relay]\nurl = \"https://file.example\" # from file\n")
	missing := filepath.Join(t.TempDir(), "none.toml")
	withEnv := env(map[string]string{"KNOCK_RELAY": "https://env.example/"})
	noEnv := env(nil)

	tests := []struct {
		name       string
		in         Inputs
		want, from string
	}{
		{"flag wins", Inputs{Flag: "https://flag.example", Env: withEnv, File: file}, "https://flag.example", "--relay flag"},
		{"env beats file", Inputs{Env: withEnv, File: file}, "https://env.example", "KNOCK_RELAY"},
		{"file beats default", Inputs{Env: noEnv, File: file}, "https://file.example", "config.toml"},
		{"default", Inputs{Env: noEnv, File: missing}, DefaultRelay, "default"},
	}
	for _, tt := range tests {
		c, err := Load(tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if c.Relay != tt.want || c.RelaySource != tt.from {
			t.Errorf("%s: got %s (%s), want %s (%s)", tt.name, c.Relay, c.RelaySource, tt.want, tt.from)
		}
	}
}

func TestLoadAllKeys(t *testing.T) {
	file := writeConfig(t, `
# Knock settings
[relay]
url = "https://relay.example"

[sessions]
save = "output+input"
dir = "/tmp/knock-rec"
retention_days = 7

[guest]
input = 'view-only'
verified_only = true

[knock]
notch = false
timeout_seconds = 30

[door]
code_max_age_days = 14

[shell]
command = "/bin/zsh -l"

[extra]
thing = 1
`)
	c, err := Load(Inputs{Env: env(nil), File: file})
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionsSave != SaveOutputInput || c.SessionsDir != "/tmp/knock-rec" || c.RetentionDays != 7 ||
		c.GuestInput != "view-only" || !c.GuestVerifiedOnly || c.KnockNotch || c.KnockTimeout != 30 || c.ShellCommand != "/bin/zsh -l" ||
		c.CodeMaxAgeDays != 14 {
		t.Fatalf("unexpected config %+v", c)
	}
	if len(c.UnknownKeys) != 1 || c.UnknownKeys[0] != "extra.thing" {
		t.Fatalf("unknown keys %v", c.UnknownKeys)
	}
}

func TestDefaultsAndRoles(t *testing.T) {
	c, err := Load(Inputs{Env: env(nil), File: filepath.Join(t.TempDir(), "none.toml")})
	if err != nil {
		t.Fatal(err)
	}
	if c.SaveMode("host") != SaveOutput || c.SaveMode("guest") != SaveOff {
		t.Fatalf("role defaults wrong: %q %q", c.SaveMode("host"), c.SaveMode("guest"))
	}
	if c.RetentionDays != 30 || c.GuestInput != "allow" || c.CodeMaxAgeDays != 7 || c.GuestVerifiedOnly || !strings.HasSuffix(c.SessionsDir, filepath.Join(".knock", "sessions")) {
		t.Fatalf("defaults wrong %+v", c)
	}
}

func TestBadValues(t *testing.T) {
	for _, body := range []string{
		"[sessions]\nsave = \"everything\"\n",
		"[guest]\ninput = \"maybe\"\n",
		"[guest]\nverified_only = \"yes\"\n",
		"[relay]\nurl = \"relay.example\"\n",
		"[relay]\nurl = \"https://\"\n",
		"[door]\ncode_max_age_days = -1\n",
		"[door]\ncode_max_age_days = \"week\"\n",
		"[relay]\nurl = \"unterminated\n",
		"just words\n",
	} {
		if _, err := Load(Inputs{Env: env(nil), File: writeConfig(t, body)}); err == nil {
			t.Errorf("expected an error for %q", body)
		}
	}
}
