package zcodesign

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Protocol defaults. Ticket 28 reads the same knobs from
// gateway.zhipu (design M3.1(e)) and applies them with Signer.SetOptions, so
// the fallbacks live here once instead of in the config layer.
const (
	// DefaultSignKeyTTL is how long a handshake private key stays cached
	// without a re-handshake (design M3: 24h, process-local, no persistence).
	DefaultSignKeyTTL = 24 * time.Hour

	// DefaultPowBits is the PoW difficulty of the official client.
	DefaultPowBits = defaultPowBits

	// DefaultHandshakeBackoff is the per-key minimum interval between handshake
	// attempts after a failure (design M3.1(a): handshake storm protection).
	DefaultHandshakeBackoff = 30 * time.Second
)

// ErrHandshakeBackoff is returned by Sign while the per-apiKeyID handshake
// backoff of a recent failure is still running: the request must fail (and be
// handled by the gateway's fail policy) instead of hammering the handshake
// endpoint (design M3.1(a)). Its error wrapping keeps the previous handshake
// classification out of reach: there is nothing to classify, no attempt was
// made.
var ErrHandshakeBackoff = errors.New("zcodesign: per-key handshake backoff is active")

// SignerOptions are the protocol knobs of a Signer. Ticket 28 reads them from
// the gateway settings (design M3.1(e)) and applies them with SetOptions, so a
// config change takes effect without a restart. Every field documents how its
// zero value is read.
type SignerOptions struct {
	// ClientVersion is the signed X-Client-Version. Empty means
	// DefaultClientVersion (0.16.9).
	ClientVersion string
	// KeyTTL is how long a handshake private key stays cached. <= 0 means
	// DefaultSignKeyTTL (24h).
	KeyTTL time.Duration
	// PowBits is the PoW difficulty in leading zero bits. Negative means
	// DefaultPowBits (8); 0 is a valid setting that accepts the first candidate.
	PowBits int
	// HandshakeBackoff is the per-apiKeyID minimum interval between handshake
	// attempts after a failure. <= 0 means DefaultHandshakeBackoff (30s).
	HandshakeBackoff time.Duration
}

// cachedSigningKey is one apiKeyID's cached private key: the handshake result
// plus the moment it stops being usable. There is no persistence and no
// background refresh; a restart simply handshakes again.
type cachedSigningKey struct {
	privateKey ed25519.PrivateKey
	expiresAt  time.Time
}

// Signer signs upstream requests with the ZCode client signature V4 headers. It
// owns the per-apiKeyID private key cache, the handshake concurrency and the
// handshake backoff; the gateway (tickets 22/23/24) only calls Sign/Invalidate
// and applies its own fail policy.
//
// A Signer is safe for concurrent use. All methods are goroutine safe.
type Signer struct {
	origin string
	doer   HTTPDoer

	// now is the clock seam: production uses time.Now, tests move a fake clock
	// to exercise the cache TTL and the handshake backoff deterministically.
	now func() time.Time

	mu               sync.Mutex
	keys             map[string]cachedSigningKey
	generations      map[string]uint64
	handshakeBlocked map[string]time.Time
	clientVersion    string
	keyTTL           time.Duration
	powBits          int
	handshakeBackoff time.Duration

	// flight collapses concurrent handshakes of the same apiKeyID into one.
	flight singleflight.Group
}

// NewSigner builds a signer for one upstream origin (the data plane origin,
// e.g. "https://open.bigmodel.cn"). An empty clientVersion falls back to
// DefaultClientVersion; a keyTTL <= 0 falls back to DefaultSignKeyTTL. The doer
// is the transport used for handshakes.
func NewSigner(origin, clientVersion string, keyTTL time.Duration, doer HTTPDoer) *Signer {
	signer := &Signer{
		origin:           origin,
		doer:             doer,
		now:              time.Now,
		keys:             make(map[string]cachedSigningKey),
		generations:      make(map[string]uint64),
		handshakeBlocked: make(map[string]time.Time),
	}
	signer.SetOptions(SignerOptions{ClientVersion: clientVersion, KeyTTL: keyTTL, PowBits: -1})
	return signer
}

