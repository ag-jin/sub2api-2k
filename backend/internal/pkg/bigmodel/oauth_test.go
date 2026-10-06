//go:build unit

package bigmodel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recordedCall is one request as the stub upstream saw it, captured before any
// transport rewriting: the URL is the production URL the code under test built.
type recordedCall struct {
	Method string
	URL    string
	Header http.Header
	Body   string
}

// stubUpstream is the counting HTTP doer used in place of a real network: it
// records every request and answers with a canned httptest response, so tests
// can assert both the exact wire shape and how many upstream calls happened.
type stubUpstream struct {
	handler http.HandlerFunc

	mu       sync.Mutex
	requests []recordedCall
}

func newStubUpstream(handler http.HandlerFunc) *stubUpstream {
	return &stubUpstream{handler: handler}
}

func (s *stubUpstream) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		body = string(raw)
	}

	s.mu.Lock()
	s.requests = append(s.requests, recordedCall{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header.Clone(),
		Body:   body,
	})
	s.mu.Unlock()

	replay := req.Clone(req.Context())
	replay.Body = io.NopCloser(strings.NewReader(body))
	recorder := httptest.NewRecorder()
	s.handler(recorder, replay)
	return recorder.Result(), nil
}

func (s *stubUpstream) calls() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedCall, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *stubUpstream) callCount() int { return len(s.calls()) }

func renderCall(t *testing.T, call recordedCall) string {
	t.Helper()
	return call.Method + " " + call.URL + " " + call.Body
}

// uniquePaths returns the distinct request paths the stub saw, in order.
func uniquePaths(calls []recordedCall) []string {
	seen := make(map[string]struct{}, len(calls))
	paths := make([]string, 0, len(calls))
	for _, call := range calls {
		parsed, err := neturl.Parse(call.URL)
		if err != nil {
			paths = append(paths, call.URL)
			continue
		}
		key := call.Method + " " + parsed.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		paths = append(paths, key)
	}
	return paths
}

func jsonBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out), "request body must be JSON: %q", raw)
	return out
}

// TestExchangeCodeSuccess pins the exchange wire contract from research
// FINAL-REPORT §六: POST https://zcode.z.ai/api/v1/oauth/token with
// {provider:"bigmodel", code, redirect_uri, state} and a JSON content type,
// answering {code:0, data:{token, bigmodel:{access_token, refresh_token}}}.
func TestExchangeCodeSuccess(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"token":"zcode-jwt-1","expires_in":3600,"bigmodel":{"access_token":"bigmodel-at-1","refresh_token":"bigmodel-rt-1"}}}`)
	})
	risk := newRiskClient(stub, 0, newFakeClock().Now)

	result, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	require.NoError(t, err)
	require.Equal(t, "bigmodel-at-1", result.AccessToken)
	require.Equal(t, "bigmodel-rt-1", result.RefreshToken)
	require.Equal(t, "zcode-jwt-1", result.ZCodeJWTToken)
	require.EqualValues(t, 3600, result.ExpiresIn)

	require.Equal(t, 1, stub.callCount(), "the exchange must issue exactly one upstream call")
	call := stub.calls()[0]
	require.Equal(t, http.MethodPost, call.Method)
	require.Equal(t, "https://zcode.z.ai/api/v1/oauth/token", call.URL)
	require.Equal(t, "application/json", call.Header.Get("Content-Type"))
	require.Equal(t, map[string]any{
		"provider":     "bigmodel",
		"code":         authCode,
		"redirect_uri": redirectURI,
		"state":        state,
	}, jsonBody(t, call.Body), "wire body: %s", renderCall(t, call))
}

// failingUpstream is a doer that always fails, standing in for a dead network.
type failingUpstream struct{ err error }

func (f failingUpstream) Do(*http.Request) (*http.Response, error) { return nil, f.err }

// TestExchangeCodeRejectsBadInputAndBadConfig covers the failures that must be
// caught before any upstream call: empty arguments (caller bug) and an
// incoherent transport configuration (wiring bug) both leave the hit counter at
// zero and are classified, never guessed from a message.
func TestExchangeCodeRejectsBadInputAndBadConfig(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	t.Run("empty auth code", func(t *testing.T) {
		t.Parallel()
		stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"code":0,"data":{"bigmodel":{"access_token":"at"}}}`)
		})
		risk := newRiskClient(stub, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "", "   ", redirectURI, state)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindInvalidInput, classified.Kind)
		require.Equal(t, "code", classified.Field)
		require.Equal(t, 0, stub.callCount(), "an invalid input must not reach upstream")
	})

	t.Run("empty redirect uri", func(t *testing.T) {
		t.Parallel()
		stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		risk := newRiskClient(stub, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "", authCode, "", state)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindInvalidInput, classified.Kind)
		require.Equal(t, "redirect_uri", classified.Field)
		require.Equal(t, 0, stub.callCount())
	})

	t.Run("empty state", func(t *testing.T) {
		t.Parallel()
		stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		risk := newRiskClient(stub, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, "")
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindInvalidInput, classified.Kind)
		require.Equal(t, "state", classified.Field)
		require.Equal(t, 0, stub.callCount())
	})

	t.Run("injected doer with proxy url", func(t *testing.T) {
		t.Parallel()
		stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		risk := newRiskClient(stub, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "http://127.0.0.1:3128", authCode, redirectURI, state)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindConfig, classified.Kind)
		require.Equal(t, 0, stub.callCount(), "a rejected configuration must not reach upstream")
	})

	t.Run("unparsable proxy url", func(t *testing.T) {
		t.Parallel()
		_, err := ExchangeCode(context.Background(), nil, "://not-a-proxy", authCode, redirectURI, state)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindConfig, classified.Kind)
	})

	t.Run("network failure", func(t *testing.T) {
		t.Parallel()
		risk := newRiskClient(failingUpstream{err: errors.New("dial tcp 127.0.0.1:443: connect: connection refused")}, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindTransport, classified.Kind)
		require.Equal(t, ErrorKindTransport, classified.Kind)
		require.False(t, IsAuthRejected(err), "a network failure must not look like a dead credential")
		require.False(t, IsRiskBlocked(err), "a network failure must not look like a risk verdict")
	})
}

