package state

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/idestis/knock/protocol"
)

// Door is ~/.knock/relays/<relay>/door.json: this device's door on one
// relay and its current code.
type Door struct {
	Relay         string `json:"relay"` // the origin this door belongs to
	DoorID        string `json:"door_id"`
	DoorKey       string `json:"door_key"`
	Code          string `json:"code"`
	CodeVersion   int64  `json:"code_version"`
	CodeCreatedAt Time   `json:"code_created_at"`
	Salt          []byte `json:"salt"`
	CodeHash      []byte `json:"code_hash"`
	// Claim is the relay's proof that this key owns DoorID, from door.opened.
	Claim string `json:"claim,omitempty"`
}

const doorAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// DoorIDLen is the length of a generated door id.
const DoorIDLen = 4

// NewDoorID returns DoorIDLen characters from the door alphabet.
func NewDoorID() string {
	b := make([]byte, DoorIDLen)
	limit := 256 - 256%len(doorAlphabet)
	var one [1]byte
	for i := range b {
		for {
			_, _ = rand.Read(one[:])
			if int(one[0]) < limit {
				b[i] = doorAlphabet[int(one[0])%len(doorAlphabet)]
				break
			}
		}
	}
	return string(b)
}

// NewDoorKey returns 32 random bytes, base64url.
func NewDoorKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// LoadDoor reads this relay's door.json, moving one from an older build
// into place first. ok is false when there is none yet, or when the file
// names another relay (it is ignored, never sent here). A door without
// code_created_at gets the current time, which starts its age clock; the
// caller saves it with the next write.
func (s Store) LoadDoor() (d Door, ok bool, err error) {
	d, ok, _, err = s.loadDoor()
	return d, ok, err
}

// loadDoor is LoadDoor that also reports whether it filled in relay or
// code_created_at, so the file still needs saving.
func (s Store) loadDoor() (d Door, ok, filled bool, err error) {
	migrated, err := s.migrate("door.json")
	if err != nil {
		return d, false, false, err
	}
	if err := readJSON(s.path("door.json"), &d); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Door{}, false, false, nil
		}
		return Door{}, false, false, err
	}
	if d.Relay != "" && d.Relay != s.Origin {
		return Door{}, false, false, nil
	}
	filled = d.Relay == "" || d.CodeCreatedAt.IsZero()
	d.Relay = s.Origin
	if d.CodeCreatedAt.IsZero() {
		d.CodeCreatedAt = Now()
	}
	if migrated && filled {
		// The move adds relay (and starts the clock) right away, so the
		// file can't be mistaken for another relay's later.
		if err := s.SaveDoor(d); err != nil {
			return d, true, true, err
		}
		filled = false
	}
	return d, true, filled, nil
}

// SaveDoor writes this relay's door.json.
func (s Store) SaveDoor(d Door) error {
	d.Relay = s.Origin
	return writeJSON(s.path("door.json"), d)
}

// DoorPath is where this relay's door.json lives.
func (s Store) DoorPath() string { return s.path("door.json") }

// DoorModTime returns door.json's modification time in Unix nanoseconds, or 0.
func (s Store) DoorModTime() int64 {
	fi, err := os.Stat(s.path("door.json"))
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// LoadOrCreateDoor returns the door, creating the id, key and first code
// when missing.
func (s Store) LoadOrCreateDoor() (Door, error) {
	d, ok, changed, err := s.loadDoor()
	if err != nil {
		return d, err
	}
	changed = changed || !ok
	if !ok {
		d = Door{Relay: s.Origin}
	}
	if len(d.DoorID) != DoorIDLen {
		// Empty, or an 8-character id from before the one-code format ("BVJY 1696 ME").
		d.DoorID, d.Claim, changed = NewDoorID(), "", true
	}
	if d.DoorKey == "" {
		d.DoorKey, d.Claim, changed = NewDoorKey(), "", true
	}
	if d.Code == "" || len(d.Salt) != protocol.SaltLen || len(d.CodeHash) != protocol.HashLen {
		d.Rotate()
		changed = true
	}
	if changed {
		if err := s.SaveDoor(d); err != nil {
			return d, err
		}
	}
	return d, nil
}

// SaveClaim stores the relay's claim for this door without touching the
// code, re-reading door.json first so a code another client just wrote
// is kept. It does nothing if the file now holds another door or key.
func (s Store) SaveClaim(doorID, doorKey, claim string) error {
	d, ok, err := s.LoadDoor()
	if err != nil || !ok || d.DoorID != doorID || d.DoorKey != doorKey || d.Claim == claim {
		return err
	}
	d.Claim = claim
	return s.SaveDoor(d)
}

// CodeExpired reports whether the code is older than maxAgeDays.
// 0 means codes never expire.
func (d Door) CodeExpired(maxAgeDays int, now time.Time) bool {
	if maxAgeDays <= 0 || d.CodeCreatedAt.IsZero() {
		return false
	}
	return now.Sub(d.CodeCreatedAt.Time) >= time.Duration(maxAgeDays)*24*time.Hour
}

// Rotate replaces the code, salt and hash and bumps the version.
func (d *Door) Rotate() {
	d.Code = protocol.NewCode()
	d.Salt = protocol.NewSalt()
	d.CodeHash = protocol.HashCode(d.Code, d.Salt, protocol.PBKDF2Iterations)
	d.CodeVersion++
	d.CodeCreatedAt = Now()
}

// Record is what the relay stores for this door's code.
func (d Door) Record() protocol.CodeRecord {
	return protocol.CodeRecord{Salt: d.Salt, CodeHash: d.CodeHash, CodeVersion: d.CodeVersion,
		KDF: protocol.KDFPBKDF2SHA256, Iterations: protocol.PBKDF2Iterations}
}

// DisplayCode formats "4821KQ" as "4821 KQ".
func DisplayCode(code string) string {
	c := protocol.NormaliseCode(code)
	if len(c) != 6 {
		return c
	}
	return c[:4] + " " + c[4:]
}

// ShareString is the one code the host gives the guest: "BVJY 1696 ME".
// The guest runs `knock BVJY 1696 ME`.
func (d Door) ShareString() string {
	return strings.ToUpper(d.DoorID) + " " + DisplayCode(d.Code)
}
