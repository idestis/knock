// Package ratelimit counts events in fixed windows. In-memory; the
// multi-replica relay replaces it with atomic counters in the shared store.
package ratelimit

import (
	"sync"
	"time"
)

type window struct {
	count int
	until time.Time
}

type Limiter struct {
	mu     sync.Mutex
	limit  int
	period time.Duration
	w      map[string]*window
}

func New(limit int, period time.Duration) *Limiter {
	l := &Limiter{limit: limit, period: period, w: map[string]*window{}}
	go func() {
		for range time.Tick(period) {
			now := time.Now()
			l.mu.Lock()
			for k, v := range l.w {
				if now.After(v.until) {
					delete(l.w, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// Blocked reports whether key has reached the limit in the current window.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.w[key]
	return ok && time.Now().Before(w.until) && w.count >= l.limit
}

// Add records one event and returns the count in the current window.
func (l *Limiter) Add(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.w[key]
	if !ok || now.After(w.until) {
		w = &window{until: now.Add(l.period)}
		l.w[key] = w
	}
	w.count++
	return w.count
}

// Reset clears key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.w, key)
	l.mu.Unlock()
}