// TestExchangeCodeGoesThroughRiskClient is the integration evidence for the
// "never bypass the risk client" rule: after a 3012 verdict the second exchange
// is refused by the negative cache without touching upstream again, so the call
// provably passes the shared risk gate rather than a direct connection.
func TestExchangeCodeGoesThroughRiskClient(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	clock := newFakeClock()
	stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":3012,"msg":"unusual activity"}`)
	})
	risk := newRiskClient(stub, 0, clock.Now)

	_, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	require.Error(t, err)
	require.Equal(t, 1, stub.callCount())

	_, err = ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	var classified *Error
	require.ErrorAs(t, err, &classified)
	require.Equal(t, ErrorKindRiskControl, classified.Kind)
	require.True(t, IsRiskBlocked(err))
	retryAfter, ok := RetryAfter(err)
	require.True(t, ok, "a risk refusal must expose the remaining window: %v", err)
	require.Positive(t, retryAfter)
	require.Equal(t, 1, stub.callCount(), "a blocked call must not reach upstream")

	// The window closes once the clock passes it, and the next call hits
	// upstream again - proof that only the shared gate, never a second path,
	// decides whether the exchange runs.
	clock.Advance(retryAfter + time.Second)
	_, err = ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	require.Error(t, err)
	var afterWindow *Error
	require.ErrorAs(t, err, &afterWindow)
	require.Equal(t, ErrorKindUpstreamCode, afterWindow.Kind)
	require.Equal(t, 2, stub.callCount())
}

// TestExchangeCodeRateLimited429OpensBackoff covers the transport-level limit:
// a 429 is a risk verdict for the shared gate, and the next call is refused
// locally instead of hammering the endpoint.
func TestExchangeCodeRateLimited429OpensBackoff(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	clock := newFakeClock()
	stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":429,"msg":"too many requests"}`)
	})
	risk := newRiskClient(stub, 0, clock.Now)

	_, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	var limited *Error
	require.ErrorAs(t, err, &limited)
	require.Equal(t, ErrorKindHTTPStatus, limited.Kind)
	require.Equal(t, http.StatusTooManyRequests, limited.Status)
	require.True(t, IsRiskBlocked(err))

	_, err = ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	var blocked *Error
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, ErrorKindRiskControl, blocked.Kind)
	require.Equal(t, 1, stub.callCount(), "the 429 backoff window must stop the second call locally")
}

