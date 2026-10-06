//go:build unit

package bigmodel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newRiskTestRequest builds a fresh synthetic request for the fake upstream.
func newRiskTestRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, http.NoBody)
	require.NoError(t, err)
	return req
}

// fakeClock is the injected time source: tests advance it explicitly and never sleep.
type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// TestRiskClientMinIntervalDisabledAndPerEndpoint covers the two edges of the
// pacing gate: a zero interval disables it entirely, and the interval is scoped
// to one endpoint key so another endpoint is never throttled by it.
func TestRiskClientMinIntervalDisabledAndPerEndpoint(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"code":200,"msg":"ok"}`)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	clock := newFakeClock()

	disabled := newRiskClient(srv.Client(), 0, clock.Now)
	for i := 0; i < 2; i++ {
		resp, err := disabled.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, srv.URL))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, int32(2), hits.Load(), "a zero interval must not pace calls")

	clock.Advance(time.Hour)
	scoped := newRiskClient(srv.Client(), 30*time.Second, clock.Now)
	resp, err := scoped.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, srv.URL))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	resp, err = scoped.Do(ctx, RiskEndpointResetStatus, newRiskTestRequest(t, srv.URL))
	require.NoError(t, err, "another endpoint key must not be throttled")
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int32(4), hits.Load())
}

// TestRiskClientEnforcesMinIntervalPerEndpoint covers the pacing gate of the
// risk client: a second call for the same endpointKey inside the configured
// interval must not reach upstream and must return a classified error carrying
// the endpoint key and the remaining wait.
func TestRiskClientEnforcesMinIntervalPerEndpoint(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"code":200,"msg":"ok"}`)
	}))
	t.Cleanup(srv.Close)

	clock := newFakeClock()
	client := newRiskClient(srv.Client(), 30*time.Second, clock.Now)
	ctx := context.Background()

	resp, err := client.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, srv.URL))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int32(1), hits.Load())

	clock.Advance(10 * time.Second)
	resp, err = client.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, srv.URL))
	require.Nil(t, resp)
	var riskErr *RiskError
	require.ErrorAs(t, err, &riskErr)
	require.Equal(t, RiskErrorKindMinInterval, riskErr.Kind)
	require.Equal(t, RiskEndpointOAuthToken, riskErr.EndpointKey)
	require.Equal(t, 20*time.Second, riskErr.RetryAfter)
	require.Equal(t, int32(1), hits.Load(), "call inside the min interval must not reach upstream")

	clock.Advance(25 * time.Second) // 35s since the last upstream call
	resp, err = client.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, srv.URL))
	require.NoError(t, err)
	require.Equal(t, int32(2), hits.Load())
	require.NoError(t, resp.Body.Close())
}

// TestRiskClientCoalescesConcurrentCallsPerEndpoint covers singleflight: N
// concurrent calls for the same endpoint key must produce exactly one upstream
// request, and every caller must still receive a readable response body.
func TestRiskClientCoalescesConcurrentCallsPerEndpoint(t *testing.T) {
	t.Parallel()

	const callers = 5
	const body = `{"code":200,"msg":"ok"}`
	var hits atomic.Int32
	var enteredOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	client := newRiskClient(srv.Client(), 30*time.Second, newFakeClock().Now)
	ctx := context.Background()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	responses := make([]*http.Response, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			responses[i], errs[i] = client.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, srv.URL))
		}(i)
	}
	close(start)

	<-entered // the single upstream request is in flight
	// Give the remaining callers a moment to join the in-flight request before
	// it completes: they share its result, so no real-time pacing is involved.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	require.Equal(t, int32(1), hits.Load(), "concurrent calls for one endpoint must coalesce into a single upstream request")
	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.NotNil(t, responses[i])
		require.Equal(t, http.StatusOK, responses[i].StatusCode)
		got, err := io.ReadAll(responses[i].Body)
		require.NoError(t, err)
		require.Equal(t, body, string(got), "every coalesced caller must get its own readable body")
		require.NoError(t, responses[i].Body.Close())
	}
}

// TestRiskClientNegativeCacheOnRiskResponses covers the risk-control negative
// cache: a 3012 / 3001 body code or an HTTP 429 puts the endpoint into a backoff
// window during which no upstream request is made and a classified error is
// returned to every caller.
func TestRiskClientNegativeCacheOnRiskResponses(t *testing.T) {
	t.Parallel()

	const riskBackoffWindow = 60 * time.Second

	cases := []struct {
		name       string
		status     int
		body       string
		wantReason string
	}{
		{
			name:       "unusual activity 3012",
			status:     http.StatusOK,
			body:       `{"code":3012,"msg":"unusual activity detected"}`,
			wantReason: "code_3012",
		},
		{
			name:       "parameter error 3001",
			status:     http.StatusOK,
			body:       `{"code":3001,"msg":"parameter error"}`,
			wantReason: "code_3001",
		},
		{
			name:       "http 429",
			status:     http.StatusTooManyRequests,
			body:       `{"error":"too many requests"}`,
			wantReason: "http_429",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			clock := newFakeClock()
			client := newRiskClient(srv.Client(), 30*time.Second, clock.Now)
			ctx := context.Background()

			// The triggering response is passed through untouched: the caller
			// still classifies and reports the upstream error itself.
			resp, err := client.Do(ctx, RiskEndpointResetStatus, newRiskTestRequest(t, srv.URL))
			require.NoError(t, err)
			require.Equal(t, tc.status, resp.StatusCode)
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(got))
			require.NoError(t, resp.Body.Close())
			require.Equal(t, int32(1), hits.Load())

			clock.Advance(time.Second)
			resp, err = client.Do(ctx, RiskEndpointResetStatus, newRiskTestRequest(t, srv.URL))
			require.Nil(t, resp)
			var riskErr *RiskError
			require.ErrorAs(t, err, &riskErr)
			require.Equal(t, RiskErrorKindBackoff, riskErr.Kind)
			require.Equal(t, RiskEndpointResetStatus, riskErr.EndpointKey)
			require.Equal(t, tc.wantReason, riskErr.Reason)
			require.Equal(t, riskBackoffWindow-time.Second, riskErr.RetryAfter)
			require.Equal(t, int32(1), hits.Load(), "a backoff window must not reach upstream")
		})
	}
}

