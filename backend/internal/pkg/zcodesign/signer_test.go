//go:build unit

package zcodesign

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

const testOrigin = "https://open.bigmodel.cn"

func TestSignWritesTheProtocolHeaders(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	signer := NewSigner(testOrigin, "", 0, stub)
	header := http.Header{"X-Custom": []string{"keep-me"}}
	sessionID := goldenSessionID

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, sessionID, header))

	assert.Equal(t, []string{
		"X-App-Id", "X-Client-Nonce", "X-Client-Pow", "X-Client-Sig",
		"X-Client-Ts", "X-Client-Version", "X-Custom", "X-Session-Id",
	}, slices.Sorted(maps.Keys(header)))
	assert.Equal(t, []string{"keep-me"}, header.Values("X-Custom"))
	assert.Equal(t, protocolAppID, header.Get(HeaderAppID))
	assert.Equal(t, protocolDefaultVersion, header.Get(HeaderClientVersion), "an empty constructor version falls back to the protocol default")
	assert.Equal(t, sessionID, header.Get(HeaderSessionID))

	// X-Client-Ts is the live wall clock in milliseconds.
	ts := header.Get(HeaderClientTs)
	tsMillis, err := strconv.ParseInt(ts, 10, 64)
	require.NoError(t, err, "X-Client-Ts must be a decimal millisecond timestamp")
	assert.Len(t, ts, 13)
	assert.WithinDuration(t, time.Now(), time.UnixMilli(tsMillis), 10*time.Second)

	// X-Client-Nonce is 16 random bytes, hex encoded.
	nonce := header.Get(HeaderClientNonce)
	assert.Len(t, nonce, 2*signNonceBytes)
	assert.Regexp(t, "^[0-9a-f]{32}$", nonce)

	// X-Client-Pow is the nonce plus an 8-hex counter that solves the
	// 8-bit challenge derived from the very values in the headers.
	assert.Regexp(t, "^"+nonce+"[0-9a-f]{8}$", header.Get(HeaderClientPow))
	challenge := hex.EncodeToString(sha256Sum(goldenAPIKeyID + "\n" + protocolAppID + "\n" + sessionID + "\n" + ts)[:16])
	digest := sha256Sum(challenge + "\n" + header.Get(HeaderClientPow))
	assert.True(t, powSatisfied(digest, 8), "the header PoW must satisfy the configured difficulty")

	// X-Client-Sig must verify with the handshake's public key over the message
	// rebuilt from the header values: an independent Ed25519 verification.
	assertGoldenSignature(t, header)

	assert.Equal(t, 1, stub.count(), "one request costs exactly one handshake")
	assert.Equal(t, []string{testOrigin + "/api/paas/c1f3a7e2/v2/client"}, stub.requestedURLs(),
		"the handshake must go to the configured origin's handshake endpoint")
}

func TestSignReusesTheCachedKeyAndResignsPerRequest(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	signer := NewSigner(testOrigin, "", 0, stub)

	first := http.Header{}
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, first))
	second := http.Header{}
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, "another-session", second))

	assert.Equal(t, 1, stub.count(), "the second request must reuse the cached private key")
	assertGoldenSignature(t, first)
	assertGoldenSignature(t, second)
	assert.NotEqual(t, first.Get(HeaderClientTs)+first.Get(HeaderClientNonce), second.Get(HeaderClientTs)+second.Get(HeaderClientNonce))
	assert.Equal(t, "another-session", second.Get(HeaderSessionID))
	assert.NotEqual(t, first.Get(HeaderClientSig), second.Get(HeaderClientSig), "a different session id must produce a different signature")
}

func TestSignNeverWritesHeadersOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("transport failure", func(t *testing.T) {
		t.Parallel()

		stub := newStubHandshake(t)
		stub.failWithKey(errors.New("connection refused"))
		signer := NewSigner(testOrigin, "", 0, stub)
		header := http.Header{"X-Custom": []string{"keep-me"}}

		err := signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, header)
		require.Error(t, err)
		var handshakeErr *HandshakeError
		require.ErrorAs(t, err, &handshakeErr, "the handshake classification must survive the signer")
		assertNoSignatureHeaders(t, header)
	})

	t.Run("business rejection", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":401,"msg":"bad key"}`)
		}))
		t.Cleanup(server.Close)

		signer := NewSigner(testOrigin, "", 0, server.Client())
		header := http.Header{}

		err := signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, header)
		requireHandshakeError(t, err, HandshakeErrorKindBusinessCode)
		assertNoSignatureHeaders(t, header)
	})

	t.Run("malformed api key", func(t *testing.T) {
		t.Parallel()

		stub := newStubHandshake(t)
		signer := NewSigner(testOrigin, "", 0, stub)
		header := http.Header{}

		err := signer.Sign(context.Background(), "no-dot-at-all", goldenSessionID, header)
		require.Error(t, err)
		var handshakeErr *HandshakeError
		require.ErrorAs(t, err, &handshakeErr)
		assert.Equal(t, HandshakeErrorKindInvalidAPIKey, handshakeErr.Kind)
		assertNoSignatureHeaders(t, header)
		assert.Zero(t, stub.count(), "a malformed credential must not reach the upstream")
	})

	t.Run("nil header", func(t *testing.T) {
		t.Parallel()

		stub := newStubHandshake(t)
		signer := NewSigner(testOrigin, "", 0, stub)

		require.Error(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, nil))
	})
}

func TestSignKeyCacheTTL(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	clock := newFakeClock()
	signer := NewSigner(testOrigin, "", time.Hour, stub)
	signer.now = clock.Now

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	clock.Advance(59 * time.Minute)
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 1, stub.count(), "a key inside its TTL must be reused")

	clock.Advance(2 * time.Hour)
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 2, stub.count(), "a key past its TTL must be handshaked again")
}

func TestSignKeyCacheDefaultsToTheProtocolTTL(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	clock := newFakeClock()
	signer := NewSigner(testOrigin, "", 0, stub)
	signer.now = clock.Now

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	clock.Advance(DefaultSignKeyTTL - time.Minute)
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 1, stub.count(), "keyTTL <= 0 must fall back to the 24h protocol default")

	clock.Advance(2 * time.Minute)
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 2, stub.count())
}

func TestSignCacheIsKeyedByAPIKeyIDAndInvalidateIsScoped(t *testing.T) {
	t.Parallel()

	const otherAPIKey = "aaaaaaaaaaaaaaa1.other-secret"
	stub := newStubHandshake(t)
	signer := NewSigner(testOrigin, "", time.Hour, stub)

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	require.NoError(t, signer.Sign(context.Background(), otherAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 2, stub.count(), "each api key id gets its own cache entry")

	signer.Invalidate(goldenAPIKeyID)
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 3, stub.count(), "Invalidate must force a re-handshake on the next Sign")

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	require.NoError(t, signer.Sign(context.Background(), otherAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 3, stub.count(), "Invalidate touches one api key id only, the new key stays cached")
}

func TestSignConcurrentColdStartCollapsesToOneHandshake(t *testing.T) {
	t.Parallel()

	const callers = 8
	stub := newStubHandshake(t)
	stub.block()
	signer := NewSigner(testOrigin, "", time.Hour, stub)

	headers := make([]http.Header, callers)
	errs := make([]error, callers)
	var waitGroup sync.WaitGroup
	for i := range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			headers[i] = http.Header{}
			errs[i] = signer.Sign(context.Background(), goldenAPIKey, fmt.Sprintf("session-%d", i), headers[i])
		}()
	}

	stub.waitForCalls(t, 1)
	// Give the remaining callers time to join the singleflight group before the
	// handshake completes; a caller that arrives later still finds the cached key.
	time.Sleep(50 * time.Millisecond)
	stub.unblock()
	waitGroup.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		assertGoldenSignature(t, headers[i])
	}
	assert.Equal(t, 1, stub.count(), "concurrent cold starts must share a single handshake")
}

func TestSignBackoffAfterHandshakeFailure(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	stub := newStubHandshake(t)
	stub.failWithKey(errors.New("handshake endpoint down"))
	signer := NewSigner(testOrigin, "", time.Hour, stub)
	signer.now = clock.Now

	err := signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{})
	require.Error(t, err)
	assert.Equal(t, 1, stub.count())

	err = signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHandshakeBackoff, "a failed handshake starts the per-key backoff window")
	assert.Equal(t, 1, stub.count(), "the backoff window must not attempt a handshake")

	clock.Advance(DefaultHandshakeBackoff - time.Second)
	err = signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{})
	assert.ErrorIs(t, err, ErrHandshakeBackoff, "the window lasts sign_handshake_backoff_seconds")
	assert.Equal(t, 1, stub.count())

	// Past the window the handshake is attempted again; this attempt fails too
	// and restarts the window.
	clock.Advance(2 * time.Second)
	err = signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrHandshakeBackoff)
	assert.Equal(t, 2, stub.count())

	// A recovered upstream serves the next request as soon as the window has
	// passed, and the successful handshake leaves no window behind.
	clock.Advance(DefaultHandshakeBackoff)
	stub.mu.Lock()
	stub.failFor = nil
	stub.mu.Unlock()
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 3, stub.count())
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 3, stub.count(), "a healthy signer must not keep a backoff window around")
}