// TestExchangeCodeFailureClassification pins the error taxonomy that 04 maps
// to admin-facing codes and 09 uses to tell "credential dead" from "retry
// later": every upstream verdict is classified, never string-matched.
func TestExchangeCodeFailureClassification(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	cases := []struct {
		name         string
		status       int
		body         string
		wantKind     ErrorKind
		wantStatus   int
		wantCode     int
		wantField    string
		authRejected bool
		riskBlocked  bool
	}{
		{
			name: "unusual activity verdict", status: http.StatusOK,
			body:     `{"code":3012,"msg":"unusual activity"}`,
			wantKind: ErrorKindUpstreamCode, wantStatus: 200, wantCode: 3012, riskBlocked: true,
		},
		{
			name: "parameter rejection", status: http.StatusOK,
			body:     `{"code":3001,"msg":"parameter error"}`,
			wantKind: ErrorKindUpstreamCode, wantStatus: 200, wantCode: 3001, riskBlocked: true,
		},
		{
			name: "other upstream code", status: http.StatusOK,
			body:     `{"code":5001,"msg":"internal"}`,
			wantKind: ErrorKindUpstreamCode, wantStatus: 200, wantCode: 5001,
		},
		{
			name: "server error", status: http.StatusInternalServerError,
			body:     `<html>bad gateway</html>`,
			wantKind: ErrorKindHTTPStatus, wantStatus: 500,
		},
		{
			name: "unauthorized", status: http.StatusUnauthorized,
			body:     `{"code":401,"msg":"unauthorized"}`,
			wantKind: ErrorKindHTTPStatus, wantStatus: 401, authRejected: true,
		},
		{
			name: "forbidden", status: http.StatusForbidden,
			body:     `{"code":403,"msg":"forbidden"}`,
			wantKind: ErrorKindHTTPStatus, wantStatus: 403, authRejected: true,
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			body:     `{"code":429,"msg":"too many requests"}`,
			wantKind: ErrorKindHTTPStatus, wantStatus: 429, riskBlocked: true,
		},
		{
			name: "non json body", status: http.StatusOK,
			body:     `<html>proxy error</html>`,
			wantKind: ErrorKindInvalidBody, wantStatus: 200,
		},
		{
			name: "missing data", status: http.StatusOK,
			body:     `{"code":0,"msg":"ok"}`,
			wantKind: ErrorKindMissingField, wantStatus: 200, wantField: "data",
		},
		{
			name: "missing bigmodel block", status: http.StatusOK,
			body:     `{"code":0,"data":{"token":"zcode-jwt"}}`,
			wantKind: ErrorKindMissingField, wantStatus: 200, wantField: "data.bigmodel.access_token",
		},
		{
			name: "empty access token", status: http.StatusOK,
			body:     `{"code":0,"data":{"bigmodel":{"access_token":""}}}`,
			wantKind: ErrorKindMissingField, wantStatus: 200, wantField: "data.bigmodel.access_token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			risk := newRiskClient(stub, 0, newFakeClock().Now)

			result, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
			require.Error(t, err)
			require.Nil(t, result)

			var classified *Error
			require.ErrorAs(t, err, &classified, "error must be classified: %v", err)
			require.Equal(t, ErrorKind(tc.wantKind), classified.Kind)
			require.Equal(t, "exchange_code", classified.Op)
			require.Equal(t, tc.wantStatus, classified.Status)
			require.Equal(t, tc.wantCode, classified.Code)
			require.Equal(t, tc.wantField, classified.Field)
			require.Equal(t, tc.authRejected, IsAuthRejected(err), "IsAuthRejected mismatch: %v", err)
			require.Equal(t, tc.riskBlocked, IsRiskBlocked(err), "IsRiskBlocked mismatch: %v", err)
			require.NotContains(t, err.Error(), "bigmodel-at-1", "the error must not contain credentials")
		})
	}
}

