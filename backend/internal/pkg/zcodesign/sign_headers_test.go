//go:build unit

package zcodesign

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-request signature literals and golden vectors.
//
// Everything here is copied from the reference implementation
// (zcode-research/sign-v4.mjs:42-56, v4SignHeaders) and from the reverse
// engineering report (zcode-150-research FINAL-REPORT.md §五). The golden
// vectors were produced by a Node script that reuses that reference
// implementation with fixed inputs (Date.now / randomBytes replaced); the
// generation command and the full output are recorded in ticket 20 Comments.
// They are duplicated here on purpose: the assertions must not read the package
// constants they verify.
const (
	protocolHeaderClientTs      = "X-Client-Ts"
	protocolHeaderClientVersion = "X-Client-Version"
	protocolHeaderClientSig     = "X-Client-Sig"
	protocolHeaderClientNonce   = "X-Client-Nonce"
	protocolHeaderClientPow     = "X-Client-Pow"
	protocolHeaderSessionID     = "X-Session-Id"
	protocolHeaderAppID         = "X-App-Id"
	protocolAppID               = "zcode"

	// goldenSign* mirror the ticket 19 golden identity (same API key, same
	// Ed25519 seed) so both halves of the protocol are locked to one identity.
	goldenSessionID    = "4f9a2c6d-8b1e-4f7a-9c3d-2e5b7a1f8d40"
	goldenSignTS       = "1759700000000"
	goldenSignNonce    = "0123456789abcdef0123456789abcdef"
	goldenDriftVersion = "9.9.9"

	// hex(sha256("<apiKeyId>\nzcode\n<sessionId>\n<ts>"))[:32].
	goldenPowChallenge = "bb4e8082236a9b794ee44c73a1e73c35"

	// 8-bit PoW solution: candidate = nonce + 8-hex counter, the first counter
	// (s = 5) whose sha256("<h0>\n<candidate>") starts with a zero byte.
	goldenPowCandidate8 = "0123456789abcdef0123456789abcdef00000005"
	goldenPowDigest8    = "004d6d07717ec828783e6386e9c2251dd717aa2899e98c4e9e1394749eb28388"
	goldenPowAttempts8  = 6

	// 10-bit PoW solution (s = 320): exercises the non-byte-aligned branch.
	goldenPowCandidate10 = "0123456789abcdef0123456789abcdef00000140"
	goldenPowAttempts10  = 321

	// 0-bit PoW: the very first candidate solves trivially (pow_bits = 0 is the
	// configured "off" position, not an error).
	goldenPowCandidate0 = "0123456789abcdef0123456789abcdef00000000"

	goldenRequestSignMessage = "2f8c1d7a4b6e9031\n1759700000000\n0.16.9\n4f9a2c6d-8b1e-4f7a-9c3d-2e5b7a1f8d40\n0123456789abcdef0123456789abcdef"
	goldenSignSig            = "N+EAc9pJlliKx6EK7cwnPGCTlplg9Ib5mLJErvNgdpcj01cgsZtTZXLgvwVlUfiZeGVIp8ndak1fNzIHnVKKAg=="

	goldenDriftMessage = "2f8c1d7a4b6e9031\n1759700000000\n9.9.9\n4f9a2c6d-8b1e-4f7a-9c3d-2e5b7a1f8d40\n0123456789abcdef0123456789abcdef"
	goldenDriftSig     = "6xIaLQr+f1jR/ITzVdpkPJdf6veUvDthQMjh4ESKRQTsdhylJ1yDPggqsbpeiOcdHw1VF7E1DlZulkkPO6CfAg=="
)

func TestSignHeaderNamesMatchReference(t *testing.T) {
	t.Parallel()

	assert.Equal(t, protocolHeaderClientTs, HeaderClientTs)
	assert.Equal(t, protocolHeaderClientVersion, HeaderClientVersion)
	assert.Equal(t, protocolHeaderClientSig, HeaderClientSig)
	assert.Equal(t, protocolHeaderClientNonce, HeaderClientNonce)
	assert.Equal(t, protocolHeaderClientPow, HeaderClientPow)
	assert.Equal(t, protocolHeaderSessionID, HeaderSessionID)
	assert.Equal(t, protocolHeaderAppID, HeaderAppID)
	assert.Equal(t, protocolAppID, signAppID)
	assert.Equal(t, protocolDefaultVersion, DefaultClientVersion)
	assert.Equal(t, 8, defaultPowBits)
	assert.Equal(t, 16, signNonceBytes)
}

