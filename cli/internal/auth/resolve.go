package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/idestis/knock/cli/internal/state"
)

// Resolved is the sign-in to use for a connection.
type Resolved struct {
	Tokens  Tokens
	Signed  bool   // false means connect anonymously
	Warning string // something the user should know, or ""
}

// Resolve loads the Keychain sign-in for this relay and refreshes it when
// it is close to expiry. Only the sign-in this relay issued is ever sent to
// it. Sign-in is optional: every failure falls back to anonymous with a
// warning instead of an error.
func Resolve(ctx context.Context, hc *http.Client, relay string, now time.Time) Resolved {
	origin, err := state.Origin(relay)
	if err != nil {
		return Resolved{Warning: "Couldn't tell which relay this is, so you're anonymous for now."}
	}
	t, ok, err := Load(origin)
	if err != nil {
		return Resolved{Warning: "Couldn't read your sign-in from the Keychain, so you're anonymous for now."}
	}
	if !ok {
		return Resolved{}
	}
	if !t.NeedsRefresh(now) {
		return Resolved{Tokens: t, Signed: true}
	}
	return refreshStored(ctx, hc, relay, t, now)
}

// ForceRefresh is used when the relay rejected an access token at hello.
func ForceRefresh(ctx context.Context, hc *http.Client, relay string, t Tokens, now time.Time) Resolved {
	t.ExpiresAt = 0
	return refreshStored(ctx, hc, relay, t, now)
}

func refreshStored(ctx context.Context, hc *http.Client, relay string, t Tokens, now time.Time) Resolved {
	origin, err := state.Origin(relay)
	if err != nil {
		return Resolved{Warning: "Couldn't tell which relay this is, so you're anonymous for now."}
	}
	expired := "Your sign-in expired, so you're anonymous for now. Run knock login to sign in again."
	if t.RefreshUntil != 0 && now.Unix() >= t.RefreshUntil {
		_ = Delete(origin)
		return Resolved{Warning: expired}
	}
	nt, err := Refresh(ctx, hc, relay, t.RefreshToken)
	switch {
	case errors.Is(err, ErrRejected):
		_ = Delete(origin)
		return Resolved{Warning: expired}
	case err != nil:
		if time.Unix(t.ExpiresAt, 0).After(now) {
			return Resolved{Tokens: t, Signed: true} // still valid; try again next time
		}
		return Resolved{Warning: "Couldn't refresh your sign-in, so you're anonymous for now."}
	}
	if err := Save(origin, nt); err != nil {
		return Resolved{Tokens: nt, Signed: true, Warning: "Couldn't save your refreshed sign-in to the Keychain."}
	}
	return Resolved{Tokens: nt, Signed: true}
}
