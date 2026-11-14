package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/idestis/knock/relay/internal/store"
)

// doorTTL reads KNOCK_DOOR_TTL: how long a door whose host has left stays
// known (in memory and in the store) before its id is free again.
func doorTTL(s string) (time.Duration, error) {
	if s == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute {
		return 0, fmt.Errorf("KNOCK_DOOR_TTL=%q: want a Go duration of at least 1m, e.g. 24h", s)
	}
	return d, nil
}

// openStore picks the door store from KNOCK_STORE. The URL is a secret: it
// never appears in an error.
func openStore(kind, url string, ttl time.Duration) (store.Store, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "memory":
		return store.NewMemory(ttl), nil
	case "valkey":
		if url == "" {
			return nil, errors.New("KNOCK_STORE=valkey needs KNOCK_VALKEY_URL (redis://, rediss:// or unix://)")
		}
		return store.NewValkey(url, ttl, store.DefaultPrefix)
	}
	return nil, fmt.Errorf("KNOCK_STORE=%q: want memory or valkey", kind)
}