func TestPowChallengeGoldenVector(t *testing.T) {
	t.Parallel()

	challenge := powChallenge(goldenAPIKeyID, goldenSessionID, goldenSignTS)
	assert.Equal(t, goldenPowChallenge, challenge)
	// The first 16 bytes of the digest, not the whole 32: a full digest here
	// would still hash consistently but would never match the official client.
	assert.Len(t, challenge, 32)
	assert.Equal(t, goldenPowChallenge, hex.EncodeToString(sha256Sum(goldenAPIKeyID + "\n" + protocolAppID + "\n" + goldenSessionID + "\n" + goldenSignTS)[:16]))
}

func TestPowMessageLayout(t *testing.T) {
	t.Parallel()

	assert.Equal(t, goldenPowChallenge+"\n"+goldenPowCandidate8, powMessage(goldenPowChallenge, goldenPowCandidate8))
}

func TestSolvePowGoldenVector(t *testing.T) {
	t.Parallel()

	candidate, attempts, err := solvePow(goldenSignNonce, goldenPowChallenge, 8, powMaxAttempts)
	require.NoError(t, err)
	assert.Equal(t, goldenPowCandidate8, candidate)
	assert.Equal(t, uint64(goldenPowAttempts8), attempts)

	// Property: the returned candidate really solves the challenge, verified
	// here by an independent SHA-256 pass, and it is the *first* counter that
	// does (the reference implementation also stops at the lowest s).
	digest := sha256Sum(powMessage(goldenPowChallenge, candidate))
	assert.Equal(t, goldenPowDigest8, hex.EncodeToString(digest))
	assert.Zero(t, digest[0], "8-bit PoW requires a zero first byte")
	assert.True(t, powSatisfied(digest, 8))

	for s := 0; s < goldenPowAttempts8-1; s++ {
		early := fmt.Sprintf("%s%08x", goldenSignNonce, s)
		assert.False(t, powSatisfied(sha256Sum(powMessage(goldenPowChallenge, early)), 8),
			"counter %d must not solve the 8-bit challenge", s)
	}
	// The counter is 8 hex digits, zero padded: "00000005", not "5".
	assert.Equal(t, goldenSignNonce+"00000005", candidate)
}

func TestSolvePowNonByteAlignedBits(t *testing.T) {
	t.Parallel()

	candidate, attempts, err := solvePow(goldenSignNonce, goldenPowChallenge, 10, powMaxAttempts)
	require.NoError(t, err)
	assert.Equal(t, goldenPowCandidate10, candidate)
	assert.Equal(t, uint64(goldenPowAttempts10), attempts)
	assert.True(t, powSatisfied(sha256Sum(powMessage(goldenPowChallenge, candidate)), 10))
}

func TestSolvePowZeroBitsShortCircuits(t *testing.T) {
	t.Parallel()

	candidate, attempts, err := solvePow(goldenSignNonce, goldenPowChallenge, 0, powMaxAttempts)
	require.NoError(t, err)
	assert.Equal(t, goldenPowCandidate0, candidate)
	assert.Equal(t, uint64(1), attempts, "pow_bits = 0 must accept the first candidate")
}

func TestSolvePowRejectsInvalidBits(t *testing.T) {
	t.Parallel()

	for _, bits := range []int{-1, -8, 257, 1024} {
		t.Run(fmt.Sprintf("bits %d", bits), func(t *testing.T) {
			t.Parallel()

			_, _, err := solvePow(goldenSignNonce, goldenPowChallenge, bits, powMaxAttempts)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrPowUnsolved)
		})
	}
}

func TestSolvePowStopsAtAttemptBound(t *testing.T) {
	t.Parallel()

	// The bound is what keeps a hostile/unreachable difficulty from spinning
	// forever: with three attempts allowed, the golden 8-bit challenge (which
	// needs six) must fail rather than loop to 2^32.
	_, attempts, err := solvePow(goldenSignNonce, goldenPowChallenge, 8, 3)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPowUnsolved)
	assert.Equal(t, uint64(3), attempts, "the loop must stop exactly at the bound")

	// Sanity check on the boundary semantics: the same challenge solves with
	// the three-attempt bound when the difficulty is low enough.
	candidate, attempts, err := solvePow(goldenSignNonce, goldenPowChallenge, 0, 3)
	require.NoError(t, err)
	assert.Equal(t, goldenPowCandidate0, candidate)
	assert.Equal(t, uint64(1), attempts)
}

