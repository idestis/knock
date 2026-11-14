package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A door claim is the relay's signed note that a door key held a door id
// from a given time: "c1.<since>.<mac>", with since in unix milliseconds
// and mac an HMAC-SHA256 over the door id, since and sha256(door key),
// keyed from KNOCK_TOKEN_SECRET. The key hash isn't in the claim; the
// presenter proves it by sending the key.
//
// Claims let the rightful host win its id back after a restart: when the
// relay holds the id for another key, a valid claim older than that key's
// hold takes it. They last as long as KNOCK_TOKEN_SECRET does.
const claimPrefix = "c1."

type claims struct{ key []byte }

func (c claims) mac(doorID string, since int64, keyHash []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte("knock.claim.v1\x00" + doorID + "\x00" + strconv.FormatInt(since, 10) + "\x00"))
	m.Write(keyHash)
	return m.Sum(nil)
}

func (c claims) issue(doorID string, keyHash []byte, since int64) string {
	return claimPrefix + strconv.FormatInt(since, 10) + "." + base64.RawURLEncoding.EncodeToString(c.mac(doorID, since, keyHash))
}

// verify returns when the claim says keyHash started holding doorID, if the
// claim is genuine. A claim from the future is refused.
func (c claims) verify(claim, doorID string, keyHash []byte) (int64, bool) {
	rest, ok := strings.CutPrefix(claim, claimPrefix)
	if !ok || len(claim) > 128 {
		return 0, false
	}
	ts, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, false
	}
	since, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || since <= 0 || since > time.Now().Add(5*time.Minute).UnixMilli() || strconv.FormatInt(since, 10) != ts {
		return 0, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, c.mac(doorID, since, keyHash)) {
		return 0, false
	}
	return since, true
}

// idLocks serialises door.open per door id, so the check of who holds an
// id and the conditional write that changes it happen one at a time,
// without holding the hub's mutex across store I/O.
type idLocks struct {
	mu sync.Mutex
	m  map[string]*idLock
}

type idLock struct {
	sync.Mutex
	refs int
}

func (l *idLocks) lock(id string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*idLock{}
	}
	k := l.m[id]
	if k == nil {
		k = &idLock{}
		l.m[id] = k
	}
	k.refs++
	l.mu.Unlock()
	k.Lock()
	return func() {
		k.Unlock()
		l.mu.Lock()
		if k.refs--; k.refs == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
}
