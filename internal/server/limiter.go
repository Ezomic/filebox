package server

import (
	"sync"
	"time"
)

const sweepThreshold = 1024

type attempts struct {
	count int
	since time.Time
}

type loginLimiter struct {
	max    int
	window time.Duration

	mu     sync.Mutex
	failed map[string]attempts
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, failed: map[string]attempts{}}
}

func (l *loginLimiter) reserve(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.failed) > sweepThreshold {
		l.sweep()
	}

	entry, found := l.failed[key]
	if !found || time.Since(entry.since) > l.window {
		entry = attempts{since: time.Now()}
	}
	if entry.count >= l.max {
		return false
	}
	entry.count++
	l.failed[key] = entry

	return true
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failed, key)
}

func (l *loginLimiter) sweep() {
	for key, entry := range l.failed {
		if time.Since(entry.since) > l.window {
			delete(l.failed, key)
		}
	}
}
