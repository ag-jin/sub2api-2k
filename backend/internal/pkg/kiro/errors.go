package kiro

import (
	"encoding/json"
	"regexp"
	"strings"
)

// IsMonthlyRequestLimit reports whether an upstream error body indicates the
// monthly request quota is exhausted. Mirrors kiro.rs
// endpoint/mod.rs::default_is_monthly_request_limit (top-level `reason` and
// nested `error.reason`).
func IsMonthlyRequestLimit(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// Fast substring check first (matches kiro.rs).
	if containsBytes(body, "MONTHLY_REQUEST_COUNT") {
		return true
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	if r, ok := v["reason"]; ok {
		var s string
		if json.Unmarshal(r, &s) == nil && s == "MONTHLY_REQUEST_COUNT" {
			return true
		}
	}
	if e, ok := v["error"]; ok {
		var em map[string]json.RawMessage
		if json.Unmarshal(e, &em) == nil {
			if r, ok := em["reason"]; ok {
				var s string
				if json.Unmarshal(r, &s) == nil && s == "MONTHLY_REQUEST_COUNT" {
					return true
				}
			}
		}
	}
	return false
}

// IsBearerTokenInvalid reports whether an upstream error body indicates the
// bearer (access) token is invalid, meaning a token refresh + retry on the
// SAME account is warranted. Mirrors kiro.rs default_is_bearer_token_invalid.
func IsBearerTokenInvalid(body []byte) bool {
	return containsBytes(body, "The bearer token included in the request is invalid")
}

// IsRateLimitError reports whether an upstream response indicates rate limiting
// or transient quota throttling — the account should be cooled down (marked
// rate-limited) and the request failed over to another account. Mirrors kiro.rs
// map_upstream_error mapping of ThrottlingException / ServiceQuotaExceeded /
// 429 to rate_limit_error.
//
// NOTE: This is distinct from IsMonthlyRequestLimit (a hard monthly cap that
// warrants a much longer cooldown) — callers should check the monthly limit
// first and treat it separately.
func IsRateLimitError(status int, body []byte) bool {
	if status == 429 {
		return true
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "throttlingexception") ||
		strings.Contains(lower, "servicequotaexceededexception")
}

// IsAccountSuspended reports whether an upstream error indicates the account is
// suspended/banned (e.g. AWS AccessDeniedException with a "suspended" reason).
// Mirrors kiro.rs map_upstream_error is_banned detection. Case-insensitive so
// it matches both "TemporarilySuspended" and "suspended". The account should be
// taken out of rotation (temp-unschedulable) and the request failed over.
func IsAccountSuspended(status int, body []byte) bool {
	lower := strings.ToLower(string(body))
	// kam: 403 + AccessDeniedException + TemporarilySuspended, OR any "suspended".
	if strings.Contains(lower, "accessdeniedexception") && strings.Contains(lower, "suspend") {
		return true
	}
	return strings.Contains(lower, "suspended")
}

func containsBytes(b []byte, sub string) bool {
	s := string(b)
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// sanitizePatterns are compiled once and reused. They redact secret-bearing
// substrings from upstream error bodies before they hit logs or the client.
// Mirrors kiro.rs sanitize_error, plus a JWT pattern (Kiro/Cognito tokens are
// JWTs that may surface in 403 bodies).
var sanitizePatterns = []*regexp.Regexp{
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`"accessToken"\s*:\s*"[^"]+"`),
	regexp.MustCompile(`"refreshToken"\s*:\s*"[^"]+"`),
	regexp.MustCompile(`"clientSecret"\s*:\s*"[^"]+"`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),
}

// SanitizeError redacts secrets (bearer tokens, sk- keys, JWTs, embedded
// access/refresh tokens and client secrets) from an upstream error string so it
// is safe to log or surface to clients. Mirrors kiro.rs sanitize_error.
func SanitizeError(message string) string {
	out := message
	for _, re := range sanitizePatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// SanitizeErrorBytes is the []byte convenience wrapper around SanitizeError.
func SanitizeErrorBytes(body []byte) string {
	return SanitizeError(string(body))
}

// IsContextLengthError reports whether an upstream error body indicates the
// request exceeded context/input limits - a client error that must NOT be
// retried or failed over (every account would reject it the same way).
// Mirrors kiro.rs map_provider_error handling of CONTENT_LENGTH_EXCEEDS_THRESHOLD
// and "Input is too long".
func IsContextLengthError(body []byte) (isErr bool, friendly string) {
	s := string(body)
	if containsBytes(body, "CONTENT_LENGTH_EXCEEDS_THRESHOLD") {
		return true, "Context window is full. Reduce conversation history, system prompt, or tools."
	}
	if containsBytes(body, "Input is too long") {
		return true, "Input is too long. Reduce the size of your messages."
	}
	_ = s
	return false, ""
}
