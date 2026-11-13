package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one saved session folder.
type Entry struct {
	Name string // folder name, also the id for "knock sessions play"
	Dir  string
	Meta Meta
	Size int64
}

// List returns saved sessions, newest first.
func List(root string) ([]Entry, error) {
	des, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(root, de.Name())
		if !isSessionDir(dir) {
			continue
		}
		e := Entry{Name: de.Name(), Dir: dir}
		if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
			_ = json.Unmarshal(b, &e.Meta)
		}
		if fi, err := os.Stat(filepath.Join(dir, "session.cast")); err == nil {
			e.Size = fi.Size()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Find resolves what the user typed: a list number, a full folder name, or
// a unique part of one (such as the short id at the end).
func Find(root, id string) (Entry, error) {
	all, err := List(root)
	if err != nil {
		return Entry{}, err
	}
	if n, err := strconv.Atoi(id); err == nil && n >= 1 && n <= len(all) && len(id) < 4 {
		return all[n-1], nil
	}
	var hits []Entry
	for _, e := range all {
		if e.Name == id {
			return e, nil
		}
		if strings.Contains(e.Name, id) || e.Meta.SessionID == id {
			hits = append(hits, e)
		}
	}
	switch len(hits) {
	case 0:
		return Entry{}, fmt.Errorf("There's no saved session called %q. Run knock sessions ls to see them.", id)
	case 1:
		return hits[0], nil
	}
	return Entry{}, fmt.Errorf("%q matches %d sessions. Use more of the name.", id, len(hits))
}

// Cleanup deletes session folders older than days. days <= 0 keeps everything.
func Cleanup(root string, days int, now time.Time) (removed int, err error) {
	if days <= 0 {
		return 0, nil
	}
	all, err := List(root)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, e := range all {
		when := e.Meta.StartedAt.Time
		if when.IsZero() {
			if fi, err := os.Stat(e.Dir); err == nil {
				when = fi.ModTime()
			}
		}
		if when.IsZero() || !when.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(e.Dir); err == nil {
			removed++
		}
	}
	return removed, nil
}

// isSessionDir only accepts folders Knock made, so a sessions.dir pointing
// somewhere shared never loses unrelated folders.
func isSessionDir(dir string) bool {
	for _, f := range []string{"meta.json", "session.cast"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}
