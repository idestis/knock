// Package client is a Go client for knockd, used by the knock CLI and tests.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/idestis/knock/protocol"
)

// Event is a control message the relay pushed without a request
// (knock.incoming, session.start, presence.update, ...).
type Event struct {
	Type string
	Data json.RawMessage
}

func (e Event) Decode(v any) error { return json.Unmarshal(e.Data, v) }

type Client struct {
	conn    *websocket.Conn
	Welcome protocol.Welcome

	Events chan Event  // control pushes
	Data   chan []byte // terminal bytes from the peer

	nextID  atomic.Int64
	mu      sync.Mutex
	waiters map[string]chan protocol.Envelope
	done    chan struct{}
	err     error
}

// WSURL turns https://relay into wss://relay/v1/ws.
func WSURL(relay string) string {
	u := strings.TrimRight(relay, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u + "/v1/ws"
}

// Dial connects and sends hello. token may be empty (anonymous).
func Dial(ctx context.Context, relay string, hello protocol.Hello) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, WSURL(relay), &websocket.DialOptions{HTTPHeader: http.Header{"User-Agent": {"knock-cli/" + hello.Version}}})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	c := &Client{conn: conn, Events: make(chan Event, 64), Data: make(chan []byte, 256),
		waiters: map[string]chan protocol.Envelope{}, done: make(chan struct{})}
	go c.readLoop()
	hello.Protocol = protocol.Version
	env, err := c.Request(ctx, protocol.TypeHello, hello)
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	if err := json.Unmarshal(env.Data, &c.Welcome); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	ctx := context.Background()
	for {
		typ, b, err := c.conn.Read(ctx)
		if err != nil {
			c.err = err
			c.mu.Lock()
			for id, w := range c.waiters {
				close(w)
				delete(c.waiters, id)
			}
			c.mu.Unlock()
			return
		}
		if typ == websocket.MessageBinary {
			c.Data <- b
			continue
		}
		var env protocol.Envelope
		if json.Unmarshal(b, &env) != nil {
			continue
		}
		if env.ID != "" {
			c.mu.Lock()
			w, ok := c.waiters[env.ID]
			if ok {
				delete(c.waiters, env.ID)
			}
			c.mu.Unlock()
			if ok {
				w <- env
				// session.start answering a knock is also an event.
				if env.Type != protocol.TypeSessionStart {
					continue
				}
			}
		}
		if env.Type == protocol.TypeError && env.ID != "" {
			// late error for a request nobody waits on any more (e.g. knock timeout)
			c.Events <- Event{Type: protocol.TypeError, Data: mustJSON(env.Error)}
			continue
		}
		c.Events <- Event{Type: env.Type, Data: env.Data}
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// Done is closed when the socket drops.
func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Err() error            { return c.err }

// Send fires a control message without waiting.
func (c *Client) Send(ctx context.Context, typ string, v any) error {
	b, err := protocol.Encode(typ, "", v)
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, b)
}

// Request sends a message and waits for the reply with the same id.
// An error reply comes back as *protocol.Error.
func (c *Client) Request(ctx context.Context, typ string, v any) (protocol.Envelope, error) {
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	w := make(chan protocol.Envelope, 1)
	c.mu.Lock()
	c.waiters[id] = w
	c.mu.Unlock()
	b, err := protocol.Encode(typ, id, v)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return protocol.Envelope{}, err
	}
	select {
	case env, ok := <-w:
		if !ok {
			return protocol.Envelope{}, errors.New("connection closed")
		}
		if env.Error != nil {
			return env, env.Error
		}
		return env, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return protocol.Envelope{}, ctx.Err()
	}
}

// RequestInto is Request plus decoding the reply payload.
func (c *Client) RequestInto(ctx context.Context, typ string, in, out any) error {
	env, err := c.Request(ctx, typ, in)
	if err != nil {
		return err
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// WriteData sends terminal bytes to the peer.
func (c *Client) WriteData(ctx context.Context, b []byte) error {
	return c.conn.Write(ctx, websocket.MessageBinary, b)
}

func (c *Client) Close() error { return c.conn.Close(websocket.StatusNormalClosure, "bye") }

// The PBKDF2 rounds a relay may ask a knocker to use. Hosts write
// protocol.PBKDF2Iterations; anything outside this range is refused.
const (
	MinIterations = 100_000
	MaxIterations = 10_000_000
)

// HashForDoor fetches the door's salt and hashes code with it.
func (c *Client) HashForDoor(ctx context.Context, doorID, code string) ([]byte, error) {
	var salt protocol.DoorSalt
	if err := c.RequestInto(ctx, protocol.TypeDoorSaltGet, protocol.DoorSaltGet{DoorID: doorID}, &salt); err != nil {
		return nil, err
	}
	if salt.KDF != protocol.KDFPBKDF2SHA256 {
		return nil, fmt.Errorf("unsupported kdf %q", salt.KDF)
	}
	if salt.Iterations < MinIterations || salt.Iterations > MaxIterations {
		// Too few makes a captured hash cheap to brute-force; too many
		// stalls this device. Either way the relay shouldn't ask for it.
		return nil, fmt.Errorf("the relay asked for %d hashing rounds, outside %d to %d", salt.Iterations, MinIterations, MaxIterations)
	}
	return protocol.HashCode(code, salt.Salt, salt.Iterations), nil
}
