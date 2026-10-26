package guard

import (
	"fmt"
	"testing"
	"time"
)

// Successful authentication uses Allow -> Success -> deferred Release. The
// success path clears failure state while the request is still in flight, so
// the later Release must be the point that removes the now-idle entry.
func TestR8_SuccessfulAttemptsLeaveNoIdleEntries(t *testing.T) {
	th := NewFailureThrottle(3, time.Minute).WithoutConcurrencyCap()

	for i := 0; i < 6000; i++ {
		ip := fmt.Sprintf("198.51.%d.%d", i/256, i%256)
		if !th.Allow(ip) {
			t.Fatalf("Allow(%q) unexpectedly rejected a clean key", ip)
		}
		th.Success(ip)
		th.Release(ip)
	}

	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.fails) != 0 {
		t.Fatalf("idle throttle entries = %d, want 0 after successful Release", len(th.fails))
	}
}

// Release is also a success-path entry point. It must not leave a map above
// the documented hard cap merely because sweepLocked previously ran only from
// the failure path.
func TestR8_ReleaseBoundsIdleMap(t *testing.T) {
	th := NewFailureThrottle(3, time.Minute).WithoutConcurrencyCap()

	for i := 0; i < 5000; i++ {
		ip := fmt.Sprintf("203.0.%d.%d", i/256, i%256)
		if !th.Allow(ip) {
			t.Fatalf("Allow(%q) unexpectedly rejected", ip)
		}
		th.Release(ip)
	}

	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.fails) > 4096 {
		t.Fatalf("idle throttle map size = %d, want <= 4096", len(th.fails))
	}
}
