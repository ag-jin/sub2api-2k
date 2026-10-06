//go:build unit

package zcodesign

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

// Protocol literals and golden vectors.
//
// The literals are copied from the reference implementation
// (zcode-research/sign-v4.mjs:13-38) and the golden vectors were produced by
// Node's WebCrypto through that reference implementation with fixed inputs
// (generation command and full output are recorded in ticket 19 Comments).
// They are duplicated here on purpose: the assertions below must not read the
// package constants they verify.
const (
	protocolKDFSalt            = "WD_CLIENT_SIGN_KDF_SALT"
	protocolKDFInfoHandshake   = "getSignKey_hmac"
	protocolKDFInfoEd25519Priv = "ed25519_priv"
	protocolHandshakePath      = "/api/paas/c1f3a7e2/v2/client"
	protocolSignMessagePrefix  = "get_sign_key"
	protocolDefaultVersion     = "0.16.9"

	goldenAPIKey       = "2f8c1d7a4b6e9031.s3cr3t-part.with.dot"
	goldenAPIKeyID     = "2f8c1d7a4b6e9031"
	goldenAPIKeySecret = "s3cr3t-part.with.dot"
	goldenTS           = "1759700000000"
	goldenNonce        = "0123456789abcdef0123456789abcdef"

	goldenHKDFHMACKeyHex = "0d1735cc6acc7f8b890c9b1f29002bc6bc27eef5a521bd8a5ac071e76502c9bd"
	goldenHKDFAESKeyHex  = "dd029b71c7ecf428f30171cc976dbd995776076dae913c3017abb1c73eb1650a"

	goldenSignMessage = "get_sign_key\n2f8c1d7a4b6e9031\n1759700000000\n0123456789abcdef0123456789abcdef"
	goldenSignature   = "Ivaiz/ffMaqA1lv4ouSqTMyCxaR3pz6D/3nTGCogl9E="

	// Ed25519 PKCS#8 key (seed 000102...1f) as Node exported it, encrypted by
	// the reference implementation with IV aabbccddeeff001122334455 and
	// aad = apiKeyId.
	goldenSeedHex       = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	goldenPubKeyB64     = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	goldenIVHex         = "aabbccddeeff001122334455"
	goldenPrivateCipher = "qrvM3e7/ABEiM0RVabK6KKa4bjAz3XcL1y9tCVmo+hcjEe719NJpajTQ/FZebwq0RMxQeg4LAtbxv2wvWV7enF8qhmJOjlbikQrWqZe34CnY2zl7Vg0/WYmKIZo="
)

