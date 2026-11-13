package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/idestis/knock/cli/internal/state"
)

// Meta is meta.json in a session folder.
type Meta struct {
	SessionID     string     `json:"session_id"`
	Role          string     `json:"role"`
	DoorID        string     `json:"door_id"`
	Peer          Person     `json:"peer"`
	PeerDevice    string     `json:"peer_device"`
	Device        string     `json:"device"`
	Recording     string     `json:"recording"`
	Cols          int        `json:"cols"`
	Rows          int        `json:"rows"`
	StartedAt     state.Time `json:"started_at"`
	EndedAt       state.Time `json:"ended_at,omitzero"`
	EndReason     string     `json:"end_reason,omitempty"`
	Client        string     `json:"client"`
	ClientVersion string     `json:"client_version"`
}

// Person is who was on the other end.
type Person struct {
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Provider string `json:"provider"`
	Verified bool   `json:"verified"`
}

// Session is one recording in progress.
type Session struct {
	Dir  string
	Cast *CastWriter

	mu   sync.Mutex
	meta Meta
	keys *os.File
}

// Start creates the session folder and files. withInput also opens keys.jsonl.
func Start(root string, m Meta, withInput bool) (*Session, error) {
	if err := state.EnsureDir(root); err != nil {
		return nil, fmt.Errorf("create %s: %w", root, err)
	}
	name := fmt.Sprintf("%s_%s_%s", m.StartedAt.UTC().Format("2006-01-02T15-04-05Z"), Slug(m.Peer.Name), shortID(m.SessionID))
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, state.DirMode); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	s := &Session{Dir: dir, meta: m}
	if err := s.writeMeta(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "session.cast"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, state.FileMode)
	if err != nil {
		return nil, err
	}
	s.Cast, err = NewCastWriter(f, Header{Width: m.Cols, Height: m.Rows, Timestamp: m.StartedAt.Unix(),
		Title: "Knock: " + m.Peer.Name, Env: map[string]string{"TERM": os.Getenv("TERM"), "SHELL": os.Getenv("SHELL")}}, nil)
	if err != nil {
		f.Close()
		return nil, err
	}
	if withInput {
		s.keys, err = os.OpenFile(filepath.Join(dir, "keys.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, state.FileMode)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Input appends one chunk of typed input to keys.jsonl, if enabled.
func (s *Session) Input(source string, p []byte) {
	if s == nil || s.keys == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{
		"t": roundMicro(time.Since(s.meta.StartedAt.Time).Seconds()), "source": source, "data": string(p),
	})
	_, _ = s.keys.Write(append(b, '\n'))
}

// Finish closes the files and records the end.
func (s *Session) Finish(reason string) error {
	if s == nil {
		return nil
	}
	err := s.Cast.Close()
	s.mu.Lock()
	if s.keys != nil {
		_ = s.keys.Close()
		s.keys = nil
	}
	s.meta.EndedAt = state.Now()
	s.meta.EndReason = reason
	s.mu.Unlock()
	if mErr := s.writeMeta(); err == nil {
		err = mErr
	}
	return err
}

func (s *Session) writeMeta() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.meta, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir, "meta.json"), append(b, '\n'), state.FileMode)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns "Sam Carter" into "sam-carter".
func Slug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if s == "" {
		return "guest"
	}
	return s
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "s_")
	if len(id) > 4 {
		id = id[:4]
	}
	if id == "" {
		return "0000"
	}
	return id
}
