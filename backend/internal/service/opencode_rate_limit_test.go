package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	opencodepkg "github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/stretchr/testify/require"
)

func openCodeTestResetAt(t *testing.T, seconds time.Duration) *time.Time {
	t.Helper()
	resetAt := time.Now().Add(seconds)
	return &resetAt
}

type openCodeRuntimeBlockRecorder struct {
	account *Account
	until   time.Time
	reason  string
}

func (r *openCodeRuntimeBlockRecorder) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	r.account = account
	r.until = until
	r.reason = reason
}

func (r *openCodeRuntimeBlockRecorder) ClearAccountSchedulingBlock(int64) {}

func TestOpenCodeRateLimitResetAt_SelectsExhaustedWindow(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *opencodepkg.UsageSnapshot
		want     time.Duration
	}{
		{
			name: "rolling rate limited",
			snapshot: &opencodepkg.UsageSnapshot{
				Rolling: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited, ResetsAt: openCodeTestResetAt(t, time.Hour)},
			},
			want: time.Hour,
		},
		{
			name: "weekly at one hundred percent",
			snapshot: &opencodepkg.UsageSnapshot{
				Weekly: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusOK, Percent: 100, ResetsAt: openCodeTestResetAt(t, 48*time.Hour)},
			},
			want: 48 * time.Hour,
		},
		{
			name: "monthly rate limited",
			snapshot: &opencodepkg.UsageSnapshot{
				Monthly: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited, ResetsAt: openCodeTestResetAt(t, 30*24*time.Hour)},
			},
			want: 30 * 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			resetAt, limited := openCodeRateLimitResetAt(&Account{ID: 1}, tt.snapshot, now)
			require.True(t, limited)
			require.WithinDuration(t, now.Add(tt.want), resetAt, time.Second)
		})
	}
}

func TestOpenCodeRateLimitResetAt_UsesLatestWindowAndNeverShortensExistingLimit(t *testing.T) {
	now := time.Now()
	weeklyReset := now.Add(3 * 24 * time.Hour)
	monthlyReset := now.Add(20 * 24 * time.Hour)
	existingReset := now.Add(30 * 24 * time.Hour)

	resetAt, limited := openCodeRateLimitResetAt(&Account{ID: 1}, &opencodepkg.UsageSnapshot{
		Rolling: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited, ResetsAt: openCodeTestResetAt(t, time.Hour)},
		Weekly:  opencodepkg.UsageWindow{Percent: 100, ResetsAt: &weeklyReset},
		Monthly: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited, ResetsAt: &monthlyReset},
	}, now)
	require.True(t, limited)
	require.Equal(t, monthlyReset, resetAt)

	resetAt, limited = openCodeRateLimitResetAt(&Account{ID: 1, RateLimitResetAt: &existingReset}, &opencodepkg.UsageSnapshot{
		Monthly: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited, ResetsAt: &monthlyReset},
	}, now)
	require.True(t, limited)
	require.Equal(t, existingReset, resetAt)
}

func TestOpenCodeRateLimitResetAt_RejectsMissingOrExpiredReset(t *testing.T) {
	now := time.Now()
	expired := now.Add(-time.Minute)
	resetAt, limited := openCodeRateLimitResetAt(&Account{ID: 1}, &opencodepkg.UsageSnapshot{
		Rolling: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited},
		Weekly:  opencodepkg.UsageWindow{Percent: 100, ResetsAt: &expired},
	}, now)
	require.False(t, limited)
	require.True(t, resetAt.IsZero())
}

func TestHandle429_OpenCodeUsesUsageWindowReset(t *testing.T) {
	resetAt := time.Now().Add(72 * time.Hour).UTC()
	upstream := &opencodeUsageTestUpstream{body: `{"usage":{"rolling":{"status":"ok","percent":2},"weekly":{"status":"rate-limited","percent":100,"resetsAt":"` + resetAt.Format(time.RFC3339) + `"},"monthly":{"status":"ok","percent":3}}}`}
	repo := &opencodeUsageTestAccountRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.SetOpenCodeUsageFetcher(NewOpenCodeUsageFetcher(upstream))
	blocker := &openCodeRuntimeBlockRecorder{}
	svc.SetAccountRuntimeBlocker(blocker)
	account := openCodeUsageTestAccount(31)
	proxyID := int64(7)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{Protocol: "http", Host: "proxy.internal", Port: 8080}
	account.Concurrency = 4
	account.Credentials["base_url"] = "https://opencode.ai/zen/go/v1"
	account.Credentials[credKeyHeaderOverrideEnabled] = true
	account.Credentials[credKeyHeaderOverrides] = map[string]any{"x-runtime-route": "primary"}

	svc.handle429(context.Background(), account, http.Header{}, nil)

	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, account.ID, repo.lastRateLimitID)
	require.WithinDuration(t, resetAt, repo.lastRateLimitReset, time.Second)
	require.Equal(t, "https://opencode.ai/zen/go/v1/usage", upstream.lastURL)
	require.Equal(t, "Bearer sk-test-placeholder", upstream.lastAuth)
	require.Equal(t, "http://proxy.internal:8080", upstream.lastProxyURL)
	require.Equal(t, account.ID, upstream.lastAccountID)
	require.Equal(t, account.Concurrency, upstream.lastConcurrency)
	require.Equal(t, []string{"primary"}, upstream.lastHeader["X-Runtime-Route"])
	require.Same(t, account, blocker.account)
	require.WithinDuration(t, resetAt, blocker.until, time.Second)
	require.Equal(t, "opencode_usage", blocker.reason)
}

