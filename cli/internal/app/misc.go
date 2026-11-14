package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/record"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/ui"
)

func runNewCode(cfg config.Config, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: knock new-code")
	}
	_, existed, err := state.LoadDoor()
	if err != nil {
		return err
	}
	d, err := state.LoadOrCreateDoor()
	if err != nil {
		return err
	}
	if existed {
		d.Rotate()
		if err := state.SaveDoor(d); err != nil {
			return fmt.Errorf("couldn't save your new code: %w", err)
		}
	}
	ui.Say("Your new code is %s. Anyone with the old code is cut off.", ui.CodeTag.Render(state.DisplayCode(d.Code)))
	ui.Say("Send them this: %s", ui.Brand.Render(d.ShareString()))
	ui.Note("If knock open is running, it switches to the new code within a few seconds.")
	return nil
}

func runConfig(cfg config.Config, args []string) error {
	if len(args) > 0 && args[0] == "path" {
		fmt.Println(cfg.Path)
		return nil
	}
	if len(args) > 0 && (args[0] == "set" || args[0] == "unset") {
		return runConfigSet(cfg, args)
	}
	if len(args) > 0 {
		return errors.New("usage: knock config [set <key> <value> | unset <key> | path]")
	}
	_, err := os.Stat(cfg.Path)
	file := cfg.Path
	if err != nil {
		file += ui.Dim.Render(" (not created yet, using defaults)")
	}
	save := cfg.SessionsSave
	if save == "" {
		save = "output on the host, off as a guest " + ui.Dim.Render("(default)")
	}
	shell := cfg.ShellCommand
	if shell == "" {
		shell = os.Getenv("SHELL") + " -l " + ui.Dim.Render("(your login shell)")
	}
	rows := [][]string{
		{"config file", file},
		{"relay.url", cfg.Relay + ui.Dim.Render("  from "+cfg.RelaySource)},
		{"sessions.save", save},
		{"sessions.dir", cfg.SessionsDir},
		{"sessions.retention_days", fmt.Sprint(cfg.RetentionDays)},
		{"guest.input", cfg.GuestInput},
		{"knock.timeout_seconds", fmt.Sprint(cfg.KnockTimeout)},
		{"knock.notch", fmt.Sprint(cfg.KnockNotch) + ui.Dim.Render("  (Knock.app only)")},
		{"shell.command", shell},
	}
	if cfg.AuthDomain != "" {
		rows = append(rows, []string{"auth.domain", cfg.AuthDomain})
	}
	for _, r := range rows {
		ui.Print(ui.Dim.Width(26).Render(r[0]) + r[1])
	}
	if len(cfg.UnknownKeys) > 0 {
		ui.Warning("Knock doesn't know these settings: %s", strings.Join(cfg.UnknownKeys, ", "))
	}
	return nil
}

func runSessions(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] == "ls" || args[0] == "list" {
		return sessionsList(cfg)
	}
	if args[0] != "play" {
		return fmt.Errorf("I don't know knock sessions %s. Try knock sessions ls or knock sessions play <id>", args[0])
	}
	fs := flag.NewFlagSet("play", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	speed := fs.Float64("speed", 1, "playback speed")
	idle := fs.Duration("idle", 2*time.Second, "shorten pauses longer than this (0 keeps them)")
	// Allow flags before or after the id.
	var ids []string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return errors.New("usage: knock sessions play [--speed 2] [--idle 2s] <id>")
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(ids) != 1 {
		return errors.New("usage: knock sessions play [--speed 2] [--idle 2s] <id>")
	}
	e, err := record.Find(cfg.SessionsDir, ids[0])
	if err != nil {
		return err
	}
	f, err := os.Open(e.Dir + "/session.cast")
	if err != nil {
		return fmt.Errorf("couldn't open the recording: %w", err)
	}
	defer f.Close()
	_, err = record.Play(ctx, f, os.Stdout, record.PlayOptions{Speed: *speed, MaxIdle: *idle})
	fmt.Print("\x1b[0m\r\n")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if err != nil {
		return err
	}
	ui.Note("End of %s.", e.Name)
	return nil
}

func sessionsList(cfg config.Config) error {
	all, err := record.List(cfg.SessionsDir)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		ui.Say("No saved sessions in %s yet.", cfg.SessionsDir)
		return nil
	}
	rows := make([][]string, 0, len(all))
	for i, e := range all {
		m := e.Meta
		who := m.Peer.Name
		if who != "" && !m.Peer.Verified {
			who += " (not verified)"
		}
		dur := ""
		if !m.EndedAt.IsZero() && !m.StartedAt.IsZero() {
			dur = m.EndedAt.Sub(m.StartedAt.Time).Round(time.Second).String()
		}
		started := ""
		if !m.StartedAt.IsZero() {
			started = m.StartedAt.Local().Format("2 Jan 15:04")
		}
		rows = append(rows, []string{fmt.Sprint(i + 1), e.Name, m.Role, who, started, dur, humanSize(e.Size)})
	}
	ui.Print(ui.Table([]string{"#", "ID", "ROLE", "WITH", "STARTED", "LENGTH", "SIZE"}, rows, nil))
	ui.Note("Replay one with knock sessions play <# or id>. Recordings older than %d days are deleted.", cfg.RetentionDays)
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func runConfigSet(cfg config.Config, args []string) error {
	keys := make([]string, 0, len(config.Settable))
	for k := range config.Settable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if args[0] == "unset" {
		if len(args) != 2 {
			return errors.New("usage: knock config unset <key>")
		}
		if _, ok := config.Settable[args[1]]; !ok {
			return fmt.Errorf("I don't know the setting %q. Settings: %s", args[1], strings.Join(keys, ", "))
		}
		if err := config.Set(cfg.Path, args[1], ""); err != nil {
			return err
		}
		ui.Say("%s is back to its default.", args[1])
		return nil
	}
	if len(args) < 3 {
		return fmt.Errorf("usage: knock config set <key> <value>. Settings: %s", strings.Join(keys, ", "))
	}
	v, err := config.Validate(args[1], strings.Join(args[2:], " "))
	if err != nil {
		return err
	}
	if err := config.Set(cfg.Path, args[1], v); err != nil {
		return err
	}
	ui.Say("Set %s to %s.", args[1], v)
	if args[1] == "relay.url" {
		ui.Note("Knock.app reads this too, unless you changed the relay in its Settings.")
	}
	return nil
}