// referenceSignature recomputes the handshake signature straight from the
// protocol documents (RFC 5869 HKDF-SHA256 + RFC 2104 HMAC) without touching
// package internals, so the httptest upstream can judge a live request
// independently of the code under test.
func referenceSignature(t *testing.T, secret, apiKeyID, ts, nonce string) string {
	t.Helper()

	reader := hkdf.New(sha256.New, []byte(secret), []byte(protocolKDFSalt), []byte(protocolKDFInfoHandshake))
	key := make([]byte, 32)
	_, err := io.ReadFull(reader, key)
	require.NoError(t, err)

	mac := hmac.New(sha256.New, key)
	_, err = mac.Write([]byte(protocolSignMessagePrefix + "\n" + apiKeyID + "\n" + ts + "\n" + nonce))
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestSignConstantsMatchReference(t *testing.T) {
	t.Parallel()

	assert.Equal(t, protocolKDFSalt, signKDFSalt)
	assert.Equal(t, protocolKDFInfoHandshake, signKDFInfoHandshake)
	assert.Equal(t, protocolKDFInfoEd25519Priv, signKDFInfoEd25519Priv)
	assert.Equal(t, "%s"+protocolHandshakePath, handshakeURLTemplate)
	assert.Equal(t, protocolSignMessagePrefix, handshakeSignMessagePrefix)
	assert.Equal(t, protocolDefaultVersion, DefaultClientVersion)
}

func TestHandshakeDerivationGoldenVector(t *testing.T) {
	t.Parallel()

	// Sanity: the independent reference helper agrees with the Node dump; if it
	// did not, the live-request check in the httptest cases would be worthless.
	assert.Equal(t, goldenSignature, referenceSignature(t, goldenAPIKeySecret, goldenAPIKeyID, goldenTS, goldenNonce))

	signKey, err := handshakeSignKey(goldenAPIKeySecret)
	require.NoError(t, err)
	assert.Equal(t, goldenHKDFHMACKeyHex, hex.EncodeToString(signKey))
	assert.Len(t, signKey, 32)

	cipherKeyBytes, err := privateCipherKey(goldenAPIKeySecret)
	require.NoError(t, err)
	assert.Equal(t, goldenHKDFAESKeyHex, hex.EncodeToString(cipherKeyBytes))
	assert.Len(t, cipherKeyBytes, 32)

	// Same derivation through the explicit salt/info seam, so a swapped
	// salt/info pair cannot hide behind the convenience wrappers.
	viaExplicit, err := hkdfSHA256(goldenAPIKeySecret, protocolKDFSalt, protocolKDFInfoHandshake, 32)
	require.NoError(t, err)
	assert.Equal(t, goldenHKDFHMACKeyHex, hex.EncodeToString(viaExplicit))

	assert.Equal(t, goldenSignMessage, handshakeMessage(goldenAPIKeyID, goldenTS, goldenNonce))

	signature, err := handshakeSignature(goldenAPIKeySecret, goldenAPIKeyID, goldenTS, goldenNonce)
	require.NoError(t, err)
	assert.Equal(t, goldenSignature, signature)
}

func TestHandshakeSignatureRejectsOtherMessages(t *testing.T) {
	t.Parallel()

	// A signature over the wrong message must not verify against the golden one:
	// pins the message construction order (prefix, id, ts, nonce).
	assert.NotEqual(t, goldenSignature, referenceSignature(t, goldenAPIKeySecret, goldenAPIKeyID, goldenTS, "fedcba9876543210fedcba9876543210"))

	mac := hmac.New(sha256.New, mustHex(t, goldenHKDFHMACKeyHex))
	_, err := mac.Write([]byte(protocolSignMessagePrefix + "\n" + goldenAPIKeyID + "\n" + goldenNonce + "\n" + goldenTS))
	require.NoError(t, err)
	assert.NotEqual(t, goldenSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	assert.NotEqual(t, goldenSignMessage, handshakeMessage(goldenAPIKeyID, goldenTS, "fedcba9876543210fedcba9876543210"))
}

// referenceCipherKey recomputes the AES-256-GCM key straight from the protocol
// documents, independently of the package derivation under test.
func referenceCipherKey(t *testing.T, secret string) []byte {
	t.Helper()

	reader := hkdf.New(sha256.New, []byte(secret), []byte(protocolKDFSalt), []byte(protocolKDFInfoEd25519Priv))
	key := make([]byte, 32)
	_, err := io.ReadFull(reader, key)
	require.NoError(t, err)
	return key
}

// sealRawPrivateCipher encrypts an arbitrary plaintext (the upstream always
// sends base64 text, but the failure paths need control over the payload):
// base64(iv || AES-256-GCM(plaintext, aad = apiKeyID)).
func sealRawPrivateCipher(t *testing.T, secret, apiKeyID string, iv, plaintext []byte) string {
	t.Helper()

	block, err := aes.NewCipher(referenceCipherKey(t, secret))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	sealed := gcm.Seal(nil, iv, plaintext, []byte(apiKeyID))
	return base64.StdEncoding.EncodeToString(append(append([]byte{}, iv...), sealed...))
}

// sealPrivateCipher builds a privateCipher payload the way the upstream does:
// base64(iv || AES-256-GCM(base64(pkcs8DER), aad = apiKeyId)).
func sealPrivateCipher(t *testing.T, secret, apiKeyID string, iv, pkcs8DER []byte) string {
	t.Helper()

	return sealRawPrivateCipher(t, secret, apiKeyID, iv, []byte(base64.StdEncoding.EncodeToString(pkcs8DER)))
}

// TestHKDFSHA256RFC5869Vectors pins the derivation primitive against the RFC
// 5869 appendix A test vectors (SHA-256 digests), independently of the
// protocol constants: if the primitive were wrong, the golden vectors below
// could still agree with a wrong reference only by coincidence.
func TestHKDFSHA256RFC5869Vectors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		ikm    string
		salt   string
		info   string
		length int
		okm    string
	}{
		{
			name:   "test case 1",
			ikm:    string(mustHex(t, "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")),
			salt:   string(mustHex(t, "000102030405060708090a0b0c")),
			info:   string(mustHex(t, "f0f1f2f3f4f5f6f7f8f9")),
			length: 42,
			okm:    "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865",
		},
		{
			name:   "test case 2",
			ikm:    string(mustHex(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f")),
			salt:   string(mustHex(t, "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf")),
			info:   string(mustHex(t, "b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeeff0f1f2f3f4f5f6f7f8f9fafbfcfdfeff")),
			length: 82,
			okm:    "b11e398dc80327a1c8e7f78c596a49344f012eda2d4efad8a050cc4c19afa97c59045a99cac7827271cb41c65e590e09da3275600c2f09b8367793a9aca3db71cc30c58179ec3e87c14c01d5c1f3434f1d87",
		},
		{
			name:   "test case 3 without salt and info",
			ikm:    string(mustHex(t, "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")),
			salt:   "",
			info:   "",
			length: 42,
			okm:    "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			derived, err := hkdfSHA256(tc.ikm, tc.salt, tc.info, tc.length)
			require.NoError(t, err)
			assert.Len(t, derived, tc.length)
			assert.Equal(t, tc.okm, hex.EncodeToString(derived))
		})
	}
}

func TestDecryptPrivateCipherGoldenVector(t *testing.T) {
	t.Parallel()

	key, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, goldenPrivateCipher)
	require.NoError(t, err)
	assert.Equal(t, goldenSeedHex, hex.EncodeToString(key.Seed()))
	assert.Equal(t, goldenPubKeyB64, base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
}

func TestDecryptPrivateCipherRoundTrip(t *testing.T) {
	t.Parallel()

	// A key the package has never seen, encrypted by the independent reference
	// implementation above: the round trip proves the plaintext handling (the
	// extra base64 layer) and the aad, not just a fixed vector.
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)

	iv := mustHex(t, "00112233445566778899aabb")
	cipherText := sealPrivateCipher(t, goldenAPIKeySecret, goldenAPIKeyID, iv, pkcs8DER)

	decrypted, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, cipherText)
	require.NoError(t, err)
	assert.Equal(t, privateKey.Seed(), decrypted.Seed())
	assert.Equal(t, []byte(publicKey), []byte(decrypted.Public().(ed25519.PublicKey)))
}

