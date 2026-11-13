// Package config loads ~/.knock/config.toml and applies flag and
// environment overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/idestis/knock/cli/internal/state"
)

// DefaultRelay is used when nothing else names a relay.
const DefaultRelay = "http://127.0.0.1:8787"

// Recording modes for sessions.save.
const (
	SaveOff         = "off"
	SaveOutput      = "output"
	SaveOutputInput = "output+input"
)

// Config is the effective configuration.
type Config struct {
	Path        string // config file path
	Relay       string
	RelaySource string // flag, env, config, default

	AuthDomain    string
	SessionsSave  string // "" means the role default
	SessionsDir   string
	RetentionDays int
	GuestInput    string // allow or view-only
	KnockNotch    bool
	KnockTimeout  int
	ShellCommand  string
	UnknownKeys   []string
}

// Inputs are the sources Load combines. Zero values mean "not given".
type Inputs struct {
	Flag string              // --relay
	Env  func(string) string // os.Getenv
	File string              // path to config.toml; "" means ~/.knock/config.toml
}

var known = map[string]bool{
	"relay.url": true, "auth.domain": true, "sessions.save": true, "sessions.dir": true,
	"sessions.retention_days": true, "guest.input": true, "knock.notch": true,
	"knock.timeout_seconds": true, "shell.command": true,
}

// Load builds the effective config. Relay URL precedence:
// --relay flag > KNOCK_RELAY > config.toml [relay] url > DefaultRelay.
func Load(in Inputs) (Config, error) {
	if in.Env == nil {
		in.Env = os.Getenv
	}
	c := Config{SessionsDir: "", RetentionDays: 30, GuestInput: "allow", KnockNotch: true, KnockTimeout: 60}
	path := in.File
	if path == "" {
		p, err := state.Path("config.toml")
		if err != nil {
			return c, err
		}
		path = p
	}
	c.Path = path

	kv := map[string]string{}
	f, err := os.Open(path)
	switch {
	case err == nil:
		kv, err = parseTOML(f)
		f.Close()
		if err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return c, err
	}

	for k := range kv {
		if !known[k] {
			c.UnknownKeys = append(c.UnknownKeys, k)
		}
	}
	c.AuthDomain = kv["auth.domain"]
	c.ShellCommand = kv["shell.command"]
	if v, ok := kv["sessions.save"]; ok {
		switch v {
		case SaveOff, SaveOutput, SaveOutputInput:
			c.SessionsSave = v
		default:
			return c, fmt.Errorf("%s: sessions.save must be off, output or output+input", path)
		}
	}
	if v, ok := kv["sessions.dir"]; ok {
		c.SessionsDir = expandHome(v)
	}
	if v, ok := kv["sessions.retention_days"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("%s: sessions.retention_days must be a whole number of days", path)
		}
		c.RetentionDays = n
	}
	if v, ok := kv["guest.input"]; ok {
		if v != "allow" && v != "view-only" {
			return c, fmt.Errorf("%s: guest.input must be allow or view-only", path)
		}
		c.GuestInput = v
	}
	if v, ok := kv["knock.notch"]; ok {
		c.KnockNotch = v == "true"
	}
	if v, ok := kv["knock.timeout_seconds"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.KnockTimeout = n
		}
	}
	if c.SessionsDir == "" {
		d, err := state.Path("sessions")
		if err != nil {
			return c, err
		}
		c.SessionsDir = d
	}

	switch {
	case in.Flag != "":
		c.Relay, c.RelaySource = in.Flag, "--relay flag"
	case in.Env("KNOCK_RELAY") != "":
		c.Relay, c.RelaySource = in.Env("KNOCK_RELAY"), "KNOCK_RELAY"
	case kv["relay.url"] != "":
		c.Relay, c.RelaySource = kv["relay.url"], "config.toml"
	default:
		c.Relay, c.RelaySource = DefaultRelay, "default"
	}
	c.Relay = strings.TrimRight(c.Relay, "/")
	if !strings.HasPrefix(c.Relay, "http://") && !strings.HasPrefix(c.Relay, "https://") {
		return c, fmt.Errorf("the relay address %q needs to start with https:// or http://", c.Relay)
	}
	return c, nil
}

// SaveMode is the recording mode for a role ("host" or "guest").
func (c Config) SaveMode(role string) string {
	if c.SessionsSave != "" {
		return c.SessionsSave
	}
	if role == "host" {
		return SaveOutput
	}
	return SaveOff
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
