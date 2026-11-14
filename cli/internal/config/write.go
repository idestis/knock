package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Settable lists the keys `knock config set` accepts, with a short hint.
var Settable = map[string]string{
	"relay.url":               "https://knock.example.ts.net",
	"sessions.save":           "off, output or output+input",
	"sessions.dir":            "~/.knock/sessions",
	"sessions.retention_days": "30",
	"guest.input":             "allow or view-only",
	"guest.verified_only":     "true or false",
	"knock.timeout_seconds":   "60",
	"knock.notch":             "true or false (Knock.app only)",
	"shell.command":           "/bin/zsh -l",
}

// Validate checks a value for key and returns it normalised.
func Validate(key, value string) (string, error) {
	hint, ok := Settable[key]
	if !ok {
		return "", fmt.Errorf("I don't know the setting %q. Run knock config to see them", key)
	}
	v := strings.TrimSpace(value)
	bad := fmt.Errorf("That doesn't work for %s. Use %s", key, hint)
	switch key {
	case "relay.url":
		v = strings.TrimRight(v, "/")
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return "", errors.New("The relay address needs to start with https:// or http://")
		}
	case "sessions.save":
		if v != SaveOff && v != SaveOutput && v != SaveOutputInput {
			return "", bad
		}
	case "guest.input":
		if v != "allow" && v != "view-only" {
			return "", bad
		}
	case "knock.notch", "guest.verified_only":
		if v != "true" && v != "false" {
			return "", bad
		}
	case "sessions.retention_days", "knock.timeout_seconds":
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || (key == "knock.timeout_seconds" && n == 0) {
			return "", fmt.Errorf("That doesn't work for %s. Use a whole number", key)
		}
	case "sessions.dir", "shell.command":
		if v == "" {
			return "", fmt.Errorf("Give %s a value, or use knock config unset", key)
		}
	}
	return v, nil
}

// Set writes key = value into the TOML file at path, under its [table],
// keeping every other line (and comment) as it was. An empty value removes the key.
func Set(path, key, value string) error {
	table, name, ok := strings.Cut(key, ".")
	if !ok {
		return fmt.Errorf("bad key %q", key)
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := []string{}
	if len(data) > 0 {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	entry := name + " = " + tomlValue(key, value)

	current, header, keyAt := "", -1, -1
	end := len(lines) // end of our table's block
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			if current == table && header >= 0 {
				end = i
				break
			}
			current = strings.TrimSpace(l[1 : len(l)-1])
			if current == table {
				header = i
			}
			continue
		}
		if current == table {
			k, _, found := strings.Cut(l, "=")
			if found && strings.TrimSpace(k) == name {
				keyAt = i
			}
		}
	}

	switch {
	case keyAt >= 0 && value == "":
		lines = append(lines[:keyAt], lines[keyAt+1:]...)
		lines = dropEmptyTable(lines, header)
	case keyAt >= 0:
		lines[keyAt] = entry
	case value == "":
		return nil
	case header >= 0:
		// insert after the last non-blank line of the table
		at := end
		for at > header+1 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		lines = append(lines[:at], append([]string{entry}, lines[at:]...)...)
	default:
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "["+table+"]", entry)
	}

	if strings.TrimSpace(strings.Join(lines, "")) == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func tomlValue(key, v string) string {
	switch key {
	case "sessions.retention_days", "knock.timeout_seconds", "knock.notch", "guest.verified_only":
		return v
	}
	return strconv.Quote(v)
}

// dropEmptyTable removes the [table] header at index h when nothing but
// blank lines follow it before the next table.
func dropEmptyTable(lines []string, h int) []string {
	if h < 0 {
		return lines
	}
	for i := h + 1; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "[") {
			break
		}
		if l != "" {
			return lines
		}
	}
	start := h
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	end := h + 1
	for end < len(lines) && strings.TrimSpace(lines[end]) == "" {
		end++
	}
	return append(lines[:start], lines[end:]...)
}
