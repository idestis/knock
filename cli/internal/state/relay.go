package state

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Origin reduces a relay URL to scheme://host[:port] (docs/LOCAL_STATE.md,
// "Per-relay state"): lower case, the default port (80 for http, 443 for
// https) dropped, no user info, path, query, fragment or trailing slash.
// IPv6 hosts keep their brackets: "http://[::1]:8787".
//
// The origin keys everything that is a secret for one relay: door.json,
// recent.json and the Keychain sign-in. Knock.app computes the same string.
func Origin(raw string) (string, error) {
	bad := fmt.Errorf("the relay address %q needs to look like https://relay.example.com", raw)
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", bad
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("the relay address %q needs to start with https:// or http://", raw)
	}
	// Plain ASCII hosts only, as Knock.app does: no IDN, no percent
	// escapes (so no IPv6 zones), nothing that could leave the folder.
	if strings.ContainsAny(u.Host, "%") {
		return "", bad
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", bad
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':') {
			return "", bad
		}
	}
	port := u.Port() // "https://host:" has an empty port, which means the default
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		for _, r := range port {
			if r < '0' || r > '9' {
				return "", bad
			}
		}
		port = strings.TrimLeft(port, "0")
		if port == "" {
			return "", bad
		}
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		return scheme + "://[" + host + "]", nil
	}
	return scheme + "://" + host, nil
}

// FolderName is the folder for an origin under ~/.knock/relays: "://" and
// ":" become "_", and an IPv6 address loses its brackets with its own ":"
// written as "-". "http://[::1]:8787" is "http_--1_8787".
func FolderName(origin string) string {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok {
		return strings.ReplaceAll(origin, ":", "_")
	}
	host, port := rest, ""
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			host = rest[1:]
		} else {
			host, port = rest[1:end], strings.TrimPrefix(rest[end+1:], ":")
		}
		host = strings.ReplaceAll(host, ":", "-")
	} else if i := strings.LastIndex(rest, ":"); i >= 0 {
		host, port = rest[:i], rest[i+1:]
	}
	name := scheme + "_" + host
	if port != "" {
		name += "_" + port
	}
	// The origin came from Origin, so this is belt and braces: nothing in
	// the name may leave the relays folder.
	return strings.NewReplacer("/", "-", "\\", "-").Replace(name)
}

// Store is the per-relay folder ~/.knock/relays/<relay>/ that holds this
// device's door and Recent list for one relay.
type Store struct {
	Origin string // scheme://host[:port]
	Dir    string // ~/.knock/relays/<relay>
}

// ForRelay returns the store for the relay at relayURL.
func ForRelay(relayURL string) (Store, error) {
	origin, err := Origin(relayURL)
	if err != nil {
		return Store{}, err
	}
	base, err := Dir()
	if err != nil {
		return Store{}, err
	}
	return Store{Origin: origin, Dir: filepath.Join(base, "relays", FolderName(origin))}, nil
}

// path joins name onto the store's folder.
func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

// migrate moves a file from an older build (~/.knock/<name>) into this
// relay's folder when the relay has no file of that kind yet. It never
// overwrites: if Knock.app moved it first, or the relay already has one,
// there is nothing to do. It reports whether this call did the move.
func (s Store) migrate(name string) (bool, error) {
	dst := s.path(name)
	if _, err := os.Lstat(dst); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	src, err := Path(name)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(src); err != nil {
		return false, nil
	}
	if err := EnsureDir(filepath.Dir(s.Dir)); err != nil {
		return false, err
	}
	if err := EnsureDir(s.Dir); err != nil {
		return false, err
	}
	// A hard link and then removing the old name is a rename that fails
	// instead of replacing a file another client put there meanwhile.
	switch err := os.Link(src, dst); {
	case err == nil:
		if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
		return true, nil
	case errors.Is(err, os.ErrExist), errors.Is(err, os.ErrNotExist):
		return false, nil // someone else won, or there's nothing left to move
	}
	// No hard links on this volume: fall back to a plain rename after
	// checking again that the relay still has no file.
	if _, err := os.Lstat(dst); err == nil {
		return false, nil
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