func TestSignRequestMessageLayout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		apiKeyID    string
		ts          string
		version     string
		sessionID   string
		nonce       string
		wantMessage string
	}{
		{
			name:        "golden identity and version",
			apiKeyID:    goldenAPIKeyID,
			ts:          goldenSignTS,
			version:     protocolDefaultVersion,
			sessionID:   goldenSessionID,
			nonce:       goldenSignNonce,
			wantMessage: goldenRequestSignMessage,
		},
		{
			name:        "official version drift is signed verbatim",
			apiKeyID:    goldenAPIKeyID,
			ts:          goldenSignTS,
			version:     goldenDriftVersion,
			sessionID:   goldenSessionID,
			nonce:       goldenSignNonce,
			wantMessage: goldenDriftMessage,
		},
		{
			name:        "field order is id, ts, version, session, nonce",
			apiKeyID:    "aaaa",
			ts:          "1",
			version:     "v2",
			sessionID:   "s3",
			nonce:       "n4",
			wantMessage: "aaaa\n1\nv2\ns3\nn4",
		},
		{
			name:        "empty session id keeps the separator",
			apiKeyID:    "aaaa",
			ts:          "1",
			version:     "0.16.9",
			sessionID:   "",
			nonce:       "n4",
			wantMessage: "aaaa\n1\n0.16.9\n\nn4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			message := signRequestMessage(tc.apiKeyID, tc.ts, tc.version, tc.sessionID, tc.nonce)
			assert.Equal(t, tc.wantMessage, message)

			// Negative control: swapping two adjacent signed fields must not
			// produce the same message (a lost separator would).
			swapped := signRequestMessage(tc.apiKeyID, tc.version, tc.ts, tc.sessionID, tc.nonce)
			if tc.ts != tc.version {
				assert.NotEqual(t, message, swapped)
			}
		})
	}
}

func TestComputeSignHeadersGoldenVector(t *testing.T) {
	t.Parallel()

	privateKey := ed25519.NewKeyFromSeed(mustHex(t, goldenSeedHex))
	values, err := computeSignHeaders(privateKey, signRequestInput{
		APIKeyID:       goldenAPIKeyID,
		SessionID:      goldenSessionID,
		Ts:             goldenSignTS,
		ClientVersion:  protocolDefaultVersion,
		Nonce:          goldenSignNonce,
		PowBits:        defaultPowBits,
		MaxPowAttempts: powMaxAttempts,
	})
	require.NoError(t, err)

	assert.Equal(t, signHeaderValues{
		Ts:        goldenSignTS,
		Version:   protocolDefaultVersion,
		Nonce:     goldenSignNonce,
		Pow:       goldenPowCandidate8,
		Sig:       goldenSignSig,
		SessionID: goldenSessionID,
	}, values)

	// The signature must verify over exactly the golden message with the golden
	// public key: an independent Ed25519 verification, not a re-derivation of
	// the signing code.
	publicKey, err := base64.StdEncoding.DecodeString(goldenPubKeyB64)
	require.NoError(t, err)
	signature, err := base64.StdEncoding.DecodeString(values.Sig)
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(publicKey, []byte(goldenRequestSignMessage), signature))
	assert.False(t, ed25519.Verify(publicKey, []byte(goldenDriftMessage), signature),
		"the signature must not verify against a different signed string")
}

func TestComputeSignHeadersVersionInjection(t *testing.T) {
	t.Parallel()

	privateKey := ed25519.NewKeyFromSeed(mustHex(t, goldenSeedHex))
	base := signRequestInput{
		APIKeyID:       goldenAPIKeyID,
		SessionID:      goldenSessionID,
		Ts:             goldenSignTS,
		Nonce:          goldenSignNonce,
		PowBits:        defaultPowBits,
		MaxPowAttempts: powMaxAttempts,
	}

	// Same ts/nonce/session, different X-Client-Version: the PoW challenge does
	// not depend on the version (h0 hashes the fixed "zcode" literal), the
	// signature does. Ticket 28 flips this per settings hot update.
	for _, tc := range []struct {
		version string
		wantSig string
		wantPow string
	}{
		{version: protocolDefaultVersion, wantSig: goldenSignSig, wantPow: goldenPowCandidate8},
		{version: goldenDriftVersion, wantSig: goldenDriftSig, wantPow: goldenPowCandidate8},
	} {
		t.Run("X-Client-Version "+tc.version, func(t *testing.T) {
			t.Parallel()

			in := base
			in.ClientVersion = tc.version
			values, err := computeSignHeaders(privateKey, in)
			require.NoError(t, err)
			assert.Equal(t, tc.version, values.Version)
			assert.Equal(t, tc.wantSig, values.Sig)
			assert.Equal(t, tc.wantPow, values.Pow)
		})
	}
}

