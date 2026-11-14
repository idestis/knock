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

// Keychain item shared with Knock.app (docs/LOCAL_STATE.md).
const (
	KeychainService = "io.github.idestis.knock"
	KeychainAccount = "session"
)

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

// Load returns the stored tokens; ok is false when signed out.
func Load() (t Tokens, ok bool, err error) {
	s, err := keyring.Get(KeychainService, KeychainAccount)
	if errors.Is(err, keyring.ErrNotFound) {
		return t, false, nil
	}
	if err != nil {
		return t, false, fmt.Errorf("read the Keychain: %w", err)
	}
	if err := json.Unmarshal([]byte(s), &t); err != nil || t.AccessToken == "" {
		return t, false, nil // unreadable entry counts as signed out
	}
	return t, true, nil
}

// Save stores tokens in the Keychain.
func Save(t Tokens) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := keyring.Set(KeychainService, KeychainAccount, string(b)); err != nil {
		return fmt.Errorf("save to the Keychain: %w", err)
	}
	return nil
}

// Delete removes the sign-in. Missing is fine.
func Delete() error {
	err := keyring.Delete(KeychainService, KeychainAccount)
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