func TestSignBackoffIsPerAPIKey(t *testing.T) {
	t.Parallel()

	const otherAPIKey = "aaaaaaaaaaaaaaa1.other-secret"
	stub := newStubHandshake(t)
	stub.failOnlyKey(goldenAPIKey, errors.New("this credential only"))
	signer := NewSigner(testOrigin, "", time.Hour, stub)

	require.Error(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 1, stub.count())

	// The other credential is untouched by the failed key's backoff.
	require.NoError(t, signer.Sign(context.Background(), otherAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 2, stub.count())

	err := signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHandshakeBackoff)
	assert.Equal(t, 2, stub.count())
}

func TestInvalidateDuringHandshakeDoesNotServeTheStaleKey(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	stub.block()
	signer := NewSigner(testOrigin, "", time.Hour, stub)

	done := make(chan error, 1)
	go func() {
		done <- signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{})
	}()

	stub.waitForCalls(t, 1)
	// The self-heal path (ticket 23) invalidates while the request that
	// triggered it is still waiting for its own handshake.
	signer.Invalidate(goldenAPIKeyID)
	stub.unblock()
	require.NoError(t, <-done)

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 2, stub.count(), "a key handshaked before Invalidate must not be cached after it")
}

func TestSignerSetOptionsHotUpdate(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	signer := NewSigner(testOrigin, "", time.Hour, stub)

	// Ticket 28 reads these four knobs from gateway settings and applies them
	// without restarting the process.
	signer.SetOptions(SignerOptions{ClientVersion: "1.2.3", KeyTTL: time.Hour, PowBits: 0, HandshakeBackoff: time.Minute})

	header := http.Header{}
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, header))
	assert.Equal(t, "1.2.3", header.Get(HeaderClientVersion))
	assert.Equal(t, header.Get(HeaderClientNonce)+"00000000", header.Get(HeaderClientPow),
		"pow_bits = 0 must accept the first candidate")
	assertGoldenSignature(t, header)

	// Changing the version changes what is signed but needs no new handshake.
	signer.SetOptions(SignerOptions{ClientVersion: "2.0.0", KeyTTL: 0, PowBits: 8, HandshakeBackoff: 0})
	assert.Equal(t, DefaultPowBits, 8)
	assert.Equal(t, DefaultHandshakeBackoff, 30*time.Second)
	assert.Equal(t, DefaultSignKeyTTL, 24*time.Hour)

	next := http.Header{}
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, next))
	assert.Equal(t, "2.0.0", next.Get(HeaderClientVersion))
	assert.NotEqual(t, header.Get(HeaderClientSig), next.Get(HeaderClientSig))
	assertGoldenSignature(t, next)
	assert.Equal(t, 1, stub.count(), "protocol knobs are not part of the private key cache")
}

// fakeClock is the signer's clock seam: tests move time forward instead of
// sleeping on TTLs and backoff windows.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// assertGoldenSignature verifies X-Client-Sig with the ticket 19 golden public// key over the message reassembled from the header values, exactly as the
// upstream would.
func assertGoldenSignature(t *testing.T, header http.Header) {
	t.Helper()

	publicKey, err := base64.StdEncoding.DecodeString(goldenPubKeyB64)
	require.NoError(t, err)
	signature, err := base64.StdEncoding.DecodeString(header.Get(HeaderClientSig))
	require.NoError(t, err)

	message := signRequestMessage(goldenAPIKeyID, header.Get(HeaderClientTs), header.Get(HeaderClientVersion),
		header.Get(HeaderSessionID), header.Get(HeaderClientNonce))
	assert.True(t, ed25519.Verify(publicKey, []byte(message), signature),
		"X-Client-Sig must verify over the signed message rebuilt from the headers")
}

