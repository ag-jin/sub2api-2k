package zcodesign

import "time"

// KeyStatus is the read-only projection of one apiKeyID's handshake private key
// cache. The admin sign status endpoint (ticket 28, design M6「渠道签名 V4 配置
// UI」) reads it to answer three questions per account: is a private key cached,
// when was the last successful handshake, and how many handshakes failed in a
// row. It deliberately carries no key material and no credential.
type KeyStatus struct {
	// Cached reports whether a usable private key is cached right now (inside
	// its TTL, not invalidated).
	Cached bool
	// LastHandshakeAt is the last successful handshake of this process, zero
	// when the process never handshaked the key. It is kept after the cached key
	// expires, so the admin UI can show how stale the key is.
	LastHandshakeAt time.Time
	// ExpiresAt is when the cached private key stops being usable; zero when no
	// key is cached.
	ExpiresAt time.Time
	// ConsecutiveFailures counts handshake failures since the last success of
	// this apiKeyID. A request rejected by the per-key backoff window is not a
	// new failed handshake and does not increment it.
	ConsecutiveFailures int
}

// KeyStatus returns the cache state of one apiKeyID. Unknown keys and keys of
// other processes read as "not cached, never handshaked here" — the honest
// answer, since the cache is process-local by design (design M3).
func (s *Signer) KeyStatus(apiKeyID string) KeyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := KeyStatus{
		LastHandshakeAt:     s.lastHandshake[apiKeyID],
		ConsecutiveFailures: s.handshakeFailures[apiKeyID],
	}
	if cached, ok := s.keys[apiKeyID]; ok && s.now().Before(cached.expiresAt) {
		status.Cached = true
		status.ExpiresAt = cached.expiresAt
	}
	return status
}
