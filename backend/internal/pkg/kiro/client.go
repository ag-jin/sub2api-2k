package kiro

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// newUUID returns a random RFC-4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildHTTPClient creates an *http.Client honoring an optional proxy URL.
// Supports http://, https://, socks5:// (with optional user:pass@).
func buildHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		// Real Kiro IDE (aws-sdk-js on Node) talks HTTP/1.1 to the backend and
		// sends "Connection: close". Force HTTP/1.1 to match that fingerprint.
		// Note: ForceAttemptHTTP2=false alone does NOT disable h2 on the default
		// (non-proxied) path — Go still auto-upgrades. Setting an empty, non-nil
		// TLSNextProto is the documented way to actually turn h2 off.
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		switch u.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			var auth *xproxy.Auth
			if u.User != nil {
				pw, _ := u.User.Password()
				auth = &xproxy.Auth{User: u.User.Username(), Password: pw}
			}
			dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("build socks5 dialer: %w", err)
			}
			ctxDialer, ok := dialer.(xproxy.ContextDialer)
			if ok {
				transport.DialContext = ctxDialer.DialContext
			} else {
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				}
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

// Endpoint describes the Kiro IDE backend endpoint.
type Endpoint struct {
	cfg KiroConfig
}

// NewEndpoint creates an Endpoint with the given config.
func NewEndpoint(cfg KiroConfig) *Endpoint {
	return &Endpoint{cfg: cfg}
}

// APIURL returns the generateAssistantResponse URL for the credential region.
//
// New Kiro 0.12.x migrated the conversation backend off AWS CodeWhisperer
// (q.{region}.amazonaws.com) onto runtime.{region}.kiro.dev. Region is still
// derived per-account, so other regions adapt automatically once observed.
func (e *Endpoint) APIURL(cred *KiroCredential) string {
	return fmt.Sprintf("https://runtime.%s.kiro.dev/generateAssistantResponse", cred.EffectiveAPIRegion())
}

func (e *Endpoint) host(cred *KiroCredential) string {
	return fmt.Sprintf("runtime.%s.kiro.dev", cred.EffectiveAPIRegion())
}

func (e *Endpoint) xAmzUserAgent(cred *KiroCredential) string {
	mid := cred.EffectiveMachineID()
	return fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", AwsSdkVersionAPI, e.cfg.KiroVersion, mid)
}

func (e *Endpoint) userAgent(cred *KiroCredential) string {
	mid := cred.EffectiveMachineID()
	return fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererstreaming#%s m/N KiroIDE-%s-%s",
		AwsSdkVersionAPI, cred.EffectiveSystemVersion(e.cfg), e.cfg.NodeVersion, AwsSdkVersionAPI, e.cfg.KiroVersion, mid,
	)
}

// DecorateRequest sets all required Kiro IDE headers on the request.
//
// Header set matches a real Kiro IDE 0.12.301 generateAssistantResponse
// request (captured 2026-06-08). Notable vs the old (0.11/kiro.rs) client:
// content-type is application/json (not x-amz-json-1.1), aws-sdk is 1.0.39
// with metric flag m/N, agent-mode is "spec" (0.12.x no longer sends "vibe"),
// and Connection: close is present.
func (e *Endpoint) DecorateRequest(req *http.Request, cred *KiroCredential, accessToken string) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", e.cfg.EffectiveAgentMode())
	req.Header.Set("x-amz-user-agent", e.xAmzUserAgent(cred))
	req.Header.Set("user-agent", e.userAgent(cred))
	req.Header.Set("host", e.host(cred))
	req.Header.Set("amz-sdk-invocation-id", newUUID())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Connection", "close")
}

// BuildHTTPClientExported is the exported wrapper around buildHTTPClient,
// used by the gateway service to construct per-credential clients.
func BuildHTTPClientExported(proxyURL string, timeout time.Duration) (*http.Client, error) {
	return buildHTTPClient(proxyURL, timeout)
}

// --- transient retry support (mirrors kiro.rs provider retry policy) ---

// MaxTransientRetries is the per-account retry count for transient upstream
// errors (429/408/5xx), matching kiro.rs MAX_RETRIES_PER_CREDENTIAL.
const MaxTransientRetries = 3

// IsTransientStatus reports whether an HTTP status should be retried in place
// (429 Too Many Requests, 408 Request Timeout, or any 5xx). Mirrors kiro.rs:
// these are upstream transient errors that must NOT disable/switch the account.
func IsTransientStatus(status int) bool {
	return status == 429 || status == 408 || status >= 500
}

// IsInPlaceRetryStatus reports whether an HTTP status should be retried on the
// SAME account/credential with backoff (429 Too Many Requests, 408 Request
// Timeout, or any 5xx).
//
// 429 is INCLUDED to mirror Kiro IDE's real behavior: the IDE's conversation
// stream (generateAssistantResponse) uses an AdaptiveRetryStrategy with
// maxAttempts=3 that treats ThrottlingException/429 as retryable, backing off
// (throttle base 500ms, ×5, cap 20s) and retrying the SAME credential — the IDE
// has no concept of "switching accounts". For Kiro a 429 is traffic protection
// ("slow down"), so the correct first response is to wait and retry the same
// credential; only after the in-place retries are exhausted does the gateway
// fail over to another credential (a multi-account safety net the IDE lacks).
// See ThrottleRetryDelay for the 429-specific backoff.
func IsInPlaceRetryStatus(status int) bool {
	return status == 429 || status == 408 || status >= 500
}

// IsThrottleStatus reports whether a status is a rate-limit/throttle (429) that
// should use the slower throttle-specific backoff (ThrottleRetryDelay) rather
// than the fast transient backoff (RetryDelay) used for 5xx/408.
func IsThrottleStatus(status int) bool {
	return status == 429
}

// RetryDelay returns an exponential backoff with jitter for the given attempt
// (0-based), used for fast-recovering transient errors (5xx/408 and network
// errors): base 200ms, cap 2s, +<=25% jitter.
func RetryDelay(attempt int) time.Duration {
	return backoffWithJitter(attempt, 200, 2000)
}

// ThrottleRetryDelay returns the backoff for a throttled (429/ThrottlingException)
// attempt, aligned with Kiro IDE 0.12.316's conversation retry strategy: the IDE
// wraps the AWS SDK adaptive strategy and multiplies the computed delay by 5
// (se10=5) over a throttling base of 500ms (THROTTLING_RETRY_DELAY_BASE), capped
// at 20s (MAXIMUM_RETRY_DELAY). We approximate the adaptive delay with an
// exponential backoff on a 500ms base, then apply the ×5 multiplier and 20s cap,
// plus <=25% jitter so concurrent requests on the same credential spread out
// (turning a same-account throttle into a natural stagger instead of a stampede).
func ThrottleRetryDelay(attempt int) time.Duration {
	const throttleBaseMs = 500
	const multiplier = 5
	const capMs = 20000
	return backoffWithJitter(attempt, throttleBaseMs*multiplier, capMs)
}

// backoffWithJitter computes an exponential backoff: baseMs << attempt (clamped
// to maxMs), plus up to 25% jitter. attempt is 0-based; the shift is bounded to
// avoid overflow.
func backoffWithJitter(attempt, baseMs, maxMs int) time.Duration {
	shift := attempt
	if shift > 6 {
		shift = 6
	}
	exp := baseMs << uint(shift)
	if exp > maxMs {
		exp = maxMs
	}
	jitterMax := exp / 4
	if jitterMax < 1 {
		jitterMax = 1
	}
	jitter := randInt(jitterMax + 1)
	return time.Duration(exp+jitter) * time.Millisecond
}