// assertNoSignatureHeaders is the atomicity assertion: a failed Sign must leave
// the request exactly as it found it.
func assertNoSignatureHeaders(t *testing.T, header http.Header) {
	t.Helper()

	for _, name := range []string{
		HeaderClientTs, HeaderClientVersion, HeaderClientSig,
		HeaderClientNonce, HeaderClientPow, HeaderSessionID, HeaderAppID,
	} {
		assert.Empty(t, header.Values(name), "%s must not be written by a failed Sign", name)
	}
}

// stubHandshake is the handshake upstream of the signer tests. It answers every
// credential with the ticket 19 golden Ed25519 key, sealed for that credential
// (so the golden public key can always verify the signature), counts every
// attempt on the client side, records the requested URLs and can fail or block
// on demand.
type stubHandshake struct {
	server *httptest.Server

	mu      sync.Mutex
	calls   int
	urls    []string
	failFor func(apiKey string) error
	release chan struct{}
}

func newStubHandshake(t *testing.T) *stubHandshake {
	t.Helper()

	stub := &stubHandshake{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		release := stub.release
		stub.mu.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sealGoldenKeyFor(r.Header.Get("Authorization")))
	}))
	// Cleanups run last-in-first-out: unblock the handler before closing the
	// server so a stalled request cannot hold the shutdown hostage.
	t.Cleanup(stub.server.Close)
	t.Cleanup(func() { stub.unblock() })
	return stub
}

func (s *stubHandshake) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.urls = append(s.urls, req.URL.String())
	failFor := s.failFor
	s.mu.Unlock()

	if failFor != nil {
		if err := failFor(req.Header.Get("Authorization")); err != nil {
			return nil, err
		}
	}
	// Forward the request the signer built (same path, headers and body) to the
	// test server, so the test never touches the real origin.
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(s.server.URL, "http://")
	return s.server.Client().Do(req)
}

// sealGoldenKeyFor builds the upstream reply for one credential: the golden
// Ed25519 private key as PKCS#8, base64 wrapped and sealed with that
// credential's own HKDF key and aad (the upstream binds the cipher to the api
// key id). It is written here straight from the protocol description (RFC 5869
// HKDF-SHA256 + AES-256-GCM) and must stay independent of the code under test.
// It returns a transport-visible body even for a malformed credential, so it
// can run on the test server's goroutine without touching *testing.T.
func sealGoldenKeyFor(apiKey string) string {
	apiKeyID, apiKeySecret, found := strings.Cut(apiKey, ".")
	if !found {
		return `{"code":401,"msg":"malformed credential"}`
	}
	reader := hkdf.New(sha256.New, []byte(apiKeySecret), []byte(protocolKDFSalt), []byte(protocolKDFInfoEd25519Priv))
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return `{"code":500,"msg":"hkdf"}`
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return `{"code":500,"msg":"cipher"}`
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return `{"code":500,"msg":"gcm"}`
	}
	pkcs8 := mustHexNoTest("302e020100300506032b657004220420" + goldenSeedHex)
	iv := mustHexNoTest("00112233445566778899aabb")
	sealed := gcm.Seal(nil, iv, []byte(base64.StdEncoding.EncodeToString(pkcs8)), []byte(apiKeyID))
	privateCipher := base64.StdEncoding.EncodeToString(append(append([]byte{}, iv...), sealed...))
	return fmt.Sprintf(`{"code":200,"data":{"privateCipher":%q}}`, privateCipher)
}

// mustHexNoTest is mustHex without the *testing.T plumbing, for callers that
// run outside the test goroutine. The input is a compile-time constant, so a
// decode failure means the test source itself is broken.
func mustHexNoTest(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

func (s *stubHandshake) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// failWithKey makes every attempt fail unless failFor says otherwise.
func (s *stubHandshake) failWithKey(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFor = func(string) error { return err }
}

// failOnlyKey fails the attempts of one credential and leaves the others intact.
func (s *stubHandshake) failOnlyKey(apiKey string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFor = func(requested string) error {
		if requested == apiKey {
			return err
		}
		return nil
	}
}

func (s *stubHandshake) requestedURLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.urls)
}

// block makes every following handshake wait until unblock is called.
func (s *stubHandshake) block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release = make(chan struct{})
}

func (s *stubHandshake) unblock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release != nil {
		close(s.release)
		s.release = nil
	}
}

// waitForCalls blocks until the stub has served at least n attempts.
func (s *stubHandshake) waitForCalls(t *testing.T, n int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.count() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Failf(t, "handshake attempts did not reach the expected count", "want >= %d, got %d", n, s.count())
}
