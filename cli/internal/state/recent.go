package state

import (
	"errors"
	"os"
	"sort"
	"time"
)

// RecentItem is one row of ~/.knock/relays/<relay>/recent.json.
type RecentItem struct {
	DoorID        string `json:"door_id"`
	OwnerName     string `json:"owner_name"`
	OwnerEmail    string `json:"owner_email,omitempty"`
	Provider      string `json:"provider"`
	Verified      bool   `json:"verified"`
	Device        string `json:"device"`
	CodeVersion   int64  `json:"code_version"`
	CodeHash      []byte `json:"code_hash"`
	LastSessionAt Time   `json:"last_session_at"`
	MismatchSince *Time  `json:"mismatch_since,omitempty"`
}

// Recent is ~/.knock/relays/<relay>/recent.json: doors joined through
// that relay. Their code hashes are only ever sent back to it.
type Recent struct {
	Items []RecentItem `json:"items"`
}

// MismatchPrune is how long a row may say "code changed" before it goes.
const MismatchPrune = 30 * 24 * time.Hour

// LoadRecent reads this relay's Recent list, moving one from an older
// build into place first.
func (s Store) LoadRecent() (Recent, error) {
	var r Recent
	if _, err := s.migrate("recent.json"); err != nil {
		return r, err
	}
	if err := readJSON(s.path("recent.json"), &r); err != nil && !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	return r, nil
}

// SaveRecent writes this relay's Recent list.
func (s Store) SaveRecent(r Recent) error {
	if r.Items == nil {
		r.Items = []RecentItem{}
	}
	return writeJSON(s.path("recent.json"), r)
}

// Find returns the row for doorID.
func (r *Recent) Find(doorID string) (*RecentItem, bool) {
	for i := range r.Items {
		if r.Items[i].DoorID == doorID {
			return &r.Items[i], true
		}
	}
	return nil, false
}

// Upsert replaces or adds the row for it.DoorID and keeps the newest first.
func (r *Recent) Upsert(it RecentItem) {
	if cur, ok := r.Find(it.DoorID); ok {
		*cur = it
	} else {
		r.Items = append(r.Items, it)
	}
	sort.SliceStable(r.Items, func(i, j int) bool { return r.Items[i].LastSessionAt.After(r.Items[j].LastSessionAt.Time) })
}

// Remove drops doorID and reports whether it was there.
func (r *Recent) Remove(doorID string) bool {
	for i := range r.Items {
		if r.Items[i].DoorID == doorID {
			r.Items = append(r.Items[:i], r.Items[i+1:]...)
			return true
		}
	}
	return false
}

// Prune drops rows that have mismatched for longer than MismatchPrune.
func (r *Recent) Prune(now time.Time) (removed int) {
	kept := r.Items[:0]
	for _, it := range r.Items {
		if it.MismatchSince != nil && now.Sub(it.MismatchSince.Time) > MismatchPrune {
			removed++
			continue
		}
		kept = append(kept, it)
	}
	r.Items = kept
	return removed
}