// TestExchangeCodeHTTPStatusErrorCarriesUpstreamBody 票 34：非 200 的兑换失败必须把
// 上游响应体（截断）并进错误信息——上游对一切被拒兑换统一回
// 500 {"code":2007,"msg":"http error","logid":...}，只有 logid 能向智谱侧定位；
// 请求入参（authCode/state/redirect_uri）不得出现在错误里。
func TestExchangeCodeHTTPStatusErrorCarriesUpstreamBody(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	t.Run("embeds the upstream body so the logid stays actionable", func(t *testing.T) {
		t.Parallel()

		stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":2007,"msg":"http error","logid":"logid-abc-123"}`)
		})
		risk := newRiskClient(stub, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
		require.Error(t, err)

		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, ErrorKindHTTPStatus, classified.Kind)
		require.Equal(t, http.StatusInternalServerError, classified.Status)
		require.Contains(t, classified.Message, `logid-abc-123`)
		require.Contains(t, err.Error(), `logid-abc-123`)

		// 只带响应体：请求入参（尤其是授权码）不得回显
		require.NotContains(t, err.Error(), authCode)
		require.NotContains(t, err.Error(), state)
		require.NotContains(t, err.Error(), redirectURI)
	})

	t.Run("bounds the embedded body to 200 bytes", func(t *testing.T) {
		t.Parallel()

		stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, strings.Repeat("x", 500))
		})
		risk := newRiskClient(stub, 0, newFakeClock().Now)

		_, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
		require.Error(t, err)

		var classified *Error
		require.ErrorAs(t, err, &classified)
		require.Equal(t, strings.Repeat("x", 200)+"...", classified.Message)
	})
}

// rewriteTransport points every request at a local test server while the code
// under test keeps building the production URL; the original URL is handed to
// the callback so the assertion still sees what the caller asked for.
type rewriteTransport struct {
	target *neturl.URL
	base   http.RoundTripper
	seen   func(originalURL string)
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.seen != nil {
		rt.seen(req.URL.String())
	}
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	req.Host = ""
	return rt.base.RoundTrip(req)
}

// TestExchangeCodeRefreshTokenTolerance pins the ticket's refresh-token
// contract: the relay may omit the token or answer an empty string, and either
// way the exchange succeeds with an empty RefreshToken so the caller can tell
// "nothing to store". The camelCase spelling the relay sometimes answers is
// accepted, with the snake_case value taking precedence.
func TestExchangeCodeRefreshTokenTolerance(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	cases := []struct {
		name        string
		body        string
		wantRefresh string
		wantJWT     string
		wantExpires int64
	}{
		{
			name:        "snake case present",
			body:        `{"code":0,"data":{"token":"zcode-jwt","expires_in":60,"bigmodel":{"access_token":"at","refresh_token":"rt"}}}`,
			wantRefresh: "rt", wantJWT: "zcode-jwt", wantExpires: 60,
		},
		{
			name:    "empty refresh token",
			body:    `{"code":0,"data":{"token":"zcode-jwt","bigmodel":{"access_token":"at","refresh_token":""}}}`,
			wantJWT: "zcode-jwt",
		},
		{
			name: "absent refresh token",
			body: `{"code":0,"data":{"bigmodel":{"access_token":"at"}}}`,
		},
		{
			name:        "camel case refresh token",
			body:        `{"code":0,"data":{"bigmodel":{"access_token":"at","refreshToken":"rt-camel"}}}`,
			wantRefresh: "rt-camel",
		},
		{
			name:        "snake case wins over camel case",
			body:        `{"code":0,"data":{"bigmodel":{"access_token":"at","refresh_token":"rt-snake","refreshToken":"rt-camel"}}}`,
			wantRefresh: "rt-snake",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})
			risk := newRiskClient(stub, 0, newFakeClock().Now)

			result, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
			require.NoError(t, err)
			require.Equal(t, "at", result.AccessToken)
			require.Equal(t, tc.wantRefresh, result.RefreshToken)
			require.Equal(t, tc.wantJWT, result.ZCodeJWTToken)
			require.EqualValues(t, tc.wantExpires, result.ExpiresIn)
		})
	}
}

// TestExchangeCodeBoundsUpstreamMessage keeps an oversized upstream msg out of
// logs and API responses.
func TestExchangeCodeBoundsUpstreamMessage(t *testing.T) {
	t.Parallel()

	stub := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":3012,"msg":"`+strings.Repeat("x", 4096)+`"}`)
	})
	risk := newRiskClient(stub, 0, newFakeClock().Now)

	_, err := ExchangeCode(context.Background(), risk, "", "auth-code-1", "http://127.0.0.1/cb", "0123456789abcdef0123456789abcdef")
	var classified *Error
	require.ErrorAs(t, err, &classified)
	require.Equal(t, 3012, classified.Code)
	require.LessOrEqual(t, len(err.Error()), 400, "upstream text must be bounded: %d bytes", len(err.Error()))
}

