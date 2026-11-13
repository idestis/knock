package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/idestis/knock/cli/internal/auth"
	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

func protocolVersion() int { return protocol.Version }

var httpClient = &http.Client{Timeout: 15 * time.Second}

// connect dials the relay, signed in when possible and anonymous otherwise.
func connect(ctx context.Context, cfg config.Config) (*client.Client, error) {
	r := auth.Resolve(ctx, httpClient, cfg.Relay, time.Now())
	if r.Warning != "" {
		ui.Warning("%s", r.Warning)
	}
	c, err := dial(ctx, cfg, r)
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.Code == protocol.ErrUnauthorised && r.Signed {
		r = auth.ForceRefresh(ctx, httpClient, cfg.Relay, r.Tokens, time.Now())
		if r.Warning != "" {
			ui.Warning("%s", r.Warning)
		}
		c, err = dial(ctx, cfg, r)
	}
	if err == nil {
		return c, nil
	}
	if errors.As(err, &perr) {
		switch perr.Code {
		case protocol.ErrAnonForbidden:
			return nil, errors.New("This relay needs you to sign in. Run knock login first.")
		case protocol.ErrUnauthorised:
			return nil, errors.New("The relay didn't accept your sign-in. Run knock login to sign in again.")
		}
		return nil, fmt.Errorf("The relay said no: %s", perr.Message)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("Can't reach the relay at %s. Check the address with knock config, or set KNOCK_RELAY", cfg.Relay)
}

func dial(ctx context.Context, cfg config.Config, r auth.Resolved) (*client.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h := protocol.Hello{Name: anonName(), Device: deviceName(), Client: protocol.ClientCLI, Version: Version}
	if r.Signed {
		h.Token = r.Tokens.AccessToken
	}
	return client.Dial(dctx, cfg.Relay, h)
}

// anonName is the name sent when not signed in: git's user.name, then $USER.
func anonName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "config", "user.name").Output(); err == nil {
		if n := strings.TrimSpace(string(out)); n != "" {
			return n
		}
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "Guest"
}

// deviceName is this machine's short host name, like "maya-mbp".
func deviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	h = strings.TrimSuffix(h, ".local")
	if i := strings.Index(h, "."); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// whoLine describes the identity the relay gave us.
func whoLine(id protocol.Identity) string {
	if id.Verified {
		s := "Signed in as " + ui.Bold.Render(id.Name)
		if id.Email != "" {
			s += ui.Dim.Render(" (" + id.Email + ")")
		}
		return s + "."
	}
	return "You're " + ui.Bold.Render(id.Name) + ui.Warn.Render(" (not verified)") + ui.Dim.Render(". Run knock login to show a verified name.")
}

// relayError turns a relay error into what the user should read.
func relayError(err error) error {
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.Message != "" {
		return errors.New(perr.Message)
	}
	return err
}