func TestDecryptPrivateCipherFailures(t *testing.T) {
	t.Parallel()

	iv := mustHex(t, goldenIVHex)
	otherIV := mustHex(t, "ffeeddccbbaa998877665544")
	validPKCS8 := mustHex(t, "302e020100300506032b657004220420"+goldenSeedHex)

	t.Run("cipher is not base64", func(t *testing.T) {
		t.Parallel()

		_, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, "!!! not base64 !!!")
		requireHandshakeError(t, err, HandshakeErrorKindInvalidCipher)
	})

	t.Run("cipher too short for iv and tag", func(t *testing.T) {
		t.Parallel()

		short := base64.StdEncoding.EncodeToString(make([]byte, privateCipherIVBytes+privateCipherTagBytes-1))
		_, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, short)
		requireHandshakeError(t, err, HandshakeErrorKindInvalidCipher)
	})

	t.Run("wrong iv", func(t *testing.T) {
		t.Parallel()

		cipherText := sealPrivateCipher(t, goldenAPIKeySecret, goldenAPIKeyID, otherIV, validPKCS8)
		// Present the real IV while the payload was sealed with another one.
		raw, err := base64.StdEncoding.DecodeString(cipherText)
		require.NoError(t, err)
		raw = append(append([]byte{}, iv...), raw[privateCipherIVBytes:]...)

		_, err = decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, base64.StdEncoding.EncodeToString(raw))
		requireHandshakeError(t, err, HandshakeErrorKindDecrypt)
	})

	t.Run("wrong aad", func(t *testing.T) {
		t.Parallel()

		cipherText := sealPrivateCipher(t, goldenAPIKeySecret, "some-other-key-id", iv, validPKCS8)
		_, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, cipherText)
		requireHandshakeError(t, err, HandshakeErrorKindDecrypt)
	})

	t.Run("wrong secret", func(t *testing.T) {
		t.Parallel()

		cipherText := sealPrivateCipher(t, "another-secret", goldenAPIKeyID, iv, validPKCS8)
		_, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, cipherText)
		requireHandshakeError(t, err, HandshakeErrorKindDecrypt)
	})

	t.Run("tampered tag", func(t *testing.T) {
		t.Parallel()

		raw, err := base64.StdEncoding.DecodeString(goldenPrivateCipher)
		require.NoError(t, err)
		raw[len(raw)-1] ^= 0x01

		_, err = decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, base64.StdEncoding.EncodeToString(raw))
		requireHandshakeError(t, err, HandshakeErrorKindDecrypt)
	})

	t.Run("plaintext is not base64", func(t *testing.T) {
		t.Parallel()

		cipherText := sealRawPrivateCipher(t, goldenAPIKeySecret, goldenAPIKeyID, iv, []byte("not base64 ###"))
		_, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, cipherText)
		requireHandshakeError(t, err, HandshakeErrorKindInvalidKey)
	})

	t.Run("plaintext base64 is not pkcs8", func(t *testing.T) {
		t.Parallel()

		cipherText := sealRawPrivateCipher(t, goldenAPIKeySecret, goldenAPIKeyID, iv, []byte(base64.StdEncoding.EncodeToString([]byte("still not der"))))
		_, err := decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, cipherText)
		requireHandshakeError(t, err, HandshakeErrorKindInvalidKey)
	})

	t.Run("pkcs8 is not an ed25519 key", func(t *testing.T) {
		t.Parallel()

		ecdsaDER, err := x509.MarshalPKCS8PrivateKey(mustECDSAKey(t))
		require.NoError(t, err)

		cipherText := sealPrivateCipher(t, goldenAPIKeySecret, goldenAPIKeyID, iv, ecdsaDER)
		_, err = decryptPrivateCipher(goldenAPIKeySecret, goldenAPIKeyID, cipherText)
		requireHandshakeError(t, err, HandshakeErrorKindInvalidKey)
	})
}

func mustECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

// capturedHandshake is one recorded upstream handshake request.
type capturedHandshake struct {
	method        string
	path          string
	rawQuery      string
	authorization string
	contentType   string
	body          []byte
}

// newHandshakeUpstream starts a fake handshake endpoint. The response is built
// from the recorded request, so each case can decide the reply.
func newHandshakeUpstream(t *testing.T, respond func(capturedHandshake) (int, string)) (*httptest.Server, chan capturedHandshake) {
	t.Helper()

	captured := make(chan capturedHandshake, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record := capturedHandshake{
			method:        r.Method,
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
		}
		captured <- record

		status, payload := respond(record)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

func TestHandshakeSuccess(t *testing.T) {
	t.Parallel()

	server, captured := newHandshakeUpstream(t, func(capturedHandshake) (int, string) {
		return http.StatusOK, `{"code":200,"msg":"success","data":{"privateCipher":"` + goldenPrivateCipher + `"}}`
	})

	result, err := handshake(context.Background(), goldenAPIKey, server.URL, server.Client())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, goldenAPIKeyID, result.APIKeyID)
	assert.Equal(t, goldenSeedHex, hex.EncodeToString(result.PrivateKey.Seed()))
	assert.Equal(t, goldenPubKeyB64, base64.StdEncoding.EncodeToString(result.PrivateKey.Public().(ed25519.PublicKey)))

	record := <-captured
	assert.Equal(t, http.MethodPost, record.method)
	assert.Equal(t, protocolHandshakePath, record.path)
	assert.Empty(t, record.rawQuery)
	assert.Equal(t, goldenAPIKey, record.authorization)
	assert.Contains(t, record.contentType, "application/json")

	var body map[string]any
	require.NoError(t, json.Unmarshal(record.body, &body))
	assert.Len(t, body, 4)
	assert.Equal(t, goldenAPIKey, body["apiKey"])
	assert.Equal(t, referenceSignature(t, goldenAPIKeySecret, goldenAPIKeyID, body["ts"].(string), body["nonce"].(string)), body["sig"])

	// The nonce is 16 random bytes hex encoded, the ts is milliseconds.
	nonce, ok := body["nonce"].(string)
	require.True(t, ok)
	assert.Regexp(t, `^[0-9a-f]{32}$`, nonce)
	ts, ok := body["ts"].(string)
	require.True(t, ok)
	assert.Regexp(t, `^[0-9]{13}$`, ts)
	millis, err := strconv.ParseInt(ts, 10, 64)
	require.NoError(t, err)
	assert.InDelta(t, time.Now().UnixMilli(), millis, float64(time.Minute.Milliseconds()))
}

func TestHandshakeExportedEntryPoint(t *testing.T) {
	t.Parallel()

	server, _ := newHandshakeUpstream(t, func(capturedHandshake) (int, string) {
		return http.StatusOK, `{"code":200,"data":{"privateCipher":"` + goldenPrivateCipher + `"}}`
	})

	result, err := Handshake(context.Background(), goldenAPIKey, server.URL, server.Client())
	require.NoError(t, err)
	assert.Equal(t, goldenAPIKeyID, result.APIKeyID)
	assert.Equal(t, goldenSeedHex, hex.EncodeToString(result.PrivateKey.Seed()))

	_, err = Handshake(context.Background(), "no-dot-at-all", server.URL, server.Client())
	requireHandshakeError(t, err, HandshakeErrorKindInvalidAPIKey)
}

func TestHandshakeUsesAUniqueNoncePerCall(t *testing.T) {
	t.Parallel()

	server, captured := newHandshakeUpstream(t, func(capturedHandshake) (int, string) {
		return http.StatusOK, `{"code":200,"data":{"privateCipher":"` + goldenPrivateCipher + `"}}`
	})

	for i := 0; i < 2; i++ {
		_, err := handshake(context.Background(), goldenAPIKey, server.URL, server.Client())
		require.NoError(t, err)
	}

	first := <-captured
	second := <-captured
	var firstBody, secondBody map[string]any
	require.NoError(t, json.Unmarshal(first.body, &firstBody))
	require.NoError(t, json.Unmarshal(second.body, &secondBody))
	assert.NotEqual(t, firstBody["nonce"], secondBody["nonce"])
}

func TestHandshakeFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		apiKey     string
		status     int
		body       func(t *testing.T) string
		wantKind   HandshakeErrorKind
		wantStatus int
		wantCode   int
	}{
		{
			name:       "http 500 with a business body",
			apiKey:     goldenAPIKey,
			status:     http.StatusInternalServerError,
			body:       func(*testing.T) string { return `{"code":500,"msg":"internal error"}` },
			wantKind:   HandshakeErrorKindHTTPStatus,
			wantStatus: http.StatusInternalServerError,
			wantCode:   500,
		},
		{
			name:       "http 502 error page",
			apiKey:     goldenAPIKey,
			status:     http.StatusBadGateway,
			body:       func(*testing.T) string { return `<html><body>502 Bad Gateway</body></html>` },
			wantKind:   HandshakeErrorKindHTTPStatus,
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "http 200 with a business rejection",
			apiKey:     goldenAPIKey,
			status:     http.StatusOK,
			body:       func(*testing.T) string { return `{"code":401,"msg":"invalid api key"}` },
			wantKind:   HandshakeErrorKindBusinessCode,
			wantStatus: http.StatusOK,
			wantCode:   401,
		},
		{
			name:       "http 200 without a code",
			apiKey:     goldenAPIKey,
			status:     http.StatusOK,
			body:       func(*testing.T) string { return `{"msg":"no code here"}` },
			wantKind:   HandshakeErrorKindBusinessCode,
			wantStatus: http.StatusOK,
		},
		{
			name:       "http 200 with code 200 but no data",
			apiKey:     goldenAPIKey,
			status:     http.StatusOK,
			body:       func(*testing.T) string { return `{"code":200,"msg":"success"}` },
			wantKind:   HandshakeErrorKindMissingCipher,
			wantStatus: http.StatusOK,
			wantCode:   200,
		},
		{
			name:       "http 200 with an empty privateCipher",
			apiKey:     goldenAPIKey,
			status:     http.StatusOK,
			body:       func(*testing.T) string { return `{"code":200,"data":{"privateCipher":"   "}}` },
			wantKind:   HandshakeErrorKindMissingCipher,
			wantStatus: http.StatusOK,
			wantCode:   200,
		},
		{
			name:       "http 200 with a non json body",
			apiKey:     goldenAPIKey,
			status:     http.StatusOK,
			body:       func(*testing.T) string { return `<!doctype html><html>maintenance</html>` },
			wantKind:   HandshakeErrorKindMalformedBody,
			wantStatus: http.StatusOK,
		},
		{
			name:   "http 200 with a string business code",
			apiKey: goldenAPIKey,
			status: http.StatusOK,
			body: func(*testing.T) string {
				return `{"code":"200","data":{"privateCipher":"` + goldenPrivateCipher + `"}}`
			},
			wantKind:   HandshakeErrorKindMalformedBody,
			wantStatus: http.StatusOK,
		},
		{
			name:     "http 200 with a privateCipher that is not base64",
			apiKey:   goldenAPIKey,
			status:   http.StatusOK,
			body:     func(*testing.T) string { return `{"code":200,"data":{"privateCipher":"!!! not base64 !!!"}}` },
			wantKind: HandshakeErrorKindInvalidCipher,
			// Cipher-level failures describe the payload, not the envelope, so
			// they carry no status/code metadata.
		},
		{
			name:   "http 200 with a privateCipher that decrypts to junk",
			apiKey: goldenAPIKey,
			status: http.StatusOK,
			body: func(t *testing.T) string {
				return envelopeJSON(t, sealRawPrivateCipher(t, goldenAPIKeySecret, goldenAPIKeyID, mustHex(t, goldenIVHex), []byte("junk")))
			},
			wantKind: HandshakeErrorKindInvalidKey,
		},
		{
			name:     "http 200 with a privateCipher for another secret",
			apiKey:   goldenAPIKeyID + ".another-secret",
			status:   http.StatusOK,
			body:     func(*testing.T) string { return envelopeJSON(t, goldenPrivateCipher) },
			wantKind: HandshakeErrorKindDecrypt,
		},
		{
			name:       "http 200 with a valid cipher but a malformed api key",
			apiKey:     "no-dot-at-all",
			status:     http.StatusOK,
			body:       func(*testing.T) string { return envelopeJSON(t, goldenPrivateCipher) },
			wantKind:   HandshakeErrorKindInvalidAPIKey,
			wantStatus: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build the reply on the test goroutine: the body builders use
			// testify helpers, which must not run on the server's goroutine.
			payload := tc.body(t)
			server, captured := newHandshakeUpstream(t, func(capturedHandshake) (int, string) {
				return tc.status, payload
			})

			result, err := handshake(context.Background(), tc.apiKey, server.URL, server.Client())
			assert.Nil(t, result)

			classified := requireHandshakeError(t, err, tc.wantKind)
			assert.Equal(t, tc.wantStatus, classified.StatusCode)
			assert.Equal(t, tc.wantCode, classified.Code)

			if tc.wantKind == HandshakeErrorKindInvalidAPIKey {
				select {
				case record := <-captured:
					t.Fatalf("malformed api key must not reach upstream, got %+v", record)
				default:
				}
			}
		})
	}
}

func TestHandshakeTransportFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection refused")
	_, err := handshake(context.Background(), goldenAPIKey, "https://open.bigmodel.cn", failingDoer{sentinel})
	classified := requireHandshakeError(t, err, HandshakeErrorKindRequest)
	assert.ErrorIs(t, classified, sentinel)
}

func TestHandshakeTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	// Cleanups run last-in-first-out: unblock the handler before closing the
	// server, so a stalled handler cannot hold the test shutdown hostage.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	client := &http.Client{Timeout: 50 * time.Millisecond}

	start := time.Now()
	_, err := handshake(ctx, goldenAPIKey, server.URL, client)
	assert.Less(t, time.Since(start), 2*time.Second)
	classified := requireHandshakeError(t, err, HandshakeErrorKindRequest)
	assert.ErrorIs(t, classified, context.DeadlineExceeded)
}

func TestHandshakeRejectsNilDoer(t *testing.T) {
	t.Parallel()

	_, err := handshake(context.Background(), goldenAPIKey, "https://open.bigmodel.cn", nil)
	requireHandshakeError(t, err, HandshakeErrorKindRequest)
}

// failingDoer is a transport that always fails.
type failingDoer struct{ err error }

func (d failingDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

// envelopeJSON wraps a privateCipher in the upstream reply envelope.
func envelopeJSON(t *testing.T, privateCipher string) string {
	t.Helper()

	payload, err := json.Marshal(map[string]any{"code": 200, "data": map[string]any{"privateCipher": privateCipher}})
	require.NoError(t, err)
	return string(payload)
}

func TestSplitAPIKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		apiKey     string
		wantID     string
		wantSecret string
	}{
		{
			name:       "splits on the first dot and keeps later dots in the secret",
			apiKey:     goldenAPIKey,
			wantID:     goldenAPIKeyID,
			wantSecret: goldenAPIKeySecret,
		},
		{
			name:       "single dot",
			apiKey:     "abcd.secret",
			wantID:     "abcd",
			wantSecret: "secret",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, secret, err := splitAPIKey(tc.apiKey)
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.wantSecret, secret)
		})
	}
}

func TestSplitAPIKeyRejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, apiKey := range []string{"", "no-dot-at-all", "2f8c1d7a4b6e9031.", ".s3cr3t"} {
		t.Run("apiKey "+apiKey, func(t *testing.T) {
			t.Parallel()

			_, _, err := splitAPIKey(apiKey)
			requireHandshakeError(t, err, HandshakeErrorKindInvalidAPIKey)
		})
	}
}

func requireHandshakeError(t *testing.T, err error, kind HandshakeErrorKind) *HandshakeError {
	t.Helper()

	require.Error(t, err)
	var target *HandshakeError
	require.ErrorAs(t, err, &target)
	assert.Equal(t, kind, target.Kind)
	return target
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()

	decoded, err := hex.DecodeString(value)
	require.NoError(t, err)
	return decoded
}
