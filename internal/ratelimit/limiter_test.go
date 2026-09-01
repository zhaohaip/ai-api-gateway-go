package ratelimit

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestMemoryLimiterAllowsBurstAndRecoversWithTime(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter, err := newMemoryLimiter(Limit{}, Limit{RequestsPerSecond: 2, Burst: 2}, clock.Now)
	if err != nil {
		t.Fatalf("newMemoryLimiter() error = %v", err)
	}
	if err := limiter.Allow("client"); err != nil {
		t.Fatalf("first Allow() error = %v", err)
	}
	if err := limiter.Allow("client"); err != nil {
		t.Fatalf("second Allow() error = %v", err)
	}
	assertLimitError(t, limiter.Allow("client"), ScopeAPIKey, 500*time.Millisecond)

	clock.Advance(500 * time.Millisecond)
	if err := limiter.Allow("client"); err != nil {
		t.Fatalf("Allow() after refill error = %v", err)
	}
}

func TestMemoryLimiterIsolatesAPIKeyBuckets(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter, err := newMemoryLimiter(Limit{}, Limit{RequestsPerSecond: 1, Burst: 1}, clock.Now)
	if err != nil {
		t.Fatalf("newMemoryLimiter() error = %v", err)
	}
	if err := limiter.Allow("first"); err != nil {
		t.Fatalf("Allow(first) error = %v", err)
	}
	assertLimitError(t, limiter.Allow("first"), ScopeAPIKey, time.Second)
	if err := limiter.Allow("second"); err != nil {
		t.Fatalf("Allow(second) error = %v", err)
	}
}

func TestMemoryLimiterAppliesGlobalLimitAcrossKeys(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter, err := newMemoryLimiter(Limit{RequestsPerSecond: 1, Burst: 1}, Limit{}, clock.Now)
	if err != nil {
		t.Fatalf("newMemoryLimiter() error = %v", err)
	}
	if err := limiter.Allow("first"); err != nil {
		t.Fatalf("Allow(first) error = %v", err)
	}
	assertLimitError(t, limiter.Allow("second"), ScopeGlobal, time.Second)
}

func TestMemoryLimiterDoesNotConsumeGlobalTokenWhenAPIKeyIsLimited(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter, err := newMemoryLimiter(
		Limit{RequestsPerSecond: 1, Burst: 2},
		Limit{RequestsPerSecond: 1, Burst: 1},
		clock.Now,
	)
	if err != nil {
		t.Fatalf("newMemoryLimiter() error = %v", err)
	}
	if err := limiter.Allow("first"); err != nil {
		t.Fatalf("Allow(first) error = %v", err)
	}
	assertLimitError(t, limiter.Allow("first"), ScopeAPIKey, time.Second)
	if err := limiter.Allow("second"); err != nil {
		t.Fatalf("Allow(second) error = %v", err)
	}
}

func TestMemoryLimiterConcurrentAccess(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter, err := newMemoryLimiter(Limit{}, Limit{RequestsPerSecond: 1, Burst: 20}, clock.Now)
	if err != nil {
		t.Fatalf("newMemoryLimiter() error = %v", err)
	}
	var waitGroup sync.WaitGroup
	results := make(chan error, 100)
	for index := 0; index < 100; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			results <- limiter.Allow("shared")
		}()
	}
	waitGroup.Wait()
	close(results)
	allowed := 0
	for err := range results {
		if err == nil {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("allowed requests = %d, want 20", allowed)
	}
}

func TestMemoryLimiterValidatesConfigurationAndZeroDisablesLimit(t *testing.T) {
	invalid := []Limit{
		{RequestsPerSecond: -1, Burst: 1},
		{RequestsPerSecond: 1, Burst: -1},
		{RequestsPerSecond: 0, Burst: 1},
		{RequestsPerSecond: 1, Burst: 0},
	}
	for _, limit := range invalid {
		if _, err := NewMemoryLimiter(limit, Limit{}); err == nil {
			t.Fatalf("NewMemoryLimiter(%#v) succeeded", limit)
		}
	}
	limiter, err := NewMemoryLimiter(Limit{}, Limit{})
	if err != nil {
		t.Fatalf("NewMemoryLimiter() error = %v", err)
	}
	for index := 0; index < 100; index++ {
		if err := limiter.Allow("client"); err != nil {
			t.Fatalf("disabled limiter request %d error = %v", index, err)
		}
	}
}

func assertLimitError(t *testing.T, err error, scope Scope, retryAfter time.Duration) {
	t.Helper()
	var limitErr *Error
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if limitErr.Scope != scope || limitErr.RetryAfter != retryAfter {
		t.Fatalf("limit error = %#v, want scope %q and retry %s", limitErr, scope, retryAfter)
	}
}
