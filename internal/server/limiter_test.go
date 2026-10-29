package server

import (
	"fmt"
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

func TestFailureCountingBlocksOnlyAfterMaxFailures(t *testing.T) {
	limiter := newLoginLimiter(3, time.Minute)
	for range 3 {
		if limiter.blocked("a") {
			t.Fatal("must not block before the limit")
		}
		limiter.fail("a")
	}
	if !limiter.blocked("a") {
		t.Fatal("must block at the limit")
	}
	if limiter.blocked("b") {
		t.Fatal("other keys are unaffected")
	}
	limiter.reset("a")
	if limiter.blocked("a") {
		t.Fatal("reset must reopen the key")
	}
}

func TestLimiterMapStaysBounded(t *testing.T) {
	limiter := newLoginLimiter(5, time.Hour)
	for i := range 3 * maxTracked {
		limiter.fail(fmt.Sprintf("key-%d", i))
	}
	if len(limiter.failed) > maxTracked {
		t.Fatalf("tracking %d keys, cap is %d", len(limiter.failed), maxTracked)
	}
}
