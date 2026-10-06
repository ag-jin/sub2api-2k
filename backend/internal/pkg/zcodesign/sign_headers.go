package zcodesign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
)

// Per-request signature header names and protocol literals.
//
// Every literal mirrors the reference implementation
// (zcode-research/sign-v4.mjs:51-56, v4SignHeaders) and the reverse engineering
// report (zcode-150-research FINAL-REPORT.md §五). They live in one block
// because the gateway (ticket 22) asserts exactly these seven headers and the
// ops surfaces (tickets 24/28) key off the same names.
const (
	// HeaderClientTs is the request timestamp in milliseconds.
	HeaderClientTs = "X-Client-Ts"
	// HeaderClientVersion is the official CLI version that is signed.
	HeaderClientVersion = "X-Client-Version"
	// HeaderClientSig is the base64 Ed25519 signature of the request message.
	HeaderClientSig = "X-Client-Sig"
	// HeaderClientNonce is the 16 random bytes of this request, hex encoded
	// (32 characters).
	HeaderClientNonce = "X-Client-Nonce"
	// HeaderClientPow is the proof-of-work candidate: nonce + 8 hex digits.
	HeaderClientPow = "X-Client-Pow"
	// HeaderSessionID is the caller's session id, signed together with the ts.
	HeaderSessionID = "X-Session-Id"
	// HeaderAppID is the fixed client identity header.
	HeaderAppID = "X-App-Id"

	// signAppID is the only value the protocol sends in HeaderAppID.
	signAppID = "zcode"

	// signNonceBytes is the per-request nonce length in raw bytes; the protocol
	// transmits it hex encoded (32 characters).
	signNonceBytes = 16
)

const (
	// defaultPowBits is the PoW difficulty of the official client: the first
	// byte of sha256("<h0>\n<candidate>") must be zero. Ticket 28 exposes it as
	// gateway.zhipu.sign_pow_bits (design M3.1(e)).
	defaultPowBits = 8

	// powMaxAttempts is the upper bound of the PoW search: the candidate counter
	// is exactly 8 hex digits, so the search space is 2^32 and an unsolvable
	// difficulty must fail instead of spinning forever.
	powMaxAttempts = uint64(1) << 32

	// maxPowBits is the largest difficulty that can be expressed by a SHA-256
	// digest; anything above it can never be satisfied.
	maxPowBits = 256
)

// ErrPowUnsolved is returned when the PoW search exhausted its attempt bound
// without finding a candidate. It is a sentinel so the gateway can classify the
// failure (ticket 24) without string matching.
var ErrPowUnsolved = errors.New("zcodesign: proof of work not solved within the attempt bound")

// powChallenge is the per-request PoW prefix h0:
//
//	h0 = hex(sha256("<apiKeyId>\nzcode\n<sessionId>\n<ts>"))[:32]
//
// i.e. the first 16 bytes of the digest, not the whole 32 bytes. The protocol
// hashes the literal "zcode" (the app id), not the client version.
func powChallenge(apiKeyID, sessionID, ts string) string {
	sum := sha256.Sum256([]byte(apiKeyID + "\n" + signAppID + "\n" + sessionID + "\n" + ts))
	return hex.EncodeToString(sum[:16])
}

// powMessage is the string hashed for every PoW candidate.
func powMessage(challenge, candidate string) string {
	return challenge + "\n" + candidate
}

// solvePow searches the first candidate that satisfies the requested number of
// leading zero bits:
//
//	candidate = <nonce><s as 8 hex digits, zero padded>
//	sha256("<h0>\n<candidate>") must have <bits> leading zero bits
//
// The search visits s = 0, 1, 2, ... and stops after maxAttempts hashes,
// returning ErrPowUnsolved. attempts is the number of hashes actually computed
// (the caller uses it for the L1 handshake metrics), and a bits value outside
// [0, 256] is rejected as a configuration error.
func solvePow(nonce, challenge string, bits int, maxAttempts uint64) (string, uint64, error) {
	if bits < 0 || bits > maxPowBits {
		return "", 0, fmt.Errorf("zcodesign: pow bits %d out of range [0,%d]", bits, maxPowBits)
	}
	for s := uint64(0); s < maxAttempts; s++ {
		candidate := fmt.Sprintf("%s%08x", nonce, s)
		if powDigestSatisfies(sha256.Sum256([]byte(powMessage(challenge, candidate))), bits) {
			return candidate, s + 1, nil
		}
	}
	return "", maxAttempts, fmt.Errorf("%w (bits=%d, attempts=%d)", ErrPowUnsolved, bits, maxAttempts)
}

