package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"time"

	"github.com/idestis/knock/cli/internal/auth"
	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/ui"
)

var providerLabels = map[string]string{"google": "Google", "github": "GitHub"}

func runLogin(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: knock login [google|github]")
	}
	offered, err := auth.Providers(ctx, httpClient, cfg.Relay)
	if err != nil {
		return err
	}
	providers := offered.Providers
	if len(providers) == 0 {
		ui.Say("This relay has no sign-in set up. You can still use Knock without signing in.")
		return nil
	}
	var provider string
	switch {
	case len(args) == 1:
		provider = args[0]
		if !slices.Contains(providers, provider) {
			return fmt.Errorf("This relay doesn't offer %s sign-in. It offers: %v", provider, providers)
		}
	case len(providers) == 1:
		provider = providers[0]
	default:
		choices := make([]ui.Choice, 0, len(providers))
		for _, p := range providers {
			label := providerLabels[p]
			if label == "" {
				label = p
			}
			choices = append(choices, ui.Choice{Key: p, Label: label, Hint: "Sign in with your " + label + " account"})
		}
		provider, err = ui.Pick("How do you want to sign in?", choices)
		if err != nil {
			return err
		}
	}

	label := providerLabels[provider]
	if label == "" {
		label = provider
	}
	tok, err := auth.Login(ctx, auth.LoginOptions{Relay: cfg.Relay, Provider: provider, HTTP: httpClient,
		Open: func(url string) error {
			ui.Say("Opening your browser to sign in with %s…", label)
			ui.Note("If it didn't open, go to: %s", url)
			_ = openBrowser(url)
			return nil
		}})
	if err != nil {
		return err
	}
	if err := auth.Save(tok); err != nil {
		return err
	}
	who := tok.User.Name
	if d := auth.Describe(tok.User); d != "" {
		who += " (" + d + ")"
	}
	ui.Say("%s Signed in as %s with %s.", ui.Good.Render("✓"), ui.Bold.Render(who), label)
	return nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func runLogout(cfg config.Config) error {
	_, ok, _ := auth.Load()
	if err := auth.Delete(); err != nil {
		return err
	}
	if !ok {
		ui.Say("You weren't signed in.")
		return nil
	}
	ui.Say("Signed out. Knock will use %s %s from now on.", ui.Bold.Render(anonName()), ui.Warn.Render("(not verified)"))
	return nil
}

func runWhoami(cfg config.Config) error {
	t, ok, err := auth.Load()
	if err != nil {
		return err
	}
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if p, err := auth.Providers(ctx, httpClient, cfg.Relay); err == nil && p.SignInRequired {
			ui.Say("You're not signed in, and this relay needs you to sign in. Run knock login.")
			ui.Note("Relay: %s", cfg.Relay)
			return nil
		}
		ui.Say("You're not signed in. People see you as %s %s.", ui.Bold.Render(anonName()), ui.Warn.Render("(not verified)"))
		ui.Note("Run knock login to show a verified name. Relay: %s", cfg.Relay)
		return nil
	}
	u := t.User
	label := providerLabels[u.Provider]
	if label == "" {
		label = u.Provider
	}
	line := "Signed in as " + ui.Bold.Render(u.Name)
	if d := auth.Describe(u); d != "" {
		line += " (" + d + ")"
	}
	ui.Say("%s with %s. %s", line, label, ui.Good.Render("✓ verified"))
	until := time.Unix(t.RefreshUntil, 0)
	ui.Note("Device %s · relay %s · sign-in lasts until %s unless you use Knock before then", deviceName(), cfg.Relay, until.Format("2 Jan 2006"))
	return nil
}
