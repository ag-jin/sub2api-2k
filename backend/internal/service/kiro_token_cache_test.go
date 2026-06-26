package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

// TestTokenCache_HitMissExpireInvalidate verifies the in-memory access-token
// cache that eliminates a per-request AWS SSO refresh (the ~0.65s first-token
// latency source). It exercises store → hit → expiry-miss → invalidate without
// touching the network.
func TestTokenCache_HitMissExpireInvalidate(t *testing.T) {
	s := NewKiroGatewayService(nil)
	const acct int64 = 4242

	// 1) Cold cache: applying to a fresh cred must miss.
	cred := &kiro.KiroCredential{RefreshToken: "rt"}
	if s.applyCachedToken(acct, cred) {
		t.Fatal("expected cache miss on cold cache")
	}

	// 2) Store a fresh token (expires in 1h) and confirm a later request hits it
	//    WITHOUT needing a refresh.
	fresh := &kiro.KiroCredential{
		AccessToken: "AT-fresh",
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	s.storeCachedToken(acct, fresh)

	got := &kiro.KiroCredential{RefreshToken: "rt"}
	if !s.applyCachedToken(acct, got) {
		t.Fatal("expected cache hit after store")
	}
	if got.AccessToken != "AT-fresh" {
		t.Fatalf("cached token not applied: got %q", got.AccessToken)
	}

	// 3) Expired token (within the 5-min buffer) must be treated as a miss so the
	//    caller refreshes.
	s.storeCachedToken(acct, &kiro.KiroCredential{
		AccessToken: "AT-stale",
		ExpiresAt:   time.Now().Add(2 * time.Minute).Format(time.RFC3339), // < 5-min buffer
	})
	stale := &kiro.KiroCredential{RefreshToken: "rt"}
	if s.applyCachedToken(acct, stale) {
		t.Fatal("expected miss for token within 5-min expiry buffer")
	}

	// 4) Invalidate must drop the entry entirely.
	s.storeCachedToken(acct, &kiro.KiroCredential{
		AccessToken: "AT-x",
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if !s.applyCachedToken(acct, &kiro.KiroCredential{}) {
		t.Fatal("precondition: expected hit before invalidate")
	}
	s.invalidateCachedToken(acct)
	if s.applyCachedToken(acct, &kiro.KiroCredential{}) {
		t.Fatal("expected miss after invalidate")
	}

	// 5) storeCachedToken must ignore an empty access token.
	s.storeCachedToken(acct, &kiro.KiroCredential{AccessToken: ""})
	if s.applyCachedToken(acct, &kiro.KiroCredential{}) {
		t.Fatal("empty token should not be cached")
	}
}

// TestTokenCache_PerAccountIsolation ensures cached tokens never leak across
// accounts (each account keeps its own fingerprint/credentials).
func TestTokenCache_PerAccountIsolation(t *testing.T) {
	s := NewKiroGatewayService(nil)
	exp := time.Now().Add(time.Hour).Format(time.RFC3339)
	s.storeCachedToken(1, &kiro.KiroCredential{AccessToken: "AT-1", ExpiresAt: exp})
	s.storeCachedToken(2, &kiro.KiroCredential{AccessToken: "AT-2", ExpiresAt: exp})

	c1 := &kiro.KiroCredential{}
	c2 := &kiro.KiroCredential{}
	if !s.applyCachedToken(1, c1) || c1.AccessToken != "AT-1" {
		t.Fatalf("account 1 token wrong: %q", c1.AccessToken)
	}
	if !s.applyCachedToken(2, c2) || c2.AccessToken != "AT-2" {
		t.Fatalf("account 2 token wrong: %q", c2.AccessToken)
	}
}
