// Package bigmodel implements the zhipu (bigmodel) login-link account protocol:
// the ZCode client handshake used by the official desktop client plus the
// risk-control HTTP client shared by every code path that talks to
// zcode.z.ai (login token exchange, reset-card status).
package bigmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Endpoint keys are the identity of a risk-controlled upstream endpoint. The
// pacing and negative-cache state of a RiskClient is keyed by them, so callers
// must pass one of these constants instead of an ad-hoc string.
const (
	// RiskEndpointOAuthToken is POST https://zcode.z.ai/api/v1/oauth/token
	// (M1 authorization-code exchange).
	RiskEndpointOAuthToken = "zcode_oauth_token"
	// RiskEndpointResetStatus is GET https://zcode.z.ai/api/v1/coding-plan/reset/status
	// (M4 read-only reset-card status).
	RiskEndpointResetStatus = "zcode_reset_status"
)

const (
	// riskBackoffBase is the first negative-cache window opened by a risk-control response.
	riskBackoffBase = time.Minute
	// riskBackoffMax caps the exponential growth of the negative-cache window.
	riskBackoffMax = 30 * time.Minute

	// riskCodeUnusualActivity is the observed zcode.z.ai "unusual activity" block code.
	riskCodeUnusualActivity = 3012
	// riskCodeParameterError is the observed zcode.z.ai parameter-error code.
	riskCodeParameterError = 3001
)

// HTTPDoer is the minimal HTTP transport seam the risk client needs.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RiskErrorKind classifies why the risk client refused to call upstream.
type RiskErrorKind string

const (
	// RiskErrorKindMinInterval: the per-endpoint minimum call interval has not elapsed.
	RiskErrorKindMinInterval RiskErrorKind = "min_interval"
	// RiskErrorKindBackoff: the endpoint is in a risk-control backoff window.
	RiskErrorKindBackoff RiskErrorKind = "backoff"
)

// RiskError is the classified, body-free error returned when the risk client
// short-circuits a call. It never embeds upstream URLs, tokens or response
// bodies, so it is safe to surface to operators.
type RiskError struct {
	EndpointKey string
	Kind        RiskErrorKind
	RetryAfter  time.Duration
	Reason      string
}

func (e *RiskError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("bigmodel risk client: endpoint %q blocked (%s, reason %s, retry after %s)",
			e.EndpointKey, e.Kind, e.Reason, e.RetryAfter)
	}
	return fmt.Sprintf("bigmodel risk client: endpoint %q blocked (%s, retry after %s)",
		e.EndpointKey, e.Kind, e.RetryAfter)
}

// riskEndpointState is the per-endpoint pacing and backoff state.
type riskEndpointState struct {
	lastCallAt time.Time
	failures   int
	nextTryAt  time.Time
	lastReason string
}

// RiskClient is the single seam every zcode.z.ai risk-controlled call goes
// through: it enforces a per-endpoint minimum call interval, coalesces
// concurrent calls to the same endpoint into one upstream request and keeps a
// negative cache with exponential backoff for risk-control responses.
type RiskClient struct {
	doer        HTTPDoer
	minInterval time.Duration
	now         func() time.Time

	flight singleflight.Group

	mu     sync.Mutex
	states map[string]*riskEndpointState
}

// NewRiskClient builds a risk client. minInterval is the global minimum delay
// between two upstream calls to the same endpoint key; zero disables pacing.
func NewRiskClient(doer HTTPDoer, minInterval time.Duration) *RiskClient {
	return newRiskClient(doer, minInterval, time.Now)
}

func newRiskClient(doer HTTPDoer, minInterval time.Duration, now func() time.Time) *RiskClient {
	if now == nil {
		now = time.Now
	}
	return &RiskClient{
		doer:        doer,
		minInterval: minInterval,
		now:         now,
		states:      make(map[string]*riskEndpointState),
	}
}

