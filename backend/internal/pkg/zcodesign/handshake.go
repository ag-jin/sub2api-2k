// Package zcodesign implements the ZCode client signature V4 protocol (the
// "coding plan discount" handshake plus the per-request X-Client-* signature
// headers) as a 1:1 port of zcode-research/sign-v4.mjs.
package zcodesign

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// HandshakeErrorKind classifies why a handshake failed. The classification is
// what the gateway self-heal path (ticket 23) and the L1 metrics (ticket 24)
// key off, so it is part of the package contract.
type HandshakeErrorKind string

const (
	// HandshakeErrorKindInvalidAPIKey: the credential is not "<id>.<secret>".
	HandshakeErrorKindInvalidAPIKey HandshakeErrorKind = "invalid_api_key"
	// HandshakeErrorKindDeriveKey: the HKDF key derivation failed.
	HandshakeErrorKindDeriveKey HandshakeErrorKind = "derive_key"
	// HandshakeErrorKindInvalidCipher: data.privateCipher is not usable
	// base64/too short to carry an IV and a GCM tag.
	HandshakeErrorKindInvalidCipher HandshakeErrorKind = "invalid_private_cipher"
	// HandshakeErrorKindDecrypt: AES-256-GCM authentication failed (wrong key,
	// IV or aad, or a tampered payload).
	HandshakeErrorKindDecrypt HandshakeErrorKind = "decrypt"
	// HandshakeErrorKindInvalidKey: the decrypted payload is not a PKCS#8
	// Ed25519 private key.
	HandshakeErrorKindInvalidKey HandshakeErrorKind = "invalid_private_key"
	// HandshakeErrorKindRequest: the handshake request could not be built, sent
	// or read (transport error, cancelled context, timeout).
	HandshakeErrorKindRequest HandshakeErrorKind = "request"
	// HandshakeErrorKindHTTPStatus: the upstream answered with a non-2xx status.
	HandshakeErrorKindHTTPStatus HandshakeErrorKind = "http_status"
	// HandshakeErrorKindMalformedBody: the upstream body is not the expected
	// JSON envelope.
	HandshakeErrorKindMalformedBody HandshakeErrorKind = "malformed_body"
	// HandshakeErrorKindBusinessCode: the envelope carried a code other than
	// 200 (HTTP 200 with code != 200 is the upstream's business rejection).
	HandshakeErrorKindBusinessCode HandshakeErrorKind = "business_code"
	// HandshakeErrorKindMissingCipher: the envelope carried no privateCipher.
	HandshakeErrorKindMissingCipher HandshakeErrorKind = "missing_private_cipher"
)

// HTTPDoer is the minimal HTTP transport seam the handshake needs. It is the
// same interface the signer (ticket 20) takes, so the gateway injects a single
// transport for both.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// HandshakeResult is one successful handshake: the credential identity plus the
// Ed25519 private key the upstream handed back. The caller owns caching; the
// handshake itself keeps no state, so a half-finished handshake can never be
// served from this package.
type HandshakeResult struct {
	APIKeyID   string
	PrivateKey ed25519.PrivateKey
}

// Handshake performs one signature V4 handshake and returns the Ed25519 private
// key that belongs to apiKey. It is the package's external entry point; the
// signer (ticket 20) calls handshake below directly, in-package, so it can fold
// the result into its own cache.
func Handshake(ctx context.Context, apiKey, origin string, doer HTTPDoer) (*HandshakeResult, error) {
	return handshake(ctx, apiKey, origin, doer)
}

// handshakeRequestBody is the upstream request body. The field order mirrors
// the reference implementation's JSON.stringify({apiKey, nonce, sig, ts}).
type handshakeRequestBody struct {
	APIKey string `json:"apiKey"`
	Nonce  string `json:"nonce"`
	Sig    string `json:"sig"`
	Ts     string `json:"ts"`
}

// handshakeResponse is the upstream reply envelope.
type handshakeResponse struct {
	Code float64 `json:"code"`
	Data *struct {
		PrivateCipher string `json:"privateCipher"`
	} `json:"data"`
}

