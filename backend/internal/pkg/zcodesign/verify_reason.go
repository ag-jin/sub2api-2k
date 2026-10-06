package zcodesign

import "encoding/json"

// VERIFY_* reasons carried by upstream error bodies when the client signature
// is rejected. Values are protocol literals (official CLI
// readRefreshableClientSignatureReason) and are exported for the gateway
// self-heal path (tickets 23/24/28).
const (
	VerifySignatureInvalid = "VERIFY_SIGNATURE_INVALID"
	VerifyAPIKeyExpired    = "VERIFY_APIKEY_EXPIRED"
)

// readZhipuSignatureVerifyReason scans an upstream error body once and returns
// the VERIFY_* reason it carries, or "" when the body carries none.
//
// It mirrors the official CLI readRefreshableClientSignatureReason field order:
// msg, reason, data.reason, error.reason, error.message — data/error must be
// JSON objects, and a value only matches on exact, case-sensitive equality with
// one of the two protocol constants.
func readZhipuSignatureVerifyReason(body []byte) string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	data, _ := root["data"].(map[string]any)
	errObj, _ := root["error"].(map[string]any)
	for _, candidate := range []any{
		root["msg"],
		root["reason"],
		data["reason"],
		errObj["reason"],
		errObj["message"],
	} {
		value, ok := candidate.(string)
		if !ok {
			continue
		}
		switch value {
		case VerifySignatureInvalid:
			return VerifySignatureInvalid
		case VerifyAPIKeyExpired:
			return VerifyAPIKeyExpired
		}
	}
	return ""
}

// ReadZhipuSignatureVerifyReason exposes the parser to callers outside this
// package (gateway error path, tickets 23/24).
func ReadZhipuSignatureVerifyReason(body []byte) string {
	return readZhipuSignatureVerifyReason(body)
}
