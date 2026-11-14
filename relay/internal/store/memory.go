package store

import (
	"context"
	"crypto/hmac"
	"slices"
	"sync"
	"time"
)

// Memory keeps records in this process. It is the default: a restart
// forgets every door, and signed claims are what win an id back.
type Memory struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	doors  map[string]memEntry
	writes int
}

type memEntry struct {
	d       Door
	expires time.Time
}

func NewMemory(ttl time.Duration) *Memory {
	return &Memory{ttl: ttl, now: time.Now, doors: map[string]memEntry{}}
}

// get returns the live entry for id, dropping it if it lapsed. Caller holds mu.
func (m *Memory) get(id string) (memEntry, bool) {
	e, ok := m.doors[id]
	if ok && !m.now().Before(e.expires) {
		delete(m.doors, id)
		return memEntry{}, false
	}
	return e, ok
}

func (m *Memory) Load(_ context.Context, ids []string) (map[string]Door, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Door{}
	for _, id := range ids {
		if e, ok := m.get(id); ok {
			out[id] = clone(e.d)
		}
	}
	return out, nil
}

func (m *Memory) Save(_ context.Context, d Door, holder []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cur *Door
	if e, ok := m.get(d.ID); ok {
		cur = &e.d
	}
	if err := checkSave(cur, d, holder); err != nil {
		return err
	}
	now := m.now()
	d = clone(d)
	if d.LastSeen.IsZero() {
		d.LastSeen = now
	}
	m.doors[d.ID] = memEntry{d: d, expires: now.Add(m.ttl)}
	m.sweep(now)
	return nil
}

func (m *Memory) Touch(_ context.Context, id string, keyHash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.get(id)
	switch {
	case !ok:
		return ErrNotFound
	case !hmac.Equal(e.d.KeyHash, keyHash):
		return ErrTaken
	}
	now := m.now()
	e.d.LastSeen = now
	e.expires = now.Add(m.ttl)
	m.doors[id] = e
	return nil
}

func (m *Memory) Close() {}

// sweep drops lapsed records every so many writes, so the map can't fill
// with doors nobody asks about again. Caller holds mu.
func (m *Memory) sweep(now time.Time) {
	if m.writes++; m.writes%512 != 0 {
		return
	}
	for id, e := range m.doors {
		if !now.Before(e.expires) {
			delete(m.doors, id)
		}
	}
}

func clone(d Door) Door {
	d.KeyHash = slices.Clone(d.KeyHash)
	d.Code.Salt = slices.Clone(d.Code.Salt)
	d.Code.CodeHash = slices.Clone(d.Code.CodeHash)
	prev := make([][]byte, len(d.Prev))
	for i, p := range d.Prev {
		prev[i] = slices.Clone(p)
	}
	if len(prev) == 0 {
		prev = nil
	}
	d.Prev = prev
	return d
}
