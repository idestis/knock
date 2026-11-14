package hub

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestClaimMaxAge(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	c := claims{key: []byte("k"), maxAge: 90 * 24 * time.Hour, now: func() time.Time { return now }}
	key := sha256.Sum256([]byte("door key"))
	since := now.Add(-400 * 24 * time.Hour).UnixMilli() // held the id for over a year

	fresh := c.issue("bvjy", key[:], since)
	if got, ok := c.verify(fresh, "bvjy", key[:]); !ok || got != since {
		t.Fatalf("fresh claim: %d %v", got, ok)
	}

	// 89 days later it still wins the id back; 91 days later it doesn't.
	now = now.Add(89 * 24 * time.Hour)
	if _, ok := c.verify(fresh, "bvjy", key[:]); !ok {
		t.Fatal("claim refused inside max age")
	}
	now = now.Add(2 * 24 * time.Hour)
	if _, ok := c.verify(fresh, "bvjy", key[:]); ok {
		t.Fatal("claim accepted past max age")
	}

	// Reopening the door issues a fresh claim that keeps the original since.
	renewed := c.issue("bvjy", key[:], since)
	if got, ok := c.verify(renewed, "bvjy", key[:]); !ok || got != since {
		t.Fatalf("renewed claim: %d %v", got, ok)
	}

	// Moving issued forward without the key breaks the signature.
	parts := strings.Split(strings.TrimPrefix(fresh, claimPrefix), ".")
	bumped := claimPrefix + parts[0] + "." + "9" + parts[1][1:] + "." + parts[2]
	if _, ok := c.verify(bumped, "bvjy", key[:]); ok {
		t.Fatal("tampered issued time accepted")
	}
	// Issued before since, or in the future, is refused.
	if _, ok := c.verify(c.issue("bvjy", key[:], now.Add(time.Hour).UnixMilli()), "bvjy", key[:]); ok {
		t.Fatal("since after issued accepted")
	}
}

func TestClaimDefaultMaxAge(t *testing.T) {
	now := time.Now()
	c := claims{key: []byte("k"), now: func() time.Time { return now }}
	key := sha256.Sum256([]byte("door key"))
	cl := c.issue("bvjy", key[:], now.UnixMilli())
	now = now.Add(DefaultClaimMaxAge + time.Minute)
	if _, ok := c.verify(cl, "bvjy", key[:]); ok {
		t.Fatal("zero maxAge should default to 90 days")
	}
}
