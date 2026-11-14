// Package auth keeps the relay sign-in in the Keychain, refreshes it, and
// runs the browser sign-in (loopback redirect with PKCE).
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idestis/knock/protocol"
)

// Keychain items shared with Knock.app (docs/LOCAL_STATE.md, "Sign-in
// tokens"): one per relay origin, account "session:<origin>". Older builds
// kept one sign-in under LegacyAccount for whatever relay was configured.
const (
	KeychainService = "io.github.idestis.knock"
	LegacyAccount   = "session"
)

// Account is the Keychain account for a relay origin (see state.Origin).
func Account(origin string) string { return "session:" + origin }

// RefreshWindow: refresh when the access token has less than this left.
const RefreshWindow = 5 * time.Minute

// Tokens is the relay's /v1/auth/token and /v1/auth/refresh body, stored
// as-is in the Keychain.
type Tokens struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token"`
	ExpiresAt    int64             `json:"expires_at"`
	RefreshUntil int64             `json:"refresh_expires_at"`
	User         protocol.Identity `json:"user"`
}

// ErrRejected means the relay refused the refresh token (HTTP 401).
var ErrRejected = errors.New("the relay no longer accepts this sign-in")

// Load returns the sign-in for the relay at origin; ok is false when
// signed out there. A sign-in from an older build moves to this relay
// first (once; whichever client runs first does it).
func Load(origin string) (t Tokens, ok bool, err error) {
	if origin == "" {
		return t, false, errors.New("no relay to load a sign-in for")
	}
	s, err := keyring.Get(KeychainService, Account(origin))
	if errors.Is(err, keyring.ErrNotFound) {
		s, err = migrateLegacy(origin)
		if s == "" && err == nil {
			return t, false, nil
		}
	}
	if err != nil {
		return t, false, fmt.Errorf("read the Keychain: %w", err)
	}
	if err := json.Unmarshal([]byte(s), &t); err != nil || t.AccessToken == "" {
		return t, false, nil // unreadable entry counts as signed out
	}
	return t, true, nil
}

// migrateLegacy moves the single sign-in older builds kept into origin's
// account and returns it, or "" when there is none.
func migrateLegacy(origin string) (string, error) {
	s, err := keyring.Get(KeychainService, LegacyAccount)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Knock.app may have moved (and since refreshed) it a moment ago:
	// its copy wins.
	if cur, err := keyring.Get(KeychainService, Account(origin)); err == nil {
		_ = keyring.Delete(KeychainService, LegacyAccount)
		return cur, nil
	}
	if err := keyring.Set(KeychainService, Account(origin), s); err != nil {
		return "", err
	}
	if err := keyring.Delete(KeychainService, LegacyAccount); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return "", err
	}
	return s, nil
}

// Save stores the sign-in for the relay at origin.
func Save(origin string, t Tokens) error {
	if origin == "" {
		return errors.New("no relay to save the sign-in for")
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := keyring.Set(KeychainService, Account(origin), string(b)); err != nil {
		return fmt.Errorf("save to the Keychain: %w", err)
	}
	return nil
}

// Delete removes the sign-in for the relay at origin. Missing is fine.
func Delete(origin string) error {
	if origin == "" {
		return nil
	}
	err := keyring.Delete(KeychainService, Account(origin))
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove from the Keychain: %w", err)
	}
	return nil
}

// NeedsRefresh reports whether the access token has under RefreshWindow left.
func (t Tokens) NeedsRefresh(now time.Time) bool {
	return time.Unix(t.ExpiresAt, 0).Sub(now) < RefreshWindow
}

// Refresh trades the refresh token for new tokens.
func Refresh(ctx context.Context, hc *http.Client, relay, refreshToken string) (Tokens, error) {
	var t Tokens
	status, err := postJSON(ctx, hc, relay+"/v1/auth/refresh", map[string]string{"refresh_token": refreshToken}, &t)
	if status == http.StatusUnauthorized {
		return t, ErrRejected
	}
	if err != nil {
		return t, err
	}
	if t.AccessToken == "" {
		return t, errors.New("the relay sent an empty sign-in")
	}
	return t, nil
}

// Exchange trades a one-time sign-in code for tokens (PKCE).
func Exchange(ctx context.Context, hc *http.Client, relay, code, verifier string) (Tokens, error) {
	var t Tokens
	status, err := postJSON(ctx, hc, relay+"/v1/auth/token", map[string]string{"code": code, "code_verifier": verifier}, &t)
	if status == http.StatusBadRequest {
		return t, errors.New("the relay didn't accept the sign-in. Try knock login again")
	}
	if err != nil {
		return t, err
	}
	return t, nil
}

// Providers asks the relay which sign-in options it offers and whether it
// requires sign-in.
func Providers(ctx context.Context, hc *http.Client, relay string) (protocol.AuthProviders, error) {
	var out protocol.AuthProviders
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relay+"/v1/auth/providers", nil)
	if err != nil {
		return out, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, unreachable(relay, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("the relay answered %s when asked how to sign in", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// Describe is how a signed-in identity reads in one line: the email, or
// the GitHub username when there is no verified email.
func Describe(id protocol.Identity) string {
	switch {
	case id.Email != "":
		return id.Email
	case id.Login != "":
		return "@" + id.Login
	}
	return ""
}

func postJSON(ctx context.Context, hc *http.Client, url string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, unreachable(url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("the relay answered %s", resp.Status)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out)
}

func unreachable(url string, err error) error {
	host := url
	if i := strings.Index(host, "/v1/"); i > 0 {
		host = host[:i]
	}
	return fmt.Errorf("can't reach the relay at %s: %w", host, err)
}