func TestComputeSignHeadersPowBits(t *testing.T) {
	t.Parallel()

	privateKey := ed25519.NewKeyFromSeed(mustHex(t, goldenSeedHex))
	base := signRequestInput{
		APIKeyID:       goldenAPIKeyID,
		SessionID:      goldenSessionID,
		Ts:             goldenSignTS,
		ClientVersion:  protocolDefaultVersion,
		Nonce:          goldenSignNonce,
		MaxPowAttempts: powMaxAttempts,
	}

	for _, tc := range []struct {
		name    string
		bits    int
		wantPow string
	}{
		{name: "default 8 bits", bits: defaultPowBits, wantPow: goldenPowCandidate8},
		{name: "non byte aligned 10 bits", bits: 10, wantPow: goldenPowCandidate10},
		{name: "0 bits accepts the first candidate", bits: 0, wantPow: goldenPowCandidate0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := base
			in.PowBits = tc.bits
			values, err := computeSignHeaders(privateKey, in)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPow, values.Pow)
			// The signature covers the nonce, never the PoW candidate: the
			// difficulty can change without touching X-Client-Sig.
			assert.Equal(t, goldenSignSig, values.Sig)
		})
	}
}

func TestComputeSignHeadersFailures(t *testing.T) {
	t.Parallel()

	privateKey := ed25519.NewKeyFromSeed(mustHex(t, goldenSeedHex))
	base := signRequestInput{
		APIKeyID:       goldenAPIKeyID,
		SessionID:      goldenSessionID,
		Ts:             goldenSignTS,
		ClientVersion:  protocolDefaultVersion,
		Nonce:          goldenSignNonce,
		PowBits:        defaultPowBits,
		MaxPowAttempts: powMaxAttempts,
	}

	t.Run("pow bound exhausted", func(t *testing.T) {
		t.Parallel()

		in := base
		in.MaxPowAttempts = 1
		_, err := computeSignHeaders(privateKey, in)
		assert.ErrorIs(t, err, ErrPowUnsolved)
	})

	t.Run("pow bits out of range", func(t *testing.T) {
		t.Parallel()

		in := base
		in.PowBits = -1
		_, err := computeSignHeaders(privateKey, in)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPowUnsolved)
	})

	t.Run("private key of the wrong size is rejected, not panicked on", func(t *testing.T) {
		t.Parallel()

		shortKey := ed25519.PrivateKey([]byte("too short"))
		// Document why the guard exists: signing with a malformed key panics
		// inside crypto/ed25519, which must never reach the gateway hot path.
		assert.Panics(t, func() { ed25519.Sign(shortKey, []byte("x")) })

		_, err := computeSignHeaders(shortKey, base)
		require.Error(t, err)
	})
}

func TestWriteSignHeadersSetsExactlySevenHeaders(t *testing.T) {
	t.Parallel()

	header := http.Header{"X-Custom": []string{"keep-me"}, "X-Client-Ts": []string{"stale"}}
	header.Add("X-Trace-Id", "trace")

	writeSignHeaders(header, signHeaderValues{
		Ts:        goldenSignTS,
		Version:   protocolDefaultVersion,
		Nonce:     goldenSignNonce,
		Pow:       goldenPowCandidate8,
		Sig:       goldenSignSig,
		SessionID: goldenSessionID,
	})

	assert.Equal(t, []string{
		"X-App-Id", "X-Client-Nonce", "X-Client-Pow", "X-Client-Sig", "X-Client-Ts",
		"X-Client-Version", "X-Custom", "X-Session-Id", "X-Trace-Id",
	}, slices.Sorted(maps.Keys(header)), "the seven signature headers plus the caller's two, nothing else")
	assert.Equal(t, []string{"keep-me"}, header.Values("X-Custom"))
	assert.Equal(t, []string{"trace"}, header.Values("X-Trace-Id"))
	assert.Len(t, header.Values(HeaderClientTs), 1, "a stale value is replaced, not appended to")
	assert.Equal(t, goldenSignTS, header.Get(HeaderClientTs))
	assert.Equal(t, protocolDefaultVersion, header.Get(HeaderClientVersion))
	assert.Equal(t, goldenSignSig, header.Get(HeaderClientSig))
	assert.Equal(t, goldenSessionID, header.Get(HeaderSessionID))
	assert.Equal(t, goldenSignNonce, header.Get(HeaderClientNonce))
	assert.Equal(t, protocolAppID, header.Get(HeaderAppID))
	assert.Equal(t, goldenPowCandidate8, header.Get(HeaderClientPow))
	assert.Empty(t, header.Get("X-Client-Sig-Extra"))
}

// powSatisfied reports whether digest carries at least bits leading zero bits.
// It is written straight from the protocol description so the assertions above
// do not depend on the implementation under test.
func powSatisfied(digest []byte, bits int) bool {
	fullBytes := bits / 8
	for i := 0; i < fullBytes && i < len(digest); i++ {
		if digest[i] != 0 {
			return false
		}
	}
	if rem := bits % 8; rem != 0 && fullBytes < len(digest) {
		if subtle.ConstantTimeByteEq(digest[fullBytes]>>(8-rem), 0) != 1 {
			return false
		}
	}
	return true
}

func sha256Sum(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