// Do performs req against the endpoint identified by endpointKey, honouring the
// pacing gate and the negative cache. Concurrent calls for the same endpoint key
// are coalesced into a single upstream request; every caller gets its own
// readable copy of the response body. A call refused by the gate returns a
// *RiskError with no upstream request made.
func (c *RiskClient) Do(ctx context.Context, endpointKey string, req *http.Request) (*http.Response, error) {
	value, err, _ := c.flight.Do(endpointKey, func() (any, error) {
		return c.doOnce(endpointKey, req)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*riskResult)
	if !ok || result == nil || result.resp == nil {
		return nil, fmt.Errorf("bigmodel risk client: no response for endpoint %q", endpointKey)
	}
	return result.response(), nil
}

// riskResult is the buffered upstream response shared by one single-flight call.
type riskResult struct {
	resp *http.Response
	body []byte
}

// response hands out an independent copy: the body of a coalesced response must
// be readable once per caller.
func (r *riskResult) response() *http.Response {
	clone := new(http.Response)
	*clone = *r.resp
	clone.Body = io.NopCloser(bytes.NewReader(r.body))
	clone.ContentLength = int64(len(r.body))
	return clone
}

// doOnce is the single-flight body: gate, call upstream, buffer the payload and
// maintain the per-endpoint negative cache from the response classification.
func (c *RiskClient) doOnce(endpointKey string, req *http.Request) (*riskResult, error) {
	now := c.now()
	if err := c.gate(endpointKey, now); err != nil {
		return nil, err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("bigmodel risk client: read endpoint %q response: %w", endpointKey, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("bigmodel risk client: close endpoint %q response: %w", endpointKey, closeErr)
	}
	reason := riskTriggerReason(resp.StatusCode, body)
	switch {
	case reason != "":
		c.recordFailure(endpointKey, now, reason)
	case resp.StatusCode >= http.StatusInternalServerError:
		// 5xx is a transport failure, not a verdict: it must neither open nor
		// clear the negative cache (only the error is reported to the caller).
	default:
		c.recordSuccess(endpointKey)
	}
	return &riskResult{resp: resp, body: body}, nil
}

// recordFailure opens (or escalates) the negative-cache window of an endpoint.
func (c *RiskClient) recordFailure(endpointKey string, now time.Time, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.stateLocked(endpointKey)
	state.failures++
	state.nextTryAt = now.Add(riskBackoffFor(state.failures))
	state.lastReason = reason
}

// recordSuccess closes any negative-cache window: the next block starts from
// the base window again.
func (c *RiskClient) recordSuccess(endpointKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.states[endpointKey]
	if state == nil {
		return
	}
	state.failures = 0
	state.nextTryAt = time.Time{}
	state.lastReason = ""
}

// riskBackoffFor doubles the negative-cache window per consecutive block,
// capped at riskBackoffMax.
func riskBackoffFor(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	backoff := riskBackoffBase
	for i := 1; i < failures && backoff < riskBackoffMax; i++ {
		backoff *= 2
	}
	if backoff > riskBackoffMax {
		backoff = riskBackoffMax
	}
	return backoff
}

// stateLocked returns the endpoint state, creating it on first use.
func (c *RiskClient) stateLocked(endpointKey string) *riskEndpointState {
	state := c.states[endpointKey]
	if state == nil {
		state = &riskEndpointState{}
		c.states[endpointKey] = state
	}
	return state
}

// gate checks and, when the call is allowed, records the call timestamp.
func (c *RiskClient) gate(endpointKey string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.stateLocked(endpointKey)

	if !state.nextTryAt.IsZero() && now.Before(state.nextTryAt) {
		return &RiskError{
			EndpointKey: endpointKey,
			Kind:        RiskErrorKindBackoff,
			RetryAfter:  state.nextTryAt.Sub(now),
			Reason:      state.lastReason,
		}
	}
	if c.minInterval > 0 && !state.lastCallAt.IsZero() && now.Sub(state.lastCallAt) < c.minInterval {
		return &RiskError{
			EndpointKey: endpointKey,
			Kind:        RiskErrorKindMinInterval,
			RetryAfter:  c.minInterval - now.Sub(state.lastCallAt),
		}
	}
	state.lastCallAt = now
	return nil
}

// riskTriggerReason classifies the upstream response: a non-empty result means
// the risk-control middleware blocked the call and the endpoint must back off.
// 3012 is the observed "unusual activity" block, 3001 the observed parameter
// rejection; 429 is the transport-level rate limit. 5xx responses are transient
// infrastructure failures and are never treated as risk verdicts.
func riskTriggerReason(status int, body []byte) string {
	if status >= http.StatusInternalServerError {
		return ""
	}
	if status == http.StatusTooManyRequests {
		return "http_429"
	}
	code, ok := responseBodyCode(body)
	if !ok {
		return ""
	}
	switch code {
	case riskCodeUnusualActivity:
		return "code_3012"
	case riskCodeParameterError:
		return "code_3001"
	}
	return ""
}

// responseBodyCode reads the top-level "code" field of an upstream JSON body.
// Non-JSON bodies and non-numeric codes are reported as absent.
func responseBodyCode(body []byte) (int, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	switch value := payload["code"].(type) {
	case float64:
		return int(value), true
	case string:
		code, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return code, true
	}
	return 0, false
}