// powDigestSatisfies reports whether digest starts with at least bits zero bits.
// bits = 0 is always satisfied (the configured "off" position), and bits = 256
// requires the whole digest to be zero.
func powDigestSatisfies(digest [sha256.Size]byte, bits int) bool {
	for _, b := range digest[:bits/8] {
		if b != 0 {
			return false
		}
	}
	if rem := bits % 8; rem != 0 {
		if digest[bits/8]>>(8-rem) != 0 {
			return false
		}
	}
	return true
}

// signHeaderValues is one fully computed signature header set. The seven values
// are computed before any of them is written to a request, which is what makes
// "never half write headers" a property of the type rather than of the caller.
type signHeaderValues struct {
	Ts        string
	Version   string
	Nonce     string
	Pow       string
	Sig       string
	SessionID string
}

// signRequestInput is the deterministic input of one signature computation:
// everything the protocol mixes into the PoW challenge, the signed message or
// the header values. Keeping it a value type lets the tests pin Ts and Nonce
// (the two fields the signer draws from the clock and the RNG) while still
// exercising the production code path.
type signRequestInput struct {
	// APIKeyID is the credential id (the part of the api key before the first
	// dot), the cache key of the signing key and a signed field.
	APIKeyID string
	// SessionID is the caller's session id, signed together with Ts.
	SessionID string
	// Ts is the request time in milliseconds.
	Ts string
	// ClientVersion is the signed CLI version (X-Client-Version).
	ClientVersion string
	// Nonce is the per-request 16 random bytes, hex encoded.
	Nonce string
	// PowBits is the required number of leading zero bits of the PoW digest.
	PowBits int
	// MaxPowAttempts bounds the PoW search.
	MaxPowAttempts uint64
}

// signRequestMessage builds the Ed25519 message:
// "<apiKeyId>\n<ts>\n<clientVersion>\n<sessionId>\n<nonce>".
// The version is a signed field: a protocol drift must be an explicit config
// change (ticket 28), never an implicit one.
func signRequestMessage(apiKeyID, ts, clientVersion, sessionID, nonce string) string {
	return apiKeyID + "\n" + ts + "\n" + clientVersion + "\n" + sessionID + "\n" + nonce
}

// computeSignHeaders runs the whole per-request computation: PoW candidate and
// Ed25519 signature over signRequestMessage, in base64. It returns only fully
// computed values; every failure leaves the caller with nothing to write.
func computeSignHeaders(privateKey ed25519.PrivateKey, in signRequestInput) (signHeaderValues, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return signHeaderValues{}, fmt.Errorf(
			"zcodesign: ed25519 private key is %d bytes, want %d", len(privateKey), ed25519.PrivateKeySize)
	}
	solved, _, err := solvePow(in.Nonce, powChallenge(in.APIKeyID, in.SessionID, in.Ts), in.PowBits, in.MaxPowAttempts)
	if err != nil {
		return signHeaderValues{}, err
	}
	message := signRequestMessage(in.APIKeyID, in.Ts, in.ClientVersion, in.SessionID, in.Nonce)
	return signHeaderValues{
		Ts:        in.Ts,
		Version:   in.ClientVersion,
		Nonce:     in.Nonce,
		Pow:       solved,
		Sig:       base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(message))),
		SessionID: in.SessionID,
	}, nil
}

// newSignNonce returns the per-request nonce: 16 random bytes, hex encoded to
// the 32 characters the protocol transmits.
func newSignNonce() (string, error) {
	buf := make([]byte, signNonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("zcodesign: read sign nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// writeSignHeaders copies the computed values into h, leaving every other
// header untouched. Keys are written as the protocol canonical spellings.
func writeSignHeaders(h http.Header, values signHeaderValues) {
	h.Set(HeaderClientTs, values.Ts)
	h.Set(HeaderClientVersion, values.Version)
	h.Set(HeaderClientSig, values.Sig)
	h.Set(HeaderSessionID, values.SessionID)
	h.Set(HeaderClientNonce, values.Nonce)
	h.Set(HeaderAppID, signAppID)
	h.Set(HeaderClientPow, values.Pow)
}