// TestRiskClientBackoffGrowsExponentiallyAndResetsOnSuccess covers the negative
// cache shape over time: consecutive risk blocks double the window (60s → 120s),
// and a successful call clears it so the next block starts from the base again.
func TestRiskClientBackoffGrowsExponentiallyAndResetsOnSuccess(t *testing.T) {
	t.Parallel()

	const (
		baseWindow   = 60 * time.Second
		secondWindow = 120 * time.Second
	)

	// Upstream sequence: hit 1 and 2 are blocked, hit 3 succeeds, hit 4 is blocked again.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 3 {
			_, _ = io.WriteString(w, `{"code":200,"msg":"ok"}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":3012,"msg":"unusual activity detected"}`)
	}))
	t.Cleanup(srv.Close)

	clock := newFakeClock()
	client := newRiskClient(srv.Client(), 30*time.Second, clock.Now)
	ctx := context.Background()

	call := func() *http.Response {
		t.Helper()
		resp, err := client.Do(ctx, RiskEndpointResetStatus, newRiskTestRequest(t, srv.URL))
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp
	}
	assertBlocked := func(wantRetryAfter time.Duration) {
		t.Helper()
		resp, err := client.Do(ctx, RiskEndpointResetStatus, newRiskTestRequest(t, srv.URL))
		require.Nil(t, resp)
		var riskErr *RiskError
		require.ErrorAs(t, err, &riskErr)
		require.Equal(t, RiskErrorKindBackoff, riskErr.Kind)
		require.Equal(t, wantRetryAfter, riskErr.RetryAfter)
	}

	// First block opens the base window.
	call()
	clock.Advance(time.Second)
	assertBlocked(baseWindow - time.Second)

	// A second consecutive block doubles the window.
	clock.Advance(baseWindow - time.Second)
	call()
	clock.Advance(time.Second)
	assertBlocked(secondWindow - time.Second)

	// A success clears the negative cache.
	clock.Advance(secondWindow - time.Second)
	resp := call()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The next block therefore starts from the base window again.
	clock.Advance(31 * time.Second) // clear the 30s min call interval
	call()
	clock.Advance(time.Second)
	assertBlocked(baseWindow - time.Second)
	require.Equal(t, int32(4), hits.Load())
}

// doerFunc adapts a function to the HTTPDoer seam.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestRiskClientTransientFailuresDoNotEnterNegativeCache covers the other half
// of the cache contract: network errors and 5xx responses are transport
// failures, not risk verdicts. They must not open a backoff window and must not
// touch an existing one (neither reset nor escalate it).
func TestRiskClientTransientFailuresDoNotEnterNegativeCache(t *testing.T) {
	t.Parallel()

	t.Run("http 5xx", func(t *testing.T) {
		t.Parallel()

		// hit 1: risk block, hit 2: 5xx that even carries a risk code, hit 3: risk block.
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 2 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"code":3012,"msg":"gateway error page"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":3012,"msg":"unusual activity detected"}`)
		}))
		t.Cleanup(srv.Close)

		clock := newFakeClock()
		// Pacing off: this test isolates the negative cache.
		client := newRiskClient(srv.Client(), 0, clock.Now)
		ctx := context.Background()

		do := func() (*http.Response, error) {
			return client.Do(ctx, RiskEndpointResetStatus, newRiskTestRequest(t, srv.URL))
		}

		resp, err := do()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, resp.Body.Close())

		clock.Advance(time.Minute) // leave the first backoff window
		resp, err = do()
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, int32(2), hits.Load(), "a 5xx must reach upstream and report its own status")

		// The 5xx must neither reset nor escalate the risk state: this block is
		// the second consecutive block, so its window is 120s.
		clock.Advance(time.Second)
		resp, err = do()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, resp.Body.Close())

		clock.Advance(time.Second)
		resp, err = do()
		require.Nil(t, resp)
		var riskErr *RiskError
		require.ErrorAs(t, err, &riskErr)
		require.Equal(t, RiskErrorKindBackoff, riskErr.Kind)
		require.Equal(t, 119*time.Second, riskErr.RetryAfter)
		require.Equal(t, int32(3), hits.Load())
	})

	t.Run("network error", func(t *testing.T) {
		t.Parallel()

		netErr := errors.New("dial tcp: connection refused")
		var calls atomic.Int32
		client := newRiskClient(doerFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, netErr
		}), 0, newFakeClock().Now)
		ctx := context.Background()

		for i := 0; i < 2; i++ {
			resp, err := client.Do(ctx, RiskEndpointOAuthToken, newRiskTestRequest(t, "https://zcode.z.ai/api/v1/oauth/token"))
			require.Nil(t, resp)
			require.ErrorIs(t, err, netErr)
			var riskErr *RiskError
			require.False(t, errors.As(err, &riskErr), "a transport error must not be classified as a risk block")
		}
		require.Equal(t, int32(2), calls.Load(), "a network error must not open a backoff window")
	})
}
