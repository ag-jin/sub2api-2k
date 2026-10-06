//go:build unit

package bigmodel

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestSessionStore builds a store with an injected clock and a sweep
// interval long enough to never fire during one test, so expiry assertions
// only exercise the TTL check itself.
func newTestSessionStore(t *testing.T, clock *fakeClock) *SessionStore {
	t.Helper()
	store := newSessionStore(SessionTTL, clock.Now, time.Hour)
	t.Cleanup(store.Stop)
	return store
}

// TestSessionStoreTTLExpiry covers the store contract 03 relies on: a session
// is readable inside its TTL window and unreadable once it is older than the
// TTL, even before the background sweep runs.
func TestSessionStoreTTLExpiry(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	store := newTestSessionStore(t, clock)

	session := &OAuthSession{State: "state-1", RedirectURI: "http://127.0.0.1/cb", ProxyURL: "http://proxy:8080"}
	store.Set("sid-1", session)

	got, ok := store.Get("sid-1")
	require.True(t, ok)
	require.Equal(t, "state-1", got.State)
	require.Equal(t, "http://127.0.0.1/cb", got.RedirectURI)
	require.Equal(t, "http://proxy:8080", got.ProxyURL)
	require.False(t, got.CreatedAt.IsZero(), "Set must stamp CreatedAt for callers that leave it zero")

	clock.Advance(SessionTTL - time.Second)
	_, ok = store.Get("sid-1")
	require.True(t, ok, "session inside the TTL window must stay readable")

	clock.Advance(2 * time.Second)
	_, ok = store.Get("sid-1")
	require.False(t, ok, "session older than the TTL must not be readable")

	// An explicit CreatedAt is honoured rather than overwritten.
	store.Set("sid-old", &OAuthSession{State: "old", CreatedAt: clock.Now().Add(-2 * SessionTTL)})
	_, ok = store.Get("sid-old")
	require.False(t, ok, "a session already older than the TTL must not be readable")
}

// TestSessionStoreDeleteAndMissing covers the negative reads: a deleted session
// and an unknown id are both unreadable and never panic.
func TestSessionStoreDeleteAndMissing(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	store := newTestSessionStore(t, clock)

	store.Set("sid", &OAuthSession{State: "s"})
	_, ok := store.Get("sid")
	require.True(t, ok)

	store.Delete("sid")
	_, ok = store.Get("sid")
	require.False(t, ok, "a deleted session must not be readable")

	_, ok = store.Get("never-set")
	require.False(t, ok)

	// Deleting twice and setting a nil session are no-ops, not panics.
	store.Delete("sid")
	store.Set("nil-session", nil)
	_, ok = store.Get("nil-session")
	require.False(t, ok)
}

// TestSessionStoreSweepDropsExpiredEntries asserts the background sweep (the
// goroutine driven by the real ticker) removes expired sessions so the map does
// not grow without bound; the sweep is invoked directly to keep the test
// deterministic.
func TestSessionStoreSweepDropsExpiredEntries(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	store := newTestSessionStore(t, clock)

	store.Set("expired", &OAuthSession{State: "a"})
	store.Set("live", &OAuthSession{State: "b"})

	clock.Advance(SessionTTL + time.Second)
	// Refresh "live" so it stays inside the window after the jump.
	store.Set("live", &OAuthSession{State: "b"})

	store.sweep()

	store.mu.RLock()
	_, expiredStillStored := store.sessions["expired"]
	_, liveStillStored := store.sessions["live"]
	store.mu.RUnlock()

	require.False(t, expiredStillStored, "the sweep must drop expired sessions from the map")
	require.True(t, liveStillStored, "the sweep must keep live sessions")
}

// TestSessionStoreCleanupTickerRemovesExpired proves the store actually runs
// its own cleanup goroutine, using a short TTL and sweep interval instead of
// the production values.
func TestSessionStoreCleanupTickerRemovesExpired(t *testing.T) {
	t.Parallel()

	store := newSessionStore(20*time.Millisecond, time.Now, 5*time.Millisecond)
	t.Cleanup(store.Stop)

	store.Set("sid", &OAuthSession{State: "s"})
	require.Eventually(t, func() bool {
		store.mu.RLock()
		defer store.mu.RUnlock()
		_, ok := store.sessions["sid"]
		return !ok
	}, 2*time.Second, 5*time.Millisecond, "cleanup goroutine must evict expired sessions")
}

// TestSessionStoreConcurrentAccess exercises Set/Get/Delete from many
// goroutines; it is meaningful under -race and must never deadlock.
func TestSessionStoreConcurrentAccess(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	store := newTestSessionStore(t, clock)

	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			id := "sid-" + strconv.Itoa(worker%4)
			for i := 0; i < 100; i++ {
				store.Set(id, &OAuthSession{State: id})
				_, _ = store.Get(id)
				if i%10 == 0 {
					store.Delete(id)
				}
			}
		}(worker)
	}
	wg.Wait()

	store.Set("sid-final", &OAuthSession{State: "final"})
	got, ok := store.Get("sid-final")
	require.True(t, ok)
	require.Equal(t, "final", got.State)
}

// TestSessionStoreStopIsIdempotent guards the wire lifecycle: Stop is called
// from shutdown paths that may run more than once.
func TestSessionStoreStopIsIdempotent(t *testing.T) {
	t.Parallel()

	store := newSessionStore(SessionTTL, time.Now, time.Hour)
	store.Stop()
	store.Stop()
}
