package state

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"github.com/idestis/knock/protocol"
)

// Door is ~/.knock/door.json: this device's door and its current code.
type Door struct {
	DoorID      string `json:"door_id"`
	DoorKey     string `json:"door_key"`
	Code        string `json:"code"`
	CodeVersion int64  `json:"code_version"`
	Salt        []byte `json:"salt"`
	CodeHash    []byte `json:"code_hash"`
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

// LoadDoor reads door.json. ok is false when there is none yet.
func LoadDoor() (d Door, ok bool, err error) {
	p, err := Path("door.json")
	if err != nil {
		return d, false, err
	}
	if err := readJSON(p, &d); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return d, false, nil
		}
		return d, false, err
	}
	return d, true, nil
}

// SaveDoor writes door.json.
func SaveDoor(d Door) error {
	p, err := Path("door.json")
	if err != nil {
		return err
	}
	return writeJSON(p, d)
}

// DoorModTime returns door.json's modification time in Unix nanoseconds, or 0.
func DoorModTime() int64 {
	p, err := Path("door.json")
	if err != nil {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// LoadOrCreateDoor returns the door, creating the id, key and first code
// when missing.
func LoadOrCreateDoor() (Door, error) {
	d, _, err := LoadDoor()
	if err != nil {
		return d, err
	}
	changed := false
	if len(d.DoorID) != DoorIDLen {
		// Empty, or an 8-character id from before the one-code format ("BVJY 1696 ME").
		d.DoorID, changed = NewDoorID(), true
	}
	if d.DoorKey == "" {
		d.DoorKey, changed = NewDoorKey(), true
	}
	if d.Code == "" || len(d.Salt) != protocol.SaltLen || len(d.CodeHash) != protocol.HashLen {
		d.Rotate()
		changed = true
	}
	if changed {
		if err := SaveDoor(d); err != nil {
			return d, err
		}
	}
	return d, nil
}

// Rotate replaces the code, salt and hash and bumps the version.
func (d *Door) Rotate() {
	d.Code = protocol.NewCode()
	d.Salt = protocol.NewSalt()
	d.CodeHash = protocol.HashCode(d.Code, d.Salt, protocol.PBKDF2Iterations)
	d.CodeVersion++
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
