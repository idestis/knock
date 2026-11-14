package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/protocol"
)

func runRecent(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "rm", "remove", "forget":
			if len(args) != 2 {
				return errors.New("usage: knock recent rm <door>")
			}
			return recentRemove(cfg, args[1])
		case "ls", "list":
		default:
			return fmt.Errorf("I don't know knock recent %s. Try knock recent or knock recent rm <door>", args[0])
		}
	}

	// Doors joined through this relay only: their code hashes are never
	// sent to another one.
	store, err := state.ForRelay(cfg.Relay)
	if err != nil {
		return err
	}
	r, err := store.LoadRecent()
	if err != nil {
		return err
	}
	pruned := r.Prune(time.Now())
	if len(r.Items) == 0 {
		if pruned > 0 {
			_ = store.SaveRecent(r)
		}
		ui.Say("No recent doors yet. Knock on one with %s.", ui.Bold.Render("knock <door> <code>"))
		return nil
	}

	statuses := map[string]protocol.RecentEntry{}
	online := true
	c, err := connect(ctx, cfg)
	if err != nil {
		ui.Warning("%s Showing what Knock remembers.", sentence(err.Error()))
		online = false
	} else {
		defer c.Close()
		check := protocol.RecentCheck{}
		for _, it := range r.Items {
			check.Items = append(check.Items, protocol.RecentItem{DoorID: it.DoorID, CodeHash: it.CodeHash})
		}
		var st protocol.RecentStatus
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.RequestInto(rctx, protocol.TypeRecentCheck, check, &st)
		cancel()
		if err != nil {
			ui.Warning("Couldn't check your doors: %s", relayError(err))
			online = false
		}
		for _, e := range st.Items {
			statuses[e.DoorID] = e
		}
	}

	now := state.Now()
	rows := make([][]string, 0, len(r.Items))
	kinds := make([]string, 0, len(r.Items))
	for i := range r.Items {
		it := &r.Items[i]
		e, ok := statuses[it.DoorID]
		status := "unknown"
		changedOwner := false
		if online && ok {
			status = e.Status
			if status == protocol.PresenceCodeChanged {
				if it.MismatchSince == nil {
					it.MismatchSince = &now
				}
			} else {
				it.MismatchSince = nil
				if e.Owner != nil && e.Owner.Name != "" {
					o := termio.CleanIdentity(*e.Owner)
					if ownerChanged(*it, o) {
						// Keep the owner you knew; knock warns when you go in.
						changedOwner = true
					} else {
						it.OwnerName, it.Verified, it.Provider = o.Name, o.Verified, o.Provider
						if e.Device != "" {
							it.Device = termio.CleanText(e.Device)
						}
					}
				} else if e.Device != "" {
					it.Device = termio.CleanText(e.Device)
				}
			}
		}
		who := termio.CleanText(it.OwnerName)
		if !it.Verified {
			who += " (not verified)"
		}
		if changedOwner {
			who += " (owner changed)"
		}
		codeCol := "code ok"
		switch {
		case status == protocol.PresenceCodeChanged:
			codeCol = "code changed: ask for a new one"
		case status == "unknown":
			codeCol = ""
		}
		rows = append(rows, []string{termio.CleanText(it.DoorID), who, termio.CleanText(it.Device), statusLabel(status), codeCol, ago(it.LastSessionAt.Time)})
		kinds = append(kinds, status)
	}
	if err := store.SaveRecent(r); err != nil {
		ui.Warning("Couldn't save your recent doors: %v", err)
	}

	ui.Print(ui.Table([]string{"DOOR", "WHO", "DEVICE", "STATUS", "CODE", "LAST SESSION"}, rows, func(row, col int) lipgloss.Style {
		s := lipgloss.NewStyle()
		if row < 0 || row >= len(kinds) {
			return s
		}
		switch col {
		case 0:
			return s.Bold(true)
		case 3:
			switch kinds[row] {
			case protocol.PresenceOnline:
				return ui.Good
			case protocol.PresenceBusy:
				return ui.Warn
			case protocol.PresenceCodeChanged:
				return ui.Bad
			default:
				return ui.Dim
			}
		case 4:
			if kinds[row] == protocol.PresenceCodeChanged {
				return ui.Bad
			}
			return ui.Dim
		case 5:
			return ui.Dim
		}
		return s
	}))
	ui.Note("Knock with knock <door>. Forget one with knock recent rm <door>.")
	return nil
}

func statusLabel(s string) string {
	switch s {
	case protocol.PresenceOnline:
		return "● online"
	case protocol.PresenceBusy:
		return "● busy"
	case protocol.PresenceOffline:
		return "○ offline"
	case protocol.PresenceCodeChanged:
		return "✕ code changed"
	}
	return "? unknown"
}

func recentRemove(cfg config.Config, arg string) error {
	doorID, _, err := state.ParseShare([]string{arg})
	if err != nil {
		return err
	}
	store, err := state.ForRelay(cfg.Relay)
	if err != nil {
		return err
	}
	r, err := store.LoadRecent()
	if err != nil {
		return err
	}
	if !r.Remove(doorID) {
		return fmt.Errorf("%s isn't in your recent doors", doorID)
	}
	if err := store.SaveRecent(r); err != nil {
		return err
	}
	ui.Say("Forgot %s.", doorID)
	return nil
}

// ago formats a time as "just now", "5 minutes ago", "3 days ago".
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 60*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	}
	return t.Local().Format("2 Jan 2006")
}