// SetOptions applies the protocol knobs and takes effect on the next Sign. It
// deliberately leaves the cached private keys alone: the version, the TTL, the
// difficulty and the backoff are not part of the key material.
func (s *Signer) SetOptions(options SignerOptions) {
	clientVersion := strings.TrimSpace(options.ClientVersion)
	if clientVersion == "" {
		clientVersion = DefaultClientVersion
	}
	keyTTL := options.KeyTTL
	if keyTTL <= 0 {
		keyTTL = DefaultSignKeyTTL
	}
	powBits := options.PowBits
	if powBits < 0 {
		powBits = DefaultPowBits
	}
	handshakeBackoff := options.HandshakeBackoff
	if handshakeBackoff <= 0 {
		handshakeBackoff = DefaultHandshakeBackoff
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientVersion = clientVersion
	s.keyTTL = keyTTL
	s.powBits = powBits
	s.handshakeBackoff = handshakeBackoff
}

// Sign computes the seven signature V4 headers for one request and writes them
// into h:
//
//	X-Client-Ts, X-Client-Version, X-Client-Sig, X-Session-Id,
//	X-Client-Nonce, X-App-Id: zcode, X-Client-Pow
//
// The private key is taken from the per-apiKeyID cache and, on a miss, from one
// handshake (concurrent misses share a single handshake). Every failure returns
// an error with no header written at all: the caller decides between fail-open
// and fail-closed, it never sees a half-signed request.
func (s *Signer) Sign(ctx context.Context, apiKey, sessionID string, h http.Header) error {
	apiKeyID, _, err := splitAPIKey(apiKey)
	if err != nil {
		return fmt.Errorf("zcodesign sign: %w", err)
	}
	if h == nil {
		return errors.New("zcodesign sign: nil header set")
	}
	privateKey, err := s.signingKey(ctx, apiKey, apiKeyID)
	if err != nil {
		return err
	}
	nonce, err := newSignNonce()
	if err != nil {
		return fmt.Errorf("zcodesign sign: %w", err)
	}
	clientVersion, powBits := s.options()
	values, err := computeSignHeaders(privateKey, signRequestInput{
		APIKeyID:       apiKeyID,
		SessionID:      sessionID,
		Ts:             strconv.FormatInt(s.now().UnixMilli(), 10),
		ClientVersion:  clientVersion,
		Nonce:          nonce,
		PowBits:        powBits,
		MaxPowAttempts: powMaxAttempts,
	})
	if err != nil {
		return fmt.Errorf("zcodesign sign: %w", err)
	}
	writeSignHeaders(h, values)
	return nil
}

// Invalidate drops the cached private key of apiKeyID, so the next Sign
// handshakes again. It is the entry point of the VERIFY_* self-heal path
// (ticket 23).
//
// Invalidate does not clear a running handshake backoff: a key that keeps
// producing handshake failures must not be able to trigger a handshake per
// request. The generation bump makes sure a handshake that was already in
// flight when Invalidate arrived cannot silently repopulate the cache with the
// key the caller just rejected.
func (s *Signer) Invalidate(apiKeyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, apiKeyID)
	s.generations[apiKeyID]++
}

// signingKey returns the private key of apiKeyID, handshaking only on a cache
// miss. Concurrent misses of the same apiKeyID are collapsed by singleflight:
// the upstream sees one handshake, not one per request.
func (s *Signer) signingKey(ctx context.Context, apiKey, apiKeyID string) (ed25519.PrivateKey, error) {
	if privateKey, ok := s.cachedKey(apiKeyID); ok {
		return privateKey, nil
	}
	value, err, _ := s.flight.Do(apiKeyID, func() (any, error) {
		// A concurrent Sign may have completed the handshake while this one was
		// queueing for the flight.
		if privateKey, ok := s.cachedKey(apiKeyID); ok {
			return privateKey, nil
		}
		if blockedUntil, blocked := s.handshakeBlockedUntil(apiKeyID); blocked {
			return nil, fmt.Errorf("%w until %s (api key %s)", ErrHandshakeBackoff, blockedUntil.UTC().Format(time.RFC3339), apiKeyID)
		}
		generation := s.keyGeneration(apiKeyID)
		result, err := handshake(ctx, apiKey, s.origin, s.doer)
		if err != nil {
			s.recordHandshakeFailure(apiKeyID)
			return nil, fmt.Errorf("zcodesign sign: %w", err)
		}
		s.storeKey(apiKeyID, generation, result.PrivateKey)
		return result.PrivateKey, nil
	})
	if err != nil {
		return nil, err
	}
	privateKey, ok := value.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("zcodesign sign: unexpected singleflight result type")
	}
	return privateKey, nil
}

// cachedKey returns the cached private key of apiKeyID while it is still inside
// its TTL.
func (s *Signer) cachedKey(apiKeyID string) (ed25519.PrivateKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cached, ok := s.keys[apiKeyID]
	if !ok || !s.now().Before(cached.expiresAt) {
		return nil, false
	}
	return cached.privateKey, true
}

// storeKey caches a freshly handshaked private key for its TTL, unless the key
// was invalidated while the handshake was in flight (the generation moved on),
// in which case the caller keeps its key for this one request but nobody else
// will ever be served it.
func (s *Signer) storeKey(apiKeyID string, generation uint64, privateKey ed25519.PrivateKey) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.generations[apiKeyID] != generation {
		return
	}
	s.keys[apiKeyID] = cachedSigningKey{privateKey: privateKey, expiresAt: s.now().Add(s.keyTTL)}
}

// keyGeneration returns the current invalidation generation of apiKeyID.
func (s *Signer) keyGeneration(apiKeyID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generations[apiKeyID]
}

// handshakeBlockedUntil reports whether the per-key backoff of a recent failed
// handshake is still running.
func (s *Signer) handshakeBlockedUntil(apiKeyID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	blockedUntil, ok := s.handshakeBlocked[apiKeyID]
	if !ok || !s.now().Before(blockedUntil) {
		return time.Time{}, false
	}
	return blockedUntil, true
}

// recordHandshakeFailure starts (or restarts) the per-key backoff window.
func (s *Signer) recordHandshakeFailure(apiKeyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handshakeBlocked[apiKeyID] = s.now().Add(s.handshakeBackoff)
}

// options snapshots the current signing knobs.
func (s *Signer) options() (clientVersion string, powBits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientVersion, s.powBits
}
