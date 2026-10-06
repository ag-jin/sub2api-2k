//go:build unit

package bigmodel

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// resetStatusTestCredentials are the credential pair one account carries.
const (
	resetStatusTestJWT    = "zcode-jwt-raw-value"
	resetStatusTestAccess = "bigmodel-access-token"
)

// TestResetStatusReaderFetchesAndParsesPools covers the happy path: one GET
// through the risk client, the three fixed auth headers on the wire, and both
// card pools parsed with expire_at milliseconds turned into UTC times.
func TestResetStatusReaderFetchesAndParsesPools(t *testing.T) {
	t.Parallel()

	// Expiry literals are epoch milliseconds of 2026-10-06T09:30:00Z,
	// 2026-10-12T00:00:00Z and 2026-09-30T18:45:00Z.
	upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{`+
			`"available_five_hour_resets":[{"expire_at":1791279000000}],`+
			`"available_week_resets":[{"expire_at":1791763200000},{"expire_at":1790793900000}],`+
			`"latest_five_hour_reset_history":{"used_at":1791200000000},`+
			`"latest_week_reset_history":null,`+
			`"has_unread_history":true}}`)
	})

	reader := NewResetStatusReader(NewRiskClient(upstream, 0), 0)

	status, err := reader.Fetch(context.Background(), resetStatusTestJWT, resetStatusTestAccess)
	require.NoError(t, err)
	require.NotNil(t, status)

	require.Equal(t, []ResetCard{
		{Type: ResetCardTypeFiveHour, ExpireAt: time.UnixMilli(1791279000000).UTC()},
	}, status.FiveHourCards)
	require.Equal(t, []ResetCard{
		{Type: ResetCardTypeWeek, ExpireAt: time.UnixMilli(1791763200000).UTC()},
		{Type: ResetCardTypeWeek, ExpireAt: time.UnixMilli(1790793900000).UTC()},
	}, status.WeekCards)
	require.True(t, status.HasUnreadHistory)

	calls := upstream.calls()
	require.Len(t, calls, 1)
	require.Equal(t, http.MethodGet, calls[0].Method)
	require.Equal(t, ResetStatusEndpoint, calls[0].URL)
	require.Empty(t, calls[0].Body)
	require.Equal(t, resetStatusTestJWT, calls[0].Header.Get("Authorization"),
		"the zcodejwt token is sent raw, never as a Bearer credential")
	require.Equal(t, resetStatusTestAccess, calls[0].Header.Get("X-Bigmodel-Authorization"))
	require.Equal(t, "PERSONAL", calls[0].Header.Get("Bigmodel-Target-Type"))
}

// TestResetStatusReaderToleratesEmptyAndPartialPools covers the pool edges: an
// account with no cards, an envelope that omits the pools entirely, a single
// populated pool, and a card whose expire_at is absent or null (the card is
// real, only its deadline is unknown, so it must still be reported).
func TestResetStatusReaderToleratesEmptyAndPartialPools(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantFive []ResetCard
		wantWeek []ResetCard
	}{
		{
			name: "both pools empty",
			body: `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[],"available_week_resets":[]}}`,
		},
		{
			name: "pools absent from envelope",
			body: `{"code":0,"msg":"ok","data":{}}`,
		},
		{
			name: "only the five hour pool is populated",
			body: `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{"expire_at":1791279000000}]}}`,
			wantFive: []ResetCard{
				{Type: ResetCardTypeFiveHour, ExpireAt: time.UnixMilli(1791279000000).UTC()},
			},
		},
		{
			name: "cards with missing or null expire_at are kept",
			body: `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{},` +
				`{"expire_at":null}],"available_week_resets":[{"expire_at":0}]}}`,
			wantFive: []ResetCard{
				{Type: ResetCardTypeFiveHour},
				{Type: ResetCardTypeFiveHour},
			},
			wantWeek: []ResetCard{{Type: ResetCardTypeWeek}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			reader := NewResetStatusReader(NewRiskClient(upstream, 0), 0)

			status, err := reader.Fetch(context.Background(), resetStatusTestJWT, resetStatusTestAccess)
			require.NoError(t, err)
			require.NotNil(t, status)
			require.Equal(t, tc.wantFive, status.FiveHourCards)
			require.Equal(t, tc.wantWeek, status.WeekCards)
			require.False(t, status.HasUnreadHistory)
		})
	}
}

// TestResetStatusReaderFailureClassification covers how each upstream failure
// is reported: the caller must be able to tell "no cards" (a successful empty
// status) from "could not read" by inspecting the error, never the status.
func TestResetStatusReaderFailureClassification(t *testing.T) {
	t.Parallel()

	longMessage := strings.Repeat("x", 300)

	cases := []struct {
		name        string
		status      int
		body        string
		wantKind    ErrorKind
		wantStatus  int
		wantCode    int
		wantField   string
		wantRisk    bool
		wantMessage string
	}{
		{
			name:        "non zero code",
			status:      http.StatusOK,
			body:        `{"code":3301,"msg":"reset status rejected"}`,
			wantKind:    ErrorKindUpstreamCode,
			wantStatus:  http.StatusOK,
			wantCode:    3301,
			wantMessage: "reset status rejected",
		},
		{
			name:       "success envelope without data",
			status:     http.StatusOK,
			body:       `{"code":0,"msg":"ok"}`,
			wantKind:   ErrorKindMissingField,
			wantStatus: http.StatusOK,
			wantField:  "data",
		},
		{
			name:       "body is not json",
			status:     http.StatusOK,
			body:       `<html>gateway</html>`,
			wantKind:   ErrorKindInvalidBody,
			wantStatus: http.StatusOK,
		},
		{
			name:       "http 500 is a transport failure, not a risk verdict",
			status:     http.StatusInternalServerError,
			body:       `{"code":3012,"msg":"unusual activity"}`,
			wantKind:   ErrorKindHTTPStatus,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "http 429 is classified as risk blocked",
			status:     http.StatusTooManyRequests,
			body:       `{"code":0,"msg":"too many requests"}`,
			wantKind:   ErrorKindHTTPStatus,
			wantStatus: http.StatusTooManyRequests,
			wantRisk:   true,
		},
		{
			name:        "upstream message is bounded",
			status:      http.StatusOK,
			body:        `{"code":4001,"msg":"` + longMessage + `"}`,
			wantKind:    ErrorKindUpstreamCode,
			wantStatus:  http.StatusOK,
			wantCode:    4001,
			wantMessage: longMessage[:maxErrorMessageBytes] + "...",
		},
		{
			name:        "risk verdict code in a 200 body",
			status:      http.StatusOK,
			body:        `{"code":3012,"msg":"unusual activity"}`,
			wantKind:    ErrorKindUpstreamCode,
			wantStatus:  http.StatusOK,
			wantCode:    3012,
			wantRisk:    true,
			wantMessage: "unusual activity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			reader := NewResetStatusReader(NewRiskClient(upstream, 0), 0)

			status, err := reader.Fetch(context.Background(), resetStatusTestJWT, resetStatusTestAccess)
			require.Error(t, err)
			require.Nil(t, status)

			var classified *Error
			require.ErrorAs(t, err, &classified)
			require.Equal(t, opResetStatus, classified.Op)
			require.Equal(t, tc.wantKind, classified.Kind)
			require.Equal(t, tc.wantStatus, classified.Status)
			require.Equal(t, tc.wantCode, classified.Code)
			require.Equal(t, tc.wantField, classified.Field)
			require.Equal(t, tc.wantRisk, IsRiskBlocked(err))
			require.Equal(t, tc.wantMessage, classified.Message)
			require.NotContains(t, err.Error(), resetStatusTestJWT, "errors must never leak the zcodejwt token")
			require.NotContains(t, err.Error(), resetStatusTestAccess, "errors must never leak the access token")
		})
	}
}

// TestResetStatusReaderRejectsMissingInput covers the guard rails: an account
// without the login credentials, or a reader built without the risk client,
// must fail before any network call is attempted.
func TestResetStatusReaderRejectsMissingInput(t *testing.T) {
	t.Parallel()

	upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	})
	ctx := context.Background()

	cases := []struct {
		name      string
		reader    *ResetStatusReader
		jwt       string
		access    string
		wantKind  ErrorKind
		wantField string
	}{
		{
			name:      "missing zcodejwt token",
			reader:    NewResetStatusReader(NewRiskClient(upstream, 0), 0),
			access:    resetStatusTestAccess,
			wantKind:  ErrorKindInvalidInput,
			wantField: "zcode_jwt_token",
		},
		{
			name:      "missing access token",
			reader:    NewResetStatusReader(NewRiskClient(upstream, 0), 0),
			jwt:       resetStatusTestJWT,
			wantKind:  ErrorKindInvalidInput,
			wantField: "access_token",
		},
		{
			name:     "no risk client injected",
			reader:   NewResetStatusReader(nil, 0),
			jwt:      resetStatusTestJWT,
			access:   resetStatusTestAccess,
			wantKind: ErrorKindConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, err := tc.reader.Fetch(ctx, tc.jwt, tc.access)
			require.Error(t, err)
			require.Nil(t, status)

			var classified *Error
			require.ErrorAs(t, err, &classified)
			require.Equal(t, opResetStatus, classified.Op)
			require.Equal(t, tc.wantKind, classified.Kind)
			require.Equal(t, tc.wantField, classified.Field)
		})
	}

	require.Zero(t, upstream.callCount(), "rejected input must never reach the network")
	require.False(t, IsRiskBlocked(nil))
}

// TestResetStatusReaderHonoursRiskBackoff covers the risk-control path: a 3012
// verdict or an HTTP 429 opens the risk client's negative cache, and every read
// inside that window fails with a classified backoff error instead of hitting
// upstream again. Once the window elapses the reader recovers by itself.
func TestResetStatusReaderHonoursRiskBackoff(t *testing.T) {
	t.Parallel()

	const successBody = `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{"expire_at":1791279000000}]}}`

	cases := []struct {
		name     string
		status   int
		body     string
		wantKind ErrorKind
		wantCode int
	}{
		{
			name:     "risk verdict in a 200 body",
			status:   http.StatusOK,
			body:     `{"code":3012,"msg":"unusual activity"}`,
			wantKind: ErrorKindUpstreamCode,
			wantCode: 3012,
		},
		{
			name:     "http 429",
			status:   http.StatusTooManyRequests,
			body:     `{"code":0,"msg":"too many requests"}`,
			wantKind: ErrorKindHTTPStatus,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			var recovering atomic.Bool
			upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				if recovering.Load() {
					_, _ = io.WriteString(w, successBody)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			clock := newFakeClock()
			reader := NewResetStatusReader(newRiskClient(upstream, 30*time.Second, clock.Now), 0)
			ctx := context.Background()

			_, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
			require.Error(t, err)
			var classified *Error
			require.ErrorAs(t, err, &classified)
			require.Equal(t, tc.wantKind, classified.Kind)
			require.Equal(t, tc.wantCode, classified.Code)
			require.True(t, IsRiskBlocked(err), "a risk verdict must be recognisable by the caller")
			require.Equal(t, int32(1), hits.Load())

			// Inside the backoff window the risk client refuses the call: the
			// reader reports the block (with the remaining wait) and upstream
			// sees no second request.
			clock.Advance(10 * time.Second)
			_, err = reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
			require.Error(t, err)
			var riskErr *RiskError
			require.ErrorAs(t, err, &riskErr)
			require.Equal(t, RiskErrorKindBackoff, riskErr.Kind)
			require.Equal(t, 50*time.Second, riskErr.RetryAfter)
			require.ErrorAs(t, err, &classified)
			require.Equal(t, ErrorKindRiskControl, classified.Kind)
			require.True(t, IsRiskBlocked(err))
			require.Equal(t, int32(1), hits.Load(), "a blocked read must not reach upstream again")

			// The window (60s after the first failure) plus the pacing interval
			// have elapsed: the reader reads upstream again and parses normally.
			recovering.Store(true)
			clock.Advance(60 * time.Second)
			status, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
			require.NoError(t, err)
			require.Equal(t, []ResetCard{
				{Type: ResetCardTypeFiveHour, ExpireAt: time.UnixMilli(1791279000000).UTC()},
			}, status.FiveHourCards)
			require.Equal(t, int32(2), hits.Load())
		})
	}
}

// TestResetStatusReaderServesCacheWithinTTL covers the TTL cache: a second read
// inside reset_status_cache_minutes is served from memory, the first read after
// the TTL elapses goes upstream again, and the cache is scoped to the
// credential pair (one account never sees another account's cards).
func TestResetStatusReaderServesCacheWithinTTL(t *testing.T) {
	t.Parallel()

	const firstBody = `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{"expire_at":1791279000000}]}}`
	const secondBody = `{"code":0,"msg":"ok","data":{"available_week_resets":[{"expire_at":1791763200000},{"expire_at":1790793900000}]}}`

	var hits atomic.Int32
	var body atomic.Value
	body.Store(firstBody)
	var served atomic.Value
	served.Store(resetStatusTestJWT)
	upstream := newStubUpstream(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == served.Load().(string) {
			_, _ = io.WriteString(w, body.Load().(string))
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[]}}`)
	})

	clock := newFakeClock()
	reader := newResetStatusReader(newRiskClient(upstream, 0, clock.Now), 10*time.Minute, clock.Now)
	ctx := context.Background()

	first, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
	require.NoError(t, err)
	require.Len(t, first.FiveHourCards, 1)
	require.Equal(t, int32(1), hits.Load())

	cached, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
	require.NoError(t, err)
	require.Equal(t, first, cached, "a cache hit must return the parsed status")
	require.Equal(t, int32(1), hits.Load(), "a read inside the TTL must not go upstream")

	clock.Advance(10*time.Minute - time.Second)
	_, err = reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
	require.NoError(t, err)
	require.Equal(t, int32(1), hits.Load(), "the cache is still warm one second before the TTL")

	// TTL elapsed and upstream now reports the week pool: the next read must
	// refetch and return the new data.
	body.Store(secondBody)
	clock.Advance(time.Second)
	refreshed, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
	require.NoError(t, err)
	require.Empty(t, refreshed.FiveHourCards)
	require.Len(t, refreshed.WeekCards, 2)
	require.Equal(t, int32(2), hits.Load(), "a read at the TTL boundary must refetch")

	// Another credential is a different cache entry, even at the same instant.
	other, err := reader.Fetch(ctx, "another-jwt", "another-access")
	require.NoError(t, err)
	require.Empty(t, other.FiveHourCards)
	require.Equal(t, int32(3), hits.Load(), "the cache is keyed by credential, not by endpoint")

	// ... and the second read of that other credential is cached again.
	_, err = reader.Fetch(ctx, "another-jwt", "another-access")
	require.NoError(t, err)
	require.Equal(t, int32(3), hits.Load())
}

