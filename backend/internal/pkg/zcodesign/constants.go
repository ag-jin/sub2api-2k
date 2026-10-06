package zcodesign

// Signature V4 protocol constants.
//
// Every literal in this file mirrors the reference implementation
// (zcode-research/sign-v4.mjs:13-38, verified live against the official
// endpoint) and the reverse-engineering report (zcode-150-research
// FINAL-REPORT.md §五). Keeping the KDF literals and the handshake endpoint in
// one file makes the protocol drift surface a single place to patch, and gives
// ticket 28 a single place to read the configurable parts from.
const (
	// signKDFSalt is the HKDF salt of every key derived from the API key secret.
	signKDFSalt = "WD_CLIENT_SIGN_KDF_SALT"

	// signKDFInfoHandshake is the HKDF info of the handshake HMAC key.
	signKDFInfoHandshake = "getSignKey_hmac"

	// signKDFInfoEd25519Priv is the HKDF info of the private-cipher AES key.
	signKDFInfoEd25519Priv = "ed25519_priv"

	// signKDFOutputBytes is the HKDF output length: 256 bit for both keys.
	signKDFOutputBytes = 32

	// handshakeSignMessagePrefix is the first line of the handshake signature
	// message; the remaining lines are apiKeyId, ts and nonce.
	handshakeSignMessagePrefix = "get_sign_key"

	// handshakeURLTemplate is the handshake endpoint template. "%s" is the
	// provider origin (for the data plane: https://open.bigmodel.cn). Ticket 28
	// turns this into a configurable value.
	handshakeURLTemplate = "%s/api/paas/c1f3a7e2/v2/client"

	// handshakeNonceBytes is the handshake nonce length in raw bytes; the
	// protocol transmits it hex-encoded (32 characters).
	handshakeNonceBytes = 16

	// handshakeSuccessCode is the only business code that carries a private key
	// cipher; HTTP 200 with any other code is the upstream's business rejection.
	handshakeSuccessCode = 200

	// privateCipherIVBytes is the AES-256-GCM nonce prefix length inside the
	// decrypted privateCipher payload.
	privateCipherIVBytes = 12

	// privateCipherTagBytes is the AES-256-GCM tag length (128 bit) appended to
	// the ciphertext.
	privateCipherTagBytes = 16
)

// DefaultClientVersion is the X-Client-Version value the signer falls back to
// when the configured gateway.zhipu.sign_client_version is empty. Protocol
// drift (a new official CLI release) is an operator-visible config change, so
// the constant exists only as the package-level default.
const DefaultClientVersion = "0.16.9"
