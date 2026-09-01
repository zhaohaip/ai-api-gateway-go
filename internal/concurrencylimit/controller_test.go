package concurrencylimit

import (
	"errors"
	"strconv"
	"sync"
	"testing"
)

func TestMemoryControllerAppliesGlobalLimitAcrossKeys(t *testing.T) {
	controller := newTestController(t, 1, 0)
	lease, err := controller.Acquire("first")
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	defer lease.Release()
	assertConcurrencyError(t, acquireError(controller, "second"), ScopeGlobal)
}

func TestMemoryControllerIsolatesAPIKeySlots(t *testing.T) {
	controller := newTestController(t, 0, 1)
	first, err := controller.Acquire("first")
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	defer first.Release()
	assertConcurrencyError(t, acquireError(controller, "first"), ScopeAPIKey)
	second, err := controller.Acquire("second")
	if err != nil {
		t.Fatalf("Acquire(second) error = %v", err)
	}
	second.Release()
}

func TestMemoryControllerReturnsGlobalSlotWhenAPIKeyAcquireFails(t *testing.T) {
	controller := newTestController(t, 2, 1)
	first, err := controller.Acquire("first")
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	defer first.Release()
	assertConcurrencyError(t, acquireError(controller, "first"), ScopeAPIKey)

	second, err := controller.Acquire("second")
	if err != nil {
		t.Fatalf("Acquire(second) after KeyID rejection error = %v", err)
	}
	second.Release()
}

func TestMemoryLeaseReleaseIsConcurrentAndIdempotent(t *testing.T) {
	controller := newTestController(t, 1, 1)
	lease, err := controller.Acquire("client")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	var waitGroup sync.WaitGroup
	for index := 0; index < 100; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			lease.Release()
		}()
	}
	waitGroup.Wait()

	next, err := controller.Acquire("client")
	if err != nil {
		t.Fatalf("Acquire() after repeated Release error = %v", err)
	}
	next.Release()
	next.Release()
}

func TestMemoryControllerConcurrentAcquireRespectsLimit(t *testing.T) {
	controller := newTestController(t, 10, 0)
	var waitGroup sync.WaitGroup
	results := make(chan Lease, 100)
	for index := 0; index < 100; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			lease, err := controller.Acquire(strconv.Itoa(index))
			if err == nil {
				results <- lease
			}
		}(index)
	}
	waitGroup.Wait()
	close(results)

	leases := make([]Lease, 0, 10)
	for lease := range results {
		leases = append(leases, lease)
	}
	if len(leases) != 10 {
		t.Fatalf("acquired leases = %d, want 10", len(leases))
	}
	for _, lease := range leases {
		lease.Release()
	}
}

func TestMemoryControllerValidatesConfigurationAndZeroDisablesLimit(t *testing.T) {
	if _, err := NewMemoryController(-1, 0); err == nil {
		t.Fatal("NewMemoryController() accepted negative global limit")
	}
	if _, err := NewMemoryController(0, -1); err == nil {
		t.Fatal("NewMemoryController() accepted negative API key limit")
	}
	controller := newTestController(t, 0, 0)
	for index := 0; index < 100; index++ {
		lease, err := controller.Acquire("client")
		if err != nil {
			t.Fatalf("disabled controller Acquire(%d) error = %v", index, err)
		}
		defer lease.Release()
	}
}

func newTestController(t *testing.T, globalMax, apiKeyMax int) *MemoryController {
	t.Helper()
	controller, err := NewMemoryController(globalMax, apiKeyMax)
	if err != nil {
		t.Fatalf("NewMemoryController() error = %v", err)
	}
	return controller
}

func acquireError(controller *MemoryController, keyID string) error {
	_, err := controller.Acquire(keyID)
	return err
}

func assertConcurrencyError(t *testing.T, err error, scope Scope) {
	t.Helper()
	var concurrencyErr *Error
	if !errors.As(err, &concurrencyErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if concurrencyErr.Scope != scope {
		t.Fatalf("scope = %q, want %q", concurrencyErr.Scope, scope)
	}
}