// TestExchangeCodeOverRealHTTP runs the exchange against a real httptest
// server so the request provably survives HTTP serialization: same production
// URL, JSON content type, JSON body.
func TestExchangeCodeOverRealHTTP(t *testing.T) {
	t.Parallel()

	const (
		authCode    = "auth-code-1"
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	type seenRequest struct {
		path        string
		contentType string
		body        string
	}
	received := make(chan seenRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		received <- seenRequest{path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: string(raw)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"token":"zcode-jwt","bigmodel":{"access_token":"at"}}}`)
	}))
	t.Cleanup(server.Close)

	target, err := neturl.Parse(server.URL)
	require.NoError(t, err)

	var originalURL string
	client := &http.Client{Transport: rewriteTransport{
		target: target,
		base:   http.DefaultTransport,
		seen:   func(url string) { originalURL = url },
	}}
	risk := newRiskClient(client, 0, newFakeClock().Now)

	result, err := ExchangeCode(context.Background(), risk, "", authCode, redirectURI, state)
	require.NoError(t, err)
	require.Equal(t, "at", result.AccessToken)
	require.Equal(t, "https://zcode.z.ai/api/v1/oauth/token", originalURL)

	call := <-received
	require.Equal(t, "/api/v1/oauth/token", call.path)
	require.Equal(t, "application/json", call.contentType)
	require.Equal(t, map[string]any{
		"provider":     "bigmodel",
		"code":         authCode,
		"redirect_uri": redirectURI,
		"state":        state,
	}, jsonBody(t, call.body))
}

// TestExchangeCodeDefaultTransportHonoursProxy covers the one case proxyURL
// applies to - no doer injected. A stub proxy records the CONNECT the client
// issues for the risk-controlled https endpoint and refuses it, so the exchange
// fails in transport: proof that the proxy sits on the call path instead of
// being silently dropped.
func TestExchangeCodeDefaultTransportHonoursProxy(t *testing.T) {
	t.Parallel()

	type connectRequest struct {
		method string
		host   string
	}
	seen := make(chan connectRequest, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- connectRequest{method: r.Method, host: r.Host}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	_, err := ExchangeCode(context.Background(), nil, proxy.URL,
		"auth-code-1", "http://127.0.0.1/cb", "0123456789abcdef0123456789abcdef")
	var classified *Error
	require.ErrorAs(t, err, &classified)
	require.Equal(t, ErrorKindTransport, classified.Kind)

	select {
	case request := <-seen:
		require.Equal(t, http.MethodConnect, request.method)
		require.Equal(t, "zcode.z.ai:443", request.host)
	case <-time.After(5 * time.Second):
		t.Fatal("the configured proxy was never used")
	}
}

// TestGenerateStateIsHex32AndUnique pins the state shape the bigmodel login URL
// needs: 32 lowercase hex characters (16 random bytes) that never repeat.
func TestGenerateStateIsHex32AndUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 32)
	for i := 0; i < 32; i++ {
		state, err := GenerateState()
		require.NoError(t, err)
		require.Len(t, state, 32, "state must be 32 hex characters")
		_, decodeErr := hex.DecodeString(state)
		require.NoError(t, decodeErr, "state must be hex: %q", state)
		require.Equal(t, strings.ToLower(state), state, "state must be lowercase hex")
		require.NotContains(t, seen, state, "state must be random")
		seen[state] = struct{}{}
	}
}

// TestBuildLoginURLShape asserts the wire shape of the authorization URL
// (research FINAL-REPORT §六): base, appId=zcode, and redirect/state
// round-tripping through percent-encoding, with no other parameter carried.
func TestBuildLoginURLShape(t *testing.T) {
	t.Parallel()

	const (
		redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		state       = "0123456789abcdef0123456789abcdef"
	)

	loginURL := BuildLoginURL(redirectURI, state)
	require.True(t, strings.HasPrefix(loginURL, "https://bigmodel.cn/login?"), loginURL)

	parsed, err := neturl.Parse(loginURL)
	require.NoError(t, err)
	query := parsed.Query()
	require.Equal(t, "zcode", query.Get("appId"))
	require.Equal(t, redirectURI, query.Get("redirect"))
	require.Equal(t, state, query.Get("state"))
	require.Len(t, query, 3, "login URL must carry exactly appId/redirect/state: %s", loginURL)
}
