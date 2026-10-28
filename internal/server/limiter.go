package server

import (
	"sync"
	"time"
)

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

func (l *loginLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, found := l.failed[key]
	if !found {
		return true
	}
	if time.Since(entry.since) > l.window {
		delete(l.failed, key)
		return true
	}

	return entry.count < l.max
}

func (l *loginLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, found := l.failed[key]
	if !found || time.Since(entry.since) > l.window {
		entry = attempts{since: time.Now()}
	}
	entry.count++
	l.failed[key] = entry
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failed, key)
}
