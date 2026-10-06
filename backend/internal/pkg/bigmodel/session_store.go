package bigmodel

import (
	"sync"
	"time"
)

const (
	// SessionTTL is how long an in-flight login session stays valid between the
	// login-URL step and the callback exchange (design M1: 10 minutes).
	SessionTTL = 10 * time.Minute
	// sessionSweepInterval is how often expired sessions are dropped from memory.
	sessionSweepInterval = 5 * time.Minute
)

// OAuthSession is the server-side state of one in-flight bigmodel login: the
// CSRF state the callback must echo, the redirect URI the exchange must resend,
// and the proxy the login was started with.
type OAuthSession struct {
	State       string
	RedirectURI string
	ProxyURL    string
	CreatedAt   time.Time
}

// SessionStore keeps in-flight login sessions in process memory under a TTL.
// Cross-instance Redis sharing is deferred to P4 (design M1); it plugs in
// behind Set/Get/Delete, which are the only accessors, so callers stay
// unaffected. Sessions are stored and returned by value: a caller can never
// alias the map's copy.
type SessionStore struct {
	mu         sync.RWMutex
	sessions   map[string]*OAuthSession
	ttl        time.Duration
	sweepEvery time.Duration
	now        func() time.Time
	stopOnce   sync.Once
	stopCh     chan struct{}
}

// NewSessionStore builds the production store: 10 minute TTL, process-local.
func NewSessionStore() *SessionStore {
	return newSessionStore(SessionTTL, time.Now, sessionSweepInterval)
}

// newSessionStore is the test seam: the clock and the sweep interval are
// injected so expiry is asserted without sleeping through real TTL windows.
func newSessionStore(ttl time.Duration, now func() time.Time, sweepEvery time.Duration) *SessionStore {
	if now == nil {
		now = time.Now
	}
	if sweepEvery <= 0 {
		sweepEvery = sessionSweepInterval
	}
	store := &SessionStore{
		sessions:   make(map[string]*OAuthSession),
		ttl:        ttl,
		sweepEvery: sweepEvery,
		now:        now,
		stopCh:     make(chan struct{}),
	}
	go store.cleanup()
	return store
}

// Set stores a session under sessionID. A nil session is ignored. CreatedAt is
// stamped with the store clock when the caller leaves it zero; an explicit
// value is kept so a session that is already past its TTL expires immediately.
func (s *SessionStore) Set(sessionID string, session *OAuthSession) {
	if s == nil || session == nil {
		return
	}
	stored := *session
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = &stored
}

// Get returns the session when it exists and is still inside the TTL window.
func (s *SessionStore) Get(sessionID string) (*OAuthSession, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if s.now().Sub(session.CreatedAt) > s.ttl {
		return nil, false
	}
	stored := *session
	return &stored, true
}

// Delete drops a session; deleting an unknown id is a no-op.
func (s *SessionStore) Delete(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// Stop halts the background sweep. It is idempotent.
func (s *SessionStore) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// cleanup drops expired sessions on a ticker until Stop.
func (s *SessionStore) cleanup() {
	ticker := time.NewTicker(s.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

// sweep removes every session older than the TTL.
func (s *SessionStore) sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for sessionID, session := range s.sessions {
		if now.Sub(session.CreatedAt) > s.ttl {
			delete(s.sessions, sessionID)
		}
	}
}