func TestHandle429_OpenCodeFallsBackWhenUsageUnavailableOrNotExhausted(t *testing.T) {
	tests := []struct {
		name     string
		upstream *opencodeUsageTestUpstream
	}{
		{name: "usage unavailable", upstream: &opencodeUsageTestUpstream{status: http.StatusServiceUnavailable}},
		{name: "usage not exhausted", upstream: &opencodeUsageTestUpstream{body: `{"usage":{"rolling":{"status":"ok","percent":2},"weekly":{"status":"ok","percent":3},"monthly":{"status":"ok","percent":4}}}`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &opencodeUsageTestAccountRepo{}
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			svc.SetOpenCodeUsageFetcher(NewOpenCodeUsageFetcher(tt.upstream))
			before := time.Now()
			svc.handle429(context.Background(), openCodeUsageTestAccount(32), http.Header{}, nil)
			after := time.Now()

			require.Equal(t, 1, repo.rateLimitCalls)
			require.True(t, !repo.lastRateLimitReset.Before(before.Add(5*time.Second)) && !repo.lastRateLimitReset.After(after.Add(5*time.Second)))
		})
	}
}

func TestHandle429_OpenCodeFallbackDoesNotShortenExistingLimit(t *testing.T) {
	existingReset := time.Now().Add(24 * time.Hour)
	repo := &opencodeUsageTestAccountRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.SetOpenCodeUsageFetcher(NewOpenCodeUsageFetcher(&opencodeUsageTestUpstream{status: http.StatusServiceUnavailable}))
	account := openCodeUsageTestAccount(34)
	account.RateLimitResetAt = &existingReset

	svc.handle429(context.Background(), account, http.Header{}, nil)

	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, existingReset, repo.lastRateLimitReset)
}

func TestHandle429_OpenCodeFallsBackPromptlyWhenUsageProbeStalls(t *testing.T) {
	repo := &opencodeUsageTestAccountRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.SetOpenCodeUsageFetcher(NewOpenCodeUsageFetcher(&opencodeUsageTestUpstream{wait: true}))

	started := time.Now()
	svc.handle429(context.Background(), openCodeUsageTestAccount(35), http.Header{}, nil)
	elapsed := time.Since(started)

	require.Less(t, elapsed, 2*time.Second)
	require.Equal(t, 1, repo.rateLimitCalls)
}

func TestAccountUsageService_OpenCodeSnapshotPersistsRateLimit(t *testing.T) {
	resetAt := time.Now().Add(14 * 24 * time.Hour).UTC()
	upstream := &opencodeUsageTestUpstream{body: `{"usage":{"rolling":{"status":"ok","percent":2},"weekly":{"status":"ok","percent":3},"monthly":{"status":"rate-limited","percent":100,"resetsAt":"` + resetAt.Format(time.RFC3339) + `"}}}`}
	account := openCodeUsageTestAccount(33)
	svc := newOpenCodeUsageTestService(upstream, account)
	repo, ok := svc.accountRepo.(*opencodeUsageTestAccountRepo)
	require.True(t, ok)
	blocker := &openCodeRuntimeBlockRecorder{}
	svc.SetAccountRuntimeBlocker(blocker)

	usage, err := svc.getOpenCodeUsage(context.Background(), account, true)

	require.NoError(t, err)
	require.NotNil(t, usage.Opencode)
	require.Equal(t, 1, repo.rateLimitCalls)
	require.WithinDuration(t, resetAt, repo.lastRateLimitReset, time.Second)
	require.Same(t, account, blocker.account)
	require.WithinDuration(t, resetAt, blocker.until, time.Second)
	require.Equal(t, "opencode_usage", blocker.reason)
}