// handshake performs the signature V4 handshake for one credential:
//
//	POST <origin>/api/paas/c1f3a7e2/v2/client
//	Authorization: <apiKey>   Content-Type: application/json
//	{apiKey, nonce, sig, ts}  sig = base64(HMAC-SHA256(hkdf(secret), msg))
//
// and returns the Ed25519 private key decrypted from data.privateCipher. Only a
// code == 200 envelope carrying a non-empty privateCipher continues; every other
// outcome is a classified *HandshakeError.
func handshake(ctx context.Context, apiKey, origin string, doer HTTPDoer) (*HandshakeResult, error) {
	apiKeyID, apiKeySecret, err := splitAPIKey(apiKey)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce, err := newHandshakeNonce()
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindRequest, Err: err}
	}
	signature, err := handshakeSignature(apiKeySecret, apiKeyID, ts, nonce)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindDeriveKey, Err: err}
	}
	body, err := json.Marshal(handshakeRequestBody{APIKey: apiKey, Nonce: nonce, Sig: signature, Ts: ts})
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindRequest, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(handshakeURLTemplate, origin), bytes.NewReader(body))
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindRequest, Err: err}
	}
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("Content-Type", "application/json")
	if doer == nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindRequest, Err: errors.New("nil http doer")}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindRequest, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindRequest, StatusCode: resp.StatusCode, Err: err}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &HandshakeError{
			Kind:       HandshakeErrorKindHTTPStatus,
			StatusCode: resp.StatusCode,
			Code:       envelopeCode(payload),
		}
	}
	var envelope handshakeResponse
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindMalformedBody, StatusCode: resp.StatusCode, Err: err}
	}
	if envelope.Code != handshakeSuccessCode {
		return nil, &HandshakeError{
			Kind:       HandshakeErrorKindBusinessCode,
			StatusCode: resp.StatusCode,
			Code:       int(envelope.Code),
		}
	}
	if envelope.Data == nil || strings.TrimSpace(envelope.Data.PrivateCipher) == "" {
		return nil, &HandshakeError{
			Kind:       HandshakeErrorKindMissingCipher,
			StatusCode: resp.StatusCode,
			Code:       int(envelope.Code),
		}
	}
	privateKey, err := decryptPrivateCipher(apiKeySecret, apiKeyID, envelope.Data.PrivateCipher)
	if err != nil {
		return nil, err
	}
	return &HandshakeResult{APIKeyID: apiKeyID, PrivateKey: privateKey}, nil
}

