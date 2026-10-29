package server

import (
	"sync"
	"time"
)

const (
	sweepThreshold = 1024
	sweepEvery     = time.Minute
	maxTracked     = 10000
)

type attempts struct {
	count int
	since time.Time
}

type loginLimiter struct {
	max    int
	window time.Duration

	mu        sync.Mutex
	failed    map[string]attempts
	lastSweep time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, failed: map[string]attempts{}}
}

func (l *loginLimiter) reserve(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.trim()

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

// blocked and fail count only real failures, unlike reserve, which counts every attempt.
func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, found := l.failed[key]

	return found && time.Since(entry.since) <= l.window && entry.count >= l.max
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trim()

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

// An attacker choosing keys must not make every call scan a huge map, nor grow it without bound.
func (l *loginLimiter) trim() {
	if len(l.failed) <= sweepThreshold {
		return
	}
	if time.Since(l.lastSweep) > sweepEvery {
		l.sweep()
		l.lastSweep = time.Now()
	}
	for key := range l.failed {
		if len(l.failed) < maxTracked {
			break
		}
		delete(l.failed, key)
	}
}

func (l *loginLimiter) sweep() {
	for key, entry := range l.failed {
		if time.Since(entry.since) > l.window {
			delete(l.failed, key)
		}
	}
}