// TestResetStatusReaderDoesNotCacheFailures covers the negative side of the
// cache: a failed read is never stored, so the next read retries upstream, and
// a non-positive TTL disables caching entirely.
func TestResetStatusReaderDoesNotCacheFailures(t *testing.T) {
	t.Parallel()

	const successBody = `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{"expire_at":1791279000000}]}}`

	cases := []struct {
		name     string
		cacheTTL time.Duration
		wantHits int32
	}{
		{name: "failures are not cached", cacheTTL: 10 * time.Minute, wantHits: 2},
		{name: "zero TTL disables caching", cacheTTL: 0, wantHits: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			var failing atomic.Bool
			failing.Store(true)
			upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				if failing.Load() {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"code":0,"msg":"boom"}`)
					return
				}
				_, _ = io.WriteString(w, successBody)
			})

			clock := newFakeClock()
			reader := newResetStatusReader(newRiskClient(upstream, 0, clock.Now), tc.cacheTTL, clock.Now)
			ctx := context.Background()

			_, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
			require.Error(t, err)

			failing.Store(false)
			status, err := reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
			require.NoError(t, err)
			require.Len(t, status.FiveHourCards, 1)

			// A third read at the same instant is either served from the cache
			// (one upstream call) or fetched again (caching disabled).
			_, err = reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
			require.NoError(t, err)
			require.Equal(t, tc.wantHits, hits.Load())
		})
	}
}

// TestResetStatusReaderCoalescesConcurrentReadsOfOneCredential covers
// single-flight: N concurrent reads of the same account must produce exactly
// one upstream request and every caller must receive the parsed status.
func TestResetStatusReaderCoalescesConcurrentReadsOfOneCredential(t *testing.T) {
	t.Parallel()

	const callers = 5
	var hits atomic.Int32
	var enteredOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := newStubUpstream(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{"expire_at":1791279000000}]}}`)
	})

	reader := NewResetStatusReader(NewRiskClient(upstream, 0), 10*time.Minute)
	ctx := context.Background()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	statuses := make([]*ResetStatus, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			statuses[i], errs[i] = reader.Fetch(ctx, resetStatusTestJWT, resetStatusTestAccess)
		}(i)
	}
	close(start)

	<-entered
	time.Sleep(50 * time.Millisecond) // let the other callers join the in-flight read
	close(release)
	wg.Wait()

	require.Equal(t, int32(1), hits.Load(), "concurrent reads of one account must coalesce")
	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.NotNil(t, statuses[i])
		require.Len(t, statuses[i].FiveHourCards, 1)
	}
}

