// Package protocol holds the wire types shared by knockd and the knock CLI.
// The Mac app keeps its own Swift copy in mac/Sources/Relay/Protocol.swift;
// change both together and bump Version.
//
// One WebSocket per client at /v1/ws. Control messages are JSON text frames
// shaped as Envelope. Terminal bytes are binary frames: host to guest is PTY
// output, guest to host is keyboard input.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Version is the protocol version a client announces in Hello.
const Version = 1

// Envelope wraps every control message.
type Envelope struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"` // request id, echoed in the reply
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

// Error is carried in an Envelope of type "error" (or attached to a reply).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Error codes.
const (
	ErrCodeMismatch  = "code_mismatch"
	ErrDoorOffline   = "door_offline"
	ErrDoorBusy      = "door_busy"
	ErrDoorTaken     = "door_taken"
	ErrRateLimited   = "rate_limited"
	ErrDenied        = "denied"
	ErrTimeout       = "timeout"
	ErrUnauthorised  = "unauthorised"
	ErrBadRequest    = "bad_request"
	ErrNotInSession  = "not_in_session"
	ErrNotFound      = "not_found"
	ErrUnsupported   = "unsupported"
	ErrAnonForbidden = "anonymous_not_allowed"
)

// Message types.
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypeError   = "error"
	TypeOK      = "ok"

	TypeDoorOpen        = "door.open"
	TypeDoorOpened      = "door.opened"
	TypeDoorClose       = "door.close"
	TypeDoorCodeSet     = "door.code.set"
	TypeDoorSaltGet     = "door.salt.get"
	TypeDoorSalt        = "door.salt"
	TypeDoorCodeChanged = "door.code_changed"
	TypeDoorForceNew    = "door.force_new_code"

	TypeRecentCheck    = "recent.check"
	TypeRecentStatus   = "recent.status"
	TypePresenceUpdate = "presence.update"

	TypeKnockRequest   = "knock.request"
	TypeKnockPending   = "knock.pending"
	TypeKnockIncoming  = "knock.incoming"
	TypeKnockAnswer    = "knock.answer"
	TypeKnockCancel    = "knock.cancel"
	TypeKnockCancelled = "knock.cancelled"

	TypeSessionStart  = "session.start"
	TypeSessionEnd    = "session.end"
	TypeSessionResize = "session.resize"
	TypeSessionMode   = "session.mode"

	TypeRelayDraining = "relay.draining"
)

// Client kinds.
const (
	ClientMac = "mac"
	ClientCLI = "cli"
)

// Hello is the first message on every socket.
type Hello struct {
	Token    string `json:"token,omitempty"` // relay access token; empty means anonymous
	Name     string `json:"name,omitempty"`  // display name, used only when anonymous
	Device   string `json:"device"`
	Client   string `json:"client"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// Identity is who someone is, as the relay sees them. Verified is false for
// anonymous users, whose name is whatever they typed. For signed-in users
// every field comes from the provider, carried in the relay's own token.
type Identity struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Login    string `json:"login,omitempty"` // GitHub username; empty for Google and anonymous
	Provider string `json:"provider"`        // google, github, anonymous
	Verified bool   `json:"verified"`
	Avatar   string `json:"avatar,omitempty"`
}

// AuthProviders is the body of GET /v1/auth/providers. Clients read it
// before connecting to learn whether they must sign in first.
type AuthProviders struct {
	Providers      []string `json:"providers"`
	SignInRequired bool     `json:"sign_in_required"`
}

// Welcome answers Hello.
type Welcome struct {
	User          Identity `json:"user"`
	Relay         string   `json:"relay"`
	Protocol      int      `json:"protocol"`
	KnockTimeout  int      `json:"knock_timeout_seconds"`
	AllowsAnon    bool     `json:"allows_anonymous"`
	AuthProviders []string `json:"auth_providers"`
}

// CodeRecord is what the relay stores for a door's current code.
type CodeRecord struct {
	Salt        []byte `json:"salt"`
	CodeHash    []byte `json:"code_hash"`
	CodeVersion int64  `json:"code_version"`
	KDF         string `json:"kdf"`
	Iterations  int    `json:"iterations"`
}

// DoorOpen registers a door. DoorID and DoorKey are generated once per
// device and kept by the client; the key proves ownership on reconnect.
type DoorOpen struct {
	DoorID  string     `json:"door_id"`
	DoorKey string     `json:"door_key"`
	Device  string     `json:"device"`
	Code    CodeRecord `json:"code"`
}

type DoorOpened struct {
	DoorID string `json:"door_id"`
}

type DoorCodeSet struct {
	Code CodeRecord `json:"code"`
}

type DoorSaltGet struct {
	DoorID string `json:"door_id"`
}

type DoorSalt struct {
	DoorID     string `json:"door_id"`
	Salt       []byte `json:"salt"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
}

type DoorRef struct {
	DoorID string `json:"door_id"`
}

type DoorForceNewCode struct {
	Reason string `json:"reason"`
}

// Presence values.
const (
	PresenceOnline      = "online"
	PresenceBusy        = "busy"
	PresenceOffline     = "offline"
	PresenceCodeChanged = "code_changed"
)

type RecentItem struct {
	DoorID   string `json:"door_id"`
	CodeHash []byte `json:"code_hash"`
}

type RecentCheck struct {
	Items []RecentItem `json:"items"`
}

type RecentEntry struct {
	DoorID      string    `json:"door_id"`
	Status      string    `json:"status"`
	Owner       *Identity `json:"owner,omitempty"`
	Device      string    `json:"device,omitempty"`
	CodeVersion int64     `json:"code_version,omitempty"`
}

type RecentStatus struct {
	Items []RecentEntry `json:"items"`
}

type KnockRequest struct {
	DoorID   string `json:"door_id"`
	CodeHash []byte `json:"code_hash"`
}

type KnockPending struct {
	KnockID   string `json:"knock_id"`
	ExpiresIn int    `json:"expires_in"`
}

type KnockIncoming struct {
	KnockID   string   `json:"knock_id"`
	From      Identity `json:"from"`
	Device    string   `json:"device"`
	ExpiresIn int      `json:"expires_in"`
}

type KnockAnswer struct {
	KnockID string `json:"knock_id"`
	Allow   bool   `json:"allow"`
}

type KnockRef struct {
	KnockID string `json:"knock_id"`
	Reason  string `json:"reason,omitempty"`
}

// Session roles.
const (
	RoleHost  = "host"
	RoleGuest = "guest"
)

type SessionStart struct {
	SessionID   string   `json:"session_id"`
	Role        string   `json:"role"`
	Peer        Identity `json:"peer"`
	PeerDevice  string   `json:"peer_device"`
	DoorID      string   `json:"door_id"`
	CodeVersion int64    `json:"code_version"`
	Cols        int      `json:"cols,omitempty"`
	Rows        int      `json:"rows,omitempty"`
	GuestInput  bool     `json:"guest_input"`
}

type SessionEnd struct {
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type SessionResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// SessionMode is sent by the host; the relay enforces it by dropping guest
// input frames while GuestInput is false ("Hands off").
type SessionMode struct {
	GuestInput bool `json:"guest_input"`
}

// Encode builds an Envelope with a JSON payload.
func Encode(typ, id string, v any) ([]byte, error) {
	env := Envelope{Type: typ, ID: id}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		env.Data = b
	}
	return json.Marshal(env)
}

// EncodeError builds an error Envelope.
func EncodeError(id, code, msg string) []byte {
	b, _ := json.Marshal(Envelope{Type: TypeError, ID: id, Error: &Error{Code: code, Message: msg}})
	return b
}
