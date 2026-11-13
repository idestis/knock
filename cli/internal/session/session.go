// Package session runs a live shared shell: the host's PTY or the guest's
// passthrough. No TUI runs during a session; the terminal is in raw mode
// and bytes go straight through.
package session

import (
	"context"
	"sync"
	"time"

	"github.com/idestis/knock/cli/internal/config"
	"github.com/idestis/knock/cli/internal/record"
	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/cli/internal/termio"
	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

// Conn is the part of *client.Client a session uses.
type Conn interface {
	Send(ctx context.Context, typ string, v any) error
	Request(ctx context.Context, typ string, v any) (protocol.Envelope, error)
	WriteData(ctx context.Context, b []byte) error
	Done() <-chan struct{}
}

var _ Conn = (*client.Client)(nil)

// Common holds what both roles need.
type Common struct {
	Conn   Conn
	Events <-chan client.Event
	Data   <-chan []byte
	Term   *termio.Terminal
	Start  protocol.SessionStart

	SaveMode    string // config.SaveOff, SaveOutput or SaveOutputInput
	SessionsDir string
	Device      string
	Version     string
}

// Result says how a session ended.
type Result struct {
	Reason   string // relay reason (host_ended, guest_left, ...) or local
	Duration time.Duration
	SavedTo  string         // recording folder, if any
	Pending  []client.Event // events that arrived during the session for the caller
	Err      error          // recording or terminal problems worth a mention
}

// Local end reasons, alongside the relay's.
const (
	ReasonShellExited = "shell_exited"
	ReasonRelayLost   = "relay_lost"
	ReasonYouEnded    = "you_ended"
)

func (c Common) startRecording(role string, cols, rows int) (*record.Session, error) {
	if c.SaveMode == "" || c.SaveMode == config.SaveOff {
		return nil, nil
	}
	p := c.Start.Peer
	return record.Start(c.SessionsDir, record.Meta{
		SessionID: c.Start.SessionID, Role: role, DoorID: c.Start.DoorID,
		Peer:       record.Person{Name: p.Name, Email: p.Email, Provider: p.Provider, Verified: p.Verified},
		PeerDevice: c.Start.PeerDevice, Device: c.Device, Recording: c.SaveMode,
		Cols: cols, Rows: rows, StartedAt: state.Now(), Client: protocol.ClientCLI, ClientVersion: c.Version,
	}, c.SaveMode == config.SaveOutputInput)
}

// readInput pumps the keyboard into ch until stop is called. It uses a
// cancellable reader so the next prompt gets the next keystroke.
func readInput(t *termio.Terminal) (<-chan []byte, func()) {
	ch := make(chan []byte, 16)
	stopped := make(chan struct{})
	r, err := t.Reader()
	if err != nil {
		close(ch)
		return ch, func() {}
	}
	go func() {
		defer close(ch)
		defer r.Close()
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				select {
				case ch <- append([]byte(nil), buf[:n]...):
				case <-stopped:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			close(stopped)
			r.Cancel()
		})
	}
}

func peerFirstName(id protocol.Identity) string {
	for i, r := range id.Name {
		if r == ' ' && i > 0 {
			return id.Name[:i]
		}
	}
	if id.Name == "" {
		return "They"
	}
	return id.Name
}
