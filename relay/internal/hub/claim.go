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

// A door claim is the relay's signed note that a door key has held a door
// id since a given time: "c2.<since>.<issued>.<mac>", both times in unix
// milliseconds, mac an HMAC-SHA256 over the door id, both times and
// sha256(door key), keyed from KNOCK_TOKEN_SECRET. The key hash isn't in
// the claim; the presenter proves it by sending the key.
//
// Claims let the rightful host win its id back after a restart: when the
// relay holds the id for another key, a valid claim older than that key's
// hold takes it. Every door.opened issues a fresh claim (same since, new
// issued), and a claim issued longer than ClaimMaxAge ago is refused, so a
// host who keeps using Knock keeps its id while one who has been gone for
// months can't take it back from someone who has used it since. Claims
// also last only as long as KNOCK_TOKEN_SECRET does.
const claimPrefix = "c2."

// DefaultClaimMaxAge is how long a claim stays good after it was issued.
const DefaultClaimMaxAge = 90 * 24 * time.Hour

type claims struct {
	key    []byte
	maxAge time.Duration
	now    func() time.Time
}

func (c claims) mac(doorID string, since, issued int64, keyHash []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte("knock.claim.v2\x00" + doorID + "\x00" + strconv.FormatInt(since, 10) + "\x00" + strconv.FormatInt(issued, 10) + "\x00"))
	m.Write(keyHash)
	return m.Sum(nil)
}

func (c claims) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c claims) issue(doorID string, keyHash []byte, since int64) string {
	issued := c.clock().UnixMilli()
	return claimPrefix + strconv.FormatInt(since, 10) + "." + strconv.FormatInt(issued, 10) + "." +
		base64.RawURLEncoding.EncodeToString(c.mac(doorID, since, issued, keyHash))
}

// verify returns when the claim says keyHash started holding doorID, if the
// claim is genuine and not older than maxAge. Times from the future are
// refused.
func (c claims) verify(claim, doorID string, keyHash []byte) (int64, bool) {
	rest, ok := strings.CutPrefix(claim, claimPrefix)
	if !ok || len(claim) > 160 {
		return 0, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return 0, false
	}
	now := c.clock()
	since, ok1 := claimTime(parts[0], now)
	issued, ok2 := claimTime(parts[1], now)
	if !ok1 || !ok2 || since > issued {
		return 0, false
	}
	maxAge := c.maxAge
	if maxAge <= 0 {
		maxAge = DefaultClaimMaxAge
	}
	if now.Sub(time.UnixMilli(issued)) > maxAge {
		return 0, false
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, c.mac(doorID, since, issued, keyHash)) {
		return 0, false
	}
	return since, true
}

// claimTime parses a canonical positive unix-ms time no more than five
// minutes in the future.
func claimTime(s string, now time.Time) (int64, bool) {
	t, err := strconv.ParseInt(s, 10, 64)
	if err != nil || t <= 0 || t > now.Add(5*time.Minute).UnixMilli() || strconv.FormatInt(t, 10) != s {
		return 0, false
	}
	return t, true
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