// newHandshakeNonce returns the 16 random bytes the protocol sends hex encoded.
func newHandshakeNonce() (string, error) {
	buf := make([]byte, handshakeNonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("zcodesign handshake: read nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// envelopeCode reads the business code of a body without keeping the body: it is
// used only to enrich transport-level errors with the upstream code.
func envelopeCode(body []byte) int {
	var envelope handshakeResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	return int(envelope.Code)
}

// HandshakeError is a classified handshake failure. It deliberately carries no
// upstream body, URL or key material: it is safe to log and surface to
// operators.
type HandshakeError struct {
	// Kind is the failure class.
	Kind HandshakeErrorKind
	// StatusCode is the upstream HTTP status, when a response was received.
	StatusCode int
	// Code is the upstream business code, when the body carried one.
	Code int
	// Err is the underlying cause, when there is one.
	Err error
}

func (e *HandshakeError) Error() string {
	message := "zcodesign handshake: " + string(e.Kind)
	if e.StatusCode != 0 {
		message += fmt.Sprintf(" (http %d)", e.StatusCode)
	}
	if e.Code != 0 {
		message += fmt.Sprintf(" (upstream code %d)", e.Code)
	}
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

// Unwrap exposes the underlying cause for errors.Is/errors.As callers.
func (e *HandshakeError) Unwrap() error { return e.Err }

// splitAPIKey splits "<apiKeyId>.<apiKeySecret>" at the first dot. The secret
// itself may contain dots (the reference implementation uses indexOf), and both
// halves must be present: a credential without a usable pair is rejected before
// any upstream call.
func splitAPIKey(apiKey string) (string, string, error) {
	apiKeyID, apiKeySecret, found := strings.Cut(apiKey, ".")
	if !found || apiKeyID == "" || apiKeySecret == "" {
		return "", "", &HandshakeError{Kind: HandshakeErrorKindInvalidAPIKey}
	}
	return apiKeyID, apiKeySecret, nil
}

// hkdfSHA256 derives length bytes with HKDF-SHA256 (RFC 5869). The explicit
// salt/info arguments keep the RFC test vectors and the protocol constants
// reachable through the same code path.
func hkdfSHA256(secret, salt, info string, length int) ([]byte, error) {
	reader := hkdf.New(sha256.New, []byte(secret), []byte(salt), []byte(info))
	out := make([]byte, length)
	if _, err := io.ReadFull(reader, out); err != nil {
		return nil, fmt.Errorf("zcodesign hkdf: derive %d bytes: %w", length, err)
	}
	return out, nil
}

// handshakeSignKey derives the HMAC key used to sign the handshake request.
func handshakeSignKey(apiKeySecret string) ([]byte, error) {
	return hkdfSHA256(apiKeySecret, signKDFSalt, signKDFInfoHandshake, signKDFOutputBytes)
}

// privateCipherKey derives the AES-256-GCM key that protects the private key
// cipher returned by the handshake.
func privateCipherKey(apiKeySecret string) ([]byte, error) {
	return hkdfSHA256(apiKeySecret, signKDFSalt, signKDFInfoEd25519Priv, signKDFOutputBytes)
}

// decryptPrivateCipher turns the handshake's data.privateCipher into the
// Ed25519 private key it protects: base64 -> AES-256-GCM (iv = first 12 bytes,
// aad = apiKeyId, 128 bit tag) -> UTF-8 text -> base64 -> PKCS#8.
//
// Every failure is classified and returned; no partial key material escapes.
func decryptPrivateCipher(apiKeySecret, apiKeyID, privateCipher string) (ed25519.PrivateKey, error) {
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateCipher))
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindInvalidCipher, Err: err}
	}
	if len(payload) < privateCipherIVBytes+privateCipherTagBytes {
		return nil, &HandshakeError{
			Kind: HandshakeErrorKindInvalidCipher,
			Err:  fmt.Errorf("cipher shorter than iv+tag: %d bytes", len(payload)),
		}
	}
	key, err := privateCipherKey(apiKeySecret)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindDeriveKey, Err: err}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindDeriveKey, Err: err}
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindDeriveKey, Err: err}
	}
	plain, err := gcm.Open(nil, payload[:privateCipherIVBytes], payload[privateCipherIVBytes:], []byte(apiKeyID))
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindDecrypt, Err: err}
	}
	inner, err := base64.StdEncoding.DecodeString(string(plain))
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindInvalidKey, Err: err}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(inner)
	if err != nil {
		return nil, &HandshakeError{Kind: HandshakeErrorKindInvalidKey, Err: err}
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, &HandshakeError{
			Kind: HandshakeErrorKindInvalidKey,
			Err:  fmt.Errorf("pkcs8 key is %T, not ed25519.PrivateKey", parsed),
		}
	}
	return privateKey, nil
}

// handshakeMessage builds the string signed by the handshake HMAC:
// "get_sign_key\n<apiKeyId>\n<ts>\n<nonce>".
func handshakeMessage(apiKeyID, ts, nonce string) string {
	return handshakeSignMessagePrefix + "\n" + apiKeyID + "\n" + ts + "\n" + nonce
}

// handshakeSignature signs the handshake message with the HKDF-derived HMAC key
// and returns standard base64.
func handshakeSignature(apiKeySecret, apiKeyID, ts, nonce string) (string, error) {
	key, err := handshakeSignKey(apiKeySecret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	if _, err := mac.Write([]byte(handshakeMessage(apiKeyID, ts, nonce))); err != nil {
		return "", fmt.Errorf("zcodesign handshake: sign request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}
