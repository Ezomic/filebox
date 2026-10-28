package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReserveAllowsExactlyMaxUnderConcurrency(t *testing.T) {
	limiter := newLoginLimiter(5, time.Minute)

	var granted atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.reserve("192.0.2.1") {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	if granted.Load() != 5 {
		t.Fatalf("granted %d reservations, want 5", granted.Load())
	}
}

func TestResetReopensTheLimiter(t *testing.T) {
	limiter := newLoginLimiter(1, time.Minute)
	limiter.reserve("a")
	if limiter.reserve("a") {
		t.Fatal("second reservation must be refused")
	}
	limiter.reset("a")
	if !limiter.reserve("a") {
		t.Fatal("reservation must work after reset")
	}
}
