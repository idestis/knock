// Package app wires the knock commands together.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/record"
	"github.com/idestis/knock/cli/internal/ui"
)

// Version is set at build time with -ldflags "-X github.com/idestis/knock/cli/internal/app.Version=v1.2.3".
var Version = "dev"

var helpSections = []ui.HelpSection{
	{Title: "Host", Rows: [][2]string{
		{"knock open [--tmux] [--view-only]", "Open your door and wait for a knock"},
		{"knock close", "Close your door, wherever it's open (app or terminal)"},
		{"knock copy", "Copy your code to the clipboard"},
		{"knock new-code", "Replace your code (cuts off everyone with the old one)"},
	}},
	{Title: "Guest", Rows: [][2]string{
		{"knock <code>", "Knock, like: knock BVJY 4821 KQ"},
		{"knock <door>", "Knock on a door from your recent list, no code needed"},
		{"knock join <code>", "The same, spelled out"},
		{"knock recent", "Doors you've been in, and whether they're open"},
		{"knock recent rm <door>", "Forget a door"},
	}},
	{Title: "Account (optional)", Rows: [][2]string{
		{"knock login [google|github]", "Sign in so people see a verified name"},
		{"knock logout", "Sign out"},
		{"knock whoami", "Show who the relay will see"},
	}},
	{Title: "Recordings", Rows: [][2]string{
		{"knock sessions ls", "List saved sessions"},
		{"knock sessions play <id>", "Replay one"},
	}},
	{Title: "Settings and updates", Rows: [][2]string{
		{"knock config", "Show the settings in effect"},
		{"knock config set <key> <value>", "Change a setting, like: knock config set relay.url https://…"},
		{"knock config unset <key>", "Go back to the default"},
		{"knock config path", "Print where the config file is"},
		{"knock update [--check]", "Update knock from GitHub Releases"},
		{"knock version", "Show the version"},
	}},
	{Title: "Global flags", Rows: [][2]string{
		{"--relay <url>", "Relay to use (also KNOCK_RELAY, or [relay] url in ~/.knock/config.toml)"},
	}},
}

const (
	helpTitle   = "Knock, knock. Who's there?"
	helpTagline = "knock shares one shell between two people, through a relay."
	helpFooter  = "During a session, press Ctrl-] then q to end it. The host can press Ctrl-] then h for Hands off."
)

// printHelp shows the banner and colours on a terminal, plain text otherwise.
func printHelp() {
	if term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == "" {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w >= 60 {
			ui.Print(ui.HelpScreen(helpTitle, helpTagline, helpSections, helpFooter))
			return
		}
	}
	fmt.Print(ui.HelpPlain(helpTagline, helpSections, helpFooter))
}

// Main runs the CLI and returns the exit code.
func Main(args []string) int {
	relayFlag, args, err := extractRelay(args)
	if err != nil {
		ui.Failure(err.Error())
		return 2
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printHelp()
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Printf("knock %s (protocol %d)\n", Version, protocolVersion())
		return 0
	}
	if args[0] == "update" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runUpdate(ctx, args[1:]); err != nil {
			ui.Failure(sentence(err.Error()))
			return 1
		}
		return 0
	}

	cfg, err := config.Load(config.Inputs{Flag: relayFlag})
	if err != nil {
		ui.Failure(err.Error())
		return 1
	}
	_, _ = record.Cleanup(cfg.SessionsDir, cfg.RetentionDays, time.Now())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "open":
		err = runOpen(ctx, cfg, rest)
	case "join":
		err = runJoin(ctx, cfg, rest)
	case "recent":
		err = runRecent(ctx, cfg, rest)
	case "new-code":
		err = runNewCode(cfg, rest)
	case "close":
		err = runClose(ctx, cfg, rest)
	case "copy":
		err = runCopy(rest)
	case "login":
		err = runLogin(ctx, cfg, rest)
	case "logout":
		err = runLogout(cfg)
	case "whoami":
		err = runWhoami(cfg)
	case "sessions":
		err = runSessions(ctx, cfg, rest)
	case "config":
		err = runConfig(cfg, rest)
	default:
		if strings.HasPrefix(cmd, "-") {
			err = fmt.Errorf("I don't know the flag %s. Run knock help to see what's there.", cmd)
			break
		}
		err = runJoin(ctx, cfg, args)
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, ui.ErrCancelled) || errors.Is(err, context.Canceled) {
		return 130
	}
	var ex exitError
	if errors.As(err, &ex) {
		return ex.code
	}
	ui.Failure(sentence(err.Error()))
	return 1
}

// exitError ends with a code after the message was already shown.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// extractRelay pulls --relay out of anywhere in args.
func extractRelay(args []string) (string, []string, error) {
	var relay string
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			out = append(out, args[i+1:]...)
			return relay, out, nil
		case a == "--relay" || a == "-relay":
			if i+1 >= len(args) {
				return "", nil, errors.New("--relay needs an address, like --relay https://relay.example.com")
			}
			relay = args[i+1]
			i++
		case strings.HasPrefix(a, "--relay="):
			relay = strings.TrimPrefix(a, "--relay=")
		default:
			out = append(out, a)
		}
	}
	return relay, out, nil
}

// sentence capitalises the first letter and ends with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}
