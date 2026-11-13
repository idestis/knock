// Package record saves sessions under ~/.knock/sessions as asciicast v2
// plus meta.json, and replays them.
package record

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// Header is the first line of an asciicast v2 file.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp,omitempty"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// CastWriter writes asciicast v2 events, one write per event so a crash
// loses nothing already shown. It is safe for concurrent use.
// Output chunks that end in the middle of a UTF-8 sequence are held back
// until the rest arrives, so every event is valid JSON text.
type CastWriter struct {
	mu      sync.Mutex
	w       io.Writer
	c       io.Closer
	start   time.Time
	now     func() time.Time
	pending []byte
	err     error
}

// NewCastWriter writes the header and returns a writer. now may be nil.
func NewCastWriter(w io.Writer, h Header, now func() time.Time) (*CastWriter, error) {
	if now == nil {
		now = time.Now
	}
	cw := &CastWriter{w: w, now: now}
	if c, ok := w.(io.Closer); ok {
		cw.c = c
	}
	cw.start = now()
	h.Version = 2
	if h.Timestamp == 0 {
		h.Timestamp = cw.start.Unix()
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if _, err := cw.w.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	return cw, nil
}

// Output records terminal output.
func (cw *CastWriter) Output(p []byte) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	buf := append(cw.pending, p...)
	cut := completeUTF8(buf)
	cw.pending = append([]byte(nil), buf[cut:]...)
	if cut > 0 {
		cw.event("o", string(buf[:cut]))
	}
}

// Resize records a terminal size change.
func (cw *CastWriter) Resize(cols, rows int) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.event("r", fmt.Sprintf("%dx%d", cols, rows))
}

// Marker records a named marker (asciicast "m" event).
func (cw *CastWriter) Marker(label string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.event("m", label)
}

func (cw *CastWriter) event(kind, data string) {
	if cw.err != nil {
		return
	}
	t := cw.now().Sub(cw.start).Seconds()
	b, err := json.Marshal([]any{roundMicro(t), kind, data})
	if err != nil {
		cw.err = err
		return
	}
	_, cw.err = cw.w.Write(append(b, '\n'))
}

// Close flushes held-back bytes and closes the underlying writer.
func (cw *CastWriter) Close() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if len(cw.pending) > 0 {
		cw.event("o", string(cw.pending)) // invalid tail becomes U+FFFD
		cw.pending = nil
	}
	if cw.c != nil {
		if err := cw.c.Close(); err != nil && cw.err == nil {
			cw.err = err
		}
	}
	return cw.err
}

func roundMicro(f float64) float64 { return float64(int64(f*1e6+0.5)) / 1e6 }

// completeUTF8 returns how many leading bytes of b form complete UTF-8
// (or definitely invalid) sequences; the rest may still be completed.
func completeUTF8(b []byte) int {
	// Look back at most 3 bytes for the start of an unfinished rune.
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		c := b[i]
		if c < 0x80 {
			return len(b)
		}
		if utf8.RuneStart(c) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			return len(b)
		}
	}
	return len(b)
}