// TestResetStatusReaderKeepsCredentialsIsolatedUnderConcurrency covers the
// correctness hazard of the shared risk client: it coalesces calls per endpoint
// key, which is endpoint-wide while a read is credential-scoped. Two accounts
// reading at the same time must produce two upstream requests, and each account
// must receive its own cards - never the other account's.
func TestResetStatusReaderKeepsCredentialsIsolatedUnderConcurrency(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	upstream := newStubUpstream(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Keep the response in flight long enough that a merging caller would
		// definitely join this request instead of issuing its own.
		time.Sleep(50 * time.Millisecond)
		switch r.Header.Get("Authorization") {
		case "jwt-a":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"available_five_hour_resets":[{"expire_at":1791279000000},{"expire_at":1790793900000}]}}`)
		case "jwt-b":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"available_week_resets":[{"expire_at":1791763200000}]}}`)
		default:
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
		}
	})

	reader := NewResetStatusReader(NewRiskClient(upstream, 0), 0)
	ctx := context.Background()

	start := make(chan struct{})
	var wg sync.WaitGroup
	var statusA, statusB *ResetStatus
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		statusA, errA = reader.Fetch(ctx, "jwt-a", "access-a")
	}()
	go func() {
		defer wg.Done()
		<-start
		statusB, errB = reader.Fetch(ctx, "jwt-b", "access-b")
	}()
	close(start)
	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)
	require.Equal(t, int32(2), hits.Load(), "two accounts must produce two upstream reads")
	require.Len(t, statusA.FiveHourCards, 2, "account A must receive its own five-hour cards")
	require.Empty(t, statusA.WeekCards)
	require.Len(t, statusB.WeekCards, 1, "account B must receive its own week cards")
	require.Empty(t, statusB.FiveHourCards)

	// The two upstream requests each carried their own credentials.
	seen := map[string]int{}
	for _, call := range upstream.calls() {
		seen[call.Header.Get("Authorization")]++
		require.Equal(t, ResetStatusEndpoint, call.URL)
	}
	require.Equal(t, map[string]int{"jwt-a": 1, "jwt-b": 1}, seen)
}
