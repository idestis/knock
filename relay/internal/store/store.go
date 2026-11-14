// Package store keeps door registrations across relay restarts: which door
// key holds a door id, its owner, its current code record and the hashes of
// a few earlier codes. Knocks, sessions and rate limits stay in the hub's
// memory.
//
// Every record expires TTL after its last write or touch, and the id is free
// again. The hub touches open doors on a heartbeat, so only doors whose host
// has been away for the whole TTL lapse.
package store

import (
	"context"
	"crypto/hmac"
	"errors"
	"time"

	"github.com/idestis/knock/protocol"
)

var (
	// ErrTaken: the id is held by another door key.
	ErrTaken = errors.New("store: door id held by another key")
	// ErrStale: the stored record has a newer code for the same key.
	ErrStale = errors.New("store: stored code is newer")
	// ErrNotFound: no record for the id (never written, or expired).
	ErrNotFound = errors.New("store: no such door")
)

// Door is one door registration. KeyHash is sha256 of the door key; the
// key itself never reaches the relay's store.
type Door struct {
	ID      string              `json:"id"`
	KeyHash []byte              `json:"key_hash"`
	Owner   protocol.Identity   `json:"owner"`
	Device  string              `json:"device"`
	Code    protocol.CodeRecord `json:"code"`
	// Prev holds hashes of earlier codes, newest last, so Recent can say
	// "code changed" to people who held one.
	Prev [][]byte `json:"prev,omitempty"`
	// ClaimedAt is when this key first held the id (unix milliseconds). An
	// older signed claim beats a newer holder.
	ClaimedAt int64 `json:"claimed_at"`
	// LastSeen is the last write or heartbeat. Not part of the JSON body;
	// stores keep it next to it.
	LastSeen time.Time `json:"-"`
}

// Store is the persistent half of the hub. Implementations must make Save
// and Touch atomic per id, so two relays can't both hold one.
type Store interface {
	// Load returns the records it has for ids. Missing ids are left out.
	Load(ctx context.Context, ids []string) (map[string]Door, error)
	// Save writes d if the id is free or held by holder (a key hash), and
	// restarts its TTL. It returns ErrTaken if another key holds the id, and
	// ErrStale if the same key holds it with a higher code version.
	Save(ctx context.Context, d Door, holder []byte) error
	// Touch restarts the TTL of id and sets its last-seen time if keyHash
	// holds it. ErrNotFound if it lapsed, ErrTaken if another key holds it.
	Touch(ctx context.Context, id string, keyHash []byte) error
	Close()
}

// checkSave is the rule both stores apply, given what they hold now.
func checkSave(cur *Door, d Door, holder []byte) error {
	if cur == nil {
		return nil
	}
	if !hmac.Equal(cur.KeyHash, holder) {
		return ErrTaken
	}
	if hmac.Equal(cur.KeyHash, d.KeyHash) && d.Code.CodeVersion < cur.Code.CodeVersion {
		return ErrStale
	}
	return nil
}
