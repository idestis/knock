package app

import (
	"context"
	"errors"
	"time"

	"github.com/atotto/clipboard"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/ui"
	"github.com/idestis/knock/protocol"
)

// runClose closes this device's door wherever it is open: Knock.app or a
// `knock open` in another terminal. It claims the door with the door key
// (which takes it from the other client), then closes it. A live session
// on that door ends.
func runClose(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: knock close")
	}
	d, ok, err := state.LoadDoor()
	if err != nil {
		return err
	}
	if !ok || d.DoorID == "" {
		ui.Say("You don't have a door yet, so there's nothing to close.")
		return nil
	}
	c, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	open := protocol.DoorOpen{DoorID: d.DoorID, DoorKey: d.DoorKey, Device: deviceName(), Code: d.Record()}
	if _, err := c.Request(rctx, protocol.TypeDoorOpen, open); err != nil {
		return relayError(err)
	}
	if _, err := c.Request(rctx, protocol.TypeDoorClose, nil); err != nil {
		return relayError(err)
	}
	ui.Say("Your door is closed. Nobody can knock until you open it again.")
	ui.Note("If Knock.app or knock open was running, it now shows the door as closed. Any live session ended.")
	return nil
}

// runCopy puts the code on the clipboard without printing it.
func runCopy(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: knock copy")
	}
	d, err := state.LoadOrCreateDoor()
	if err != nil {
		return err
	}
	if err := clipboard.WriteAll(d.ShareString()); err != nil {
		return errors.New("Couldn't reach the clipboard. On Linux, install wl-clipboard or xclip.")
	}
	ui.Say("Copied your code. Send it to the person who's helping.")
	return nil
}
