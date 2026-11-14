package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/store"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func record(id string, key []byte, version int64) store.Door {
	return store.Door{ID: id, KeyHash: key, Owner: protocol.Identity{ID: "github:maya", Name: "Maya", Provider: "github", Verified: true},
		Device: "maya-mbp", ClaimedAt: 1_700_000_000_000,
		Code: protocol.CodeRecord{Salt: randBytes(protocol.SaltLen), CodeHash: randBytes(protocol.HashLen), CodeVersion: version,
			KDF: protocol.KDFPBKDF2SHA256, Iterations: protocol.PBKDF2Iterations},
		Prev: [][]byte{randBytes(protocol.HashLen)}}
}

// Every Store must behave the same. The Valkey run needs a server:
// KNOCK_TEST_VALKEY_URL=redis://127.0.0.1:6379 go test ./relay/internal/store
func TestContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		contract(t, func(ttl time.Duration) store.Store { return store.NewMemory(ttl) })
	})
	t.Run("valkey", func(t *testing.T) {
		url := os.Getenv("KNOCK_TEST_VALKEY_URL")
		if url == "" {
			t.Skip("KNOCK_TEST_VALKEY_URL not set")
		}
		contract(t, func(ttl time.Duration) store.Store {
			// A fresh prefix per store, so runs don't see each other.
			s, err := store.NewValkey(url, ttl, "knocktest:"+hex.EncodeToString(randBytes(6))+":")
			if err != nil {
				t.Fatal(err)
			}
			return s
		})
	})
}

func contract(t *testing.T, open func(ttl time.Duration) store.Store) {
	ctx := context.Background()

	t.Run("save and load", func(t *testing.T) {
		s := open(time.Hour)
		defer s.Close()
		mine := randBytes(32)
		d := record("maya01", mine, 3)
		if err := s.Save(ctx, d, mine); err != nil {
			t.Fatal(err)
		}
		got, err := s.Load(ctx, []string{"maya01", "nobody"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want one record, got %d", len(got))
		}
		g := got["maya01"]
		if g.ID != d.ID || !protocol.EqualHash(g.KeyHash, mine) || g.Owner != d.Owner || g.Device != d.Device || g.ClaimedAt != d.ClaimedAt ||
			g.Code.CodeVersion != 3 || !protocol.EqualHash(g.Code.CodeHash, d.Code.CodeHash) || len(g.Prev) != 1 || g.LastSeen.IsZero() {
			t.Fatalf("round trip: got %+v", g)
		}
		if got, err := s.Load(ctx, nil); err != nil || len(got) != 0 {
			t.Fatalf("empty load: %v %v", got, err)
		}
	})

	t.Run("one key holds an id", func(t *testing.T) {
		s := open(time.Hour)
		defer s.Close()
		mine, theirs := randBytes(32), randBytes(32)
		if err := s.Save(ctx, record("maya02", mine, 1), mine); err != nil {
			t.Fatal(err)
		}
		// Another key can't take it by naming itself as the holder.
		if err := s.Save(ctx, record("maya02", theirs, 9), theirs); !errors.Is(err, store.ErrTaken) {
			t.Fatalf("other key: want ErrTaken, got %v", err)
		}
		// The holder can write a newer code, not an older one.
		if err := s.Save(ctx, record("maya02", mine, 2), mine); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, record("maya02", mine, 1), mine); !errors.Is(err, store.ErrStale) {
			t.Fatalf("older code: want ErrStale, got %v", err)
		}
		// Naming the current holder hands the id over (a claim takeover).
		if err := s.Save(ctx, record("maya02", theirs, 1), mine); err != nil {
			t.Fatalf("takeover: %v", err)
		}
		got, _ := s.Load(ctx, []string{"maya02"})
		if !protocol.EqualHash(got["maya02"].KeyHash, theirs) {
			t.Fatal("takeover didn't stick")
		}
		if err := s.Save(ctx, record("maya02", mine, 5), mine); !errors.Is(err, store.ErrTaken) {
			t.Fatalf("old holder after takeover: want ErrTaken, got %v", err)
		}
	})

	t.Run("touch", func(t *testing.T) {
		s := open(time.Hour)
		defer s.Close()
		mine, theirs := randBytes(32), randBytes(32)
		if err := s.Touch(ctx, "maya03", mine); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("absent: want ErrNotFound, got %v", err)
		}
		if err := s.Save(ctx, record("maya03", mine, 1), mine); err != nil {
			t.Fatal(err)
		}
		if err := s.Touch(ctx, "maya03", theirs); !errors.Is(err, store.ErrTaken) {
			t.Fatalf("other key: want ErrTaken, got %v", err)
		}
		if err := s.Touch(ctx, "maya03", mine); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("records lapse after the TTL", func(t *testing.T) {
		s := open(400 * time.Millisecond)
		defer s.Close()
		mine, theirs := randBytes(32), randBytes(32)
		if err := s.Save(ctx, record("maya04", mine, 1), mine); err != nil {
			t.Fatal(err)
		}
		// A touch restarts the clock.
		time.Sleep(250 * time.Millisecond)
		if err := s.Touch(ctx, "maya04", mine); err != nil {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
		if got, _ := s.Load(ctx, []string{"maya04"}); len(got) != 1 {
			t.Fatal("touched record lapsed early")
		}
		time.Sleep(400 * time.Millisecond)
		if got, _ := s.Load(ctx, []string{"maya04"}); len(got) != 0 {
			t.Fatal("record outlived its TTL")
		}
		// The id is free for anyone.
		if err := s.Save(ctx, record("maya04", theirs, 1), theirs); err != nil {
			t.Fatalf("lapsed id: %v", err)
		}
	})
}
