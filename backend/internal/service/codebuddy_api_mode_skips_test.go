//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CodeBuddy「API 线路」账号（credentials.api_key 非空）必须跳过腾讯官方域副功能
// （签到 / 积分 / 成长链 / 保活 / 活跃上报 / token 刷新 / 腾讯业务码冷却）：
// 这些路径固定打 copilot.tencent.com 与 www.codebuddy.cn，对中转账号是纯噪声。

// --- 腾讯官方域副功能：API 线路账号一律跳过 ---

// activeAPIModeAccount 构造可调度的 API 线路账号（候选列表类断言用：停调/不可调度
// 会先于 API 线路判定命中跳过原因，掩盖本改动）。
func activeAPIModeAccount(extraCreds map[string]any) *Account {
	acct := apiModeAccount(extraCreds)
	acct.Status = StatusActive
	acct.Schedulable = true
	return acct
}

func TestListCodeBuddyCheckinCandidates_SkipsAPIMode(t *testing.T) {
	api := activeAPIModeAccount(nil)
	oauth := &Account{ID: 802, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "at", "uid": "u1"}}
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api, oauth), nil)

	candidates, err := svc.ListCodeBuddyCheckinCandidates(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, candidates, 2)

	byID := map[int64]CodeBuddyCheckinCandidate{}
	for _, candidate := range candidates {
		byID[candidate.AccountID] = candidate
	}
	assert.Contains(t, byID[api.ID].SkipReason, "API 线路", "API 线路账号不得进腾讯签到请求")
	assert.Empty(t, byID[oauth.ID].SkipReason, "腾讯 auth JSON 账号回归：仍在签到候选内")
}

func TestCodeBuddyCheckin_RejectsAPIModeBeforeUpstream(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit":1,"streak_days":1}}`))
	}))
	defer srv.Close()

	api := apiModeAccount(map[string]any{"access_token": "looks-like-tencent", "uid": "u1"})
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api), nil).WithTestBaseURL(srv.URL)

	_, err := svc.Checkin(context.Background(), api.ID)
	require.Error(t, err, "手动签到必须对 API 线路账号给出明确错误")
	assert.Contains(t, err.Error(), "API 线路")
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "API 线路账号不得向腾讯计费域发签到请求")
}

func TestCodeBuddyZeroCreditProbeTarget_ExcludesAPIMode(t *testing.T) {
	api := apiModeAccount(map[string]any{"access_token": "at", "uid": "u1"})
	api.Status = StatusActive
	assert.False(t, codeBuddyZeroCreditProbeTarget(api), "API 线路账号不得被腾讯积分探针（含 upstream_balance 钩子）命中")

	oauth := &Account{ID: 802, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"access_token": "at", "uid": "u1"}}
	assert.True(t, codeBuddyZeroCreditProbeTarget(oauth), "腾讯 auth JSON 账号回归：仍在探测目标内")
}

func TestAccountUsageService_CodeBuddyAPIModeHasNoCreditsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	api := apiModeAccount(map[string]any{"access_token": "at"})
	repo := &codebuddyUsageTestRepo{accounts: []*Account{api}}
	svc := NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, NewUsageCache(), nil, nil, nil)
	svc.SetCodeBuddyCreditsFetcher(NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(srv.URL))

	_, err := svc.GetUsage(context.Background(), api.ID)
	require.Error(t, err, "API 线路账号不查腾讯积分余额")
	assert.Contains(t, err.Error(), "USAGE_UNSUPPORTED")
}

func TestListCodeBuddyActivityCandidates_SkipsAPIMode(t *testing.T) {
	api := activeAPIModeAccount(map[string]any{"access_token": "at", "uid": "u1"})
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api), nil)

	candidates, err := svc.ListCodeBuddyActivityCandidates(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Contains(t, candidates[0].SkipReason, "API 线路")
}

func TestReportCodeBuddyActivity_RejectsAPIModeBeforeUpstream(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	api := apiModeAccount(map[string]any{"access_token": "at", "uid": "u1"})
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api), nil).WithTestBaseURL(srv.URL)

	res := svc.ReportCodeBuddyActivity(context.Background(), api, 1)
	require.Error(t, res.Err, "API 线路账号不做腾讯活跃上报")
	assert.Contains(t, res.Err.Error(), "API 线路")
	assert.Equal(t, 0, res.Reported)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "不得向腾讯域发活跃上报")
}

func TestListCodeBuddyGrowthCandidates_SkipsAPIMode(t *testing.T) {
	api := activeAPIModeAccount(map[string]any{"access_token": "at", "uid": "u1"})
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api), nil)

	candidates, err := svc.ListCodeBuddyGrowthCandidates(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Contains(t, candidates[0].SkipReason, "API 线路")
}

func TestRunCodeBuddyGrowthChannels_RejectsAPIMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	SetCodeBuddyGrowthTestBase(srv.URL)
	defer SetCodeBuddyGrowthTestBase("")

	api := apiModeAccount(map[string]any{"access_token": "at", "uid": "u1"})
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api), nil)

	summary := svc.RunCodeBuddyGrowthChannels(context.Background(), api, "2026-10-11",
		[]string{codebuddy.CodeBuddyGrowthChannelStreak})
	assert.Contains(t, summary.Error, "API 线路")
}

func TestRunCodeBuddyGrowthChannelNow_RejectsAPIMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	SetCodeBuddyGrowthTestBase(srv.URL)
	defer SetCodeBuddyGrowthTestBase("")

	api := apiModeAccount(map[string]any{"access_token": "at", "uid": "u1"})
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(api), nil).WithTestBaseURL(srv.URL)

	res := svc.RunCodeBuddyGrowthChannelNow(context.Background(), api, codebuddy.CodeBuddyGrowthChannelAdopt)
	assert.Contains(t, res.Error, "API 线路", "手动执行单通道同样必须拒绝 API 线路账号")
}

func TestCodeBuddyTokenRefresher_APIModeNeverRefreshes(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	api := apiModeAccount(map[string]any{"access_token": "at", "refresh_token": "rt", "expires_at": expired})
	refresher := &CodeBuddyTokenRefresher{}

	assert.False(t, refresher.CanRefresh(api), "API 线路账号无条件跳过 token 刷新")
	assert.False(t, refresher.NeedsRefresh(api, time.Hour), "API 线路账号不需要（也不得）刷新")
}

func TestCodeBuddyKeepalive_SkipsAPIMode(t *testing.T) {
	api := apiModeAccount(map[string]any{"access_token": "at", "refresh_token": "rt"})
	api.Status = StatusActive
	repo := &keepaliveRepoStub{accts: []Account{*api}}
	keepalive := NewCodeBuddyTokenKeepalive(repo, nil, &CodeBuddyTokenRefresher{})

	summary, err := keepalive.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
	require.NoError(t, err)
	assert.Equal(t, 0, summary.Refreshed, "API 线路账号不得进入腾讯 token 刷新")
	assert.Equal(t, 1, summary.Skipped)
}

func TestCodeBuddyAccountCooldown_SkipsAPIMode(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := apiModeAccount(nil)

	_, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusPaymentRequired, nil, time.Now())
	assert.False(t, ok, "API 线路账号不套用腾讯计费（402）冷却")
	assert.False(t, repo.called)

	_, ok = rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusForbidden,
		[]byte(`{"code":14018,"msg":"账号积分耗尽"}`), time.Now())
	assert.False(t, ok, "API 线路账号不套用腾讯业务码 14018 冷却")
	assert.False(t, repo.called)
}

func TestApplyCodeBuddyBizCodeCooldown_SkipsAPIMode(t *testing.T) {
	repo := &cooldownRepoStub{}
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	acct := apiModeAccount(nil)
	handled := svc.ApplyCodeBuddyBizCodeCooldown(context.Background(), acct, CodeBuddyCreditsExhaustedBizCode, "deepseek-v4.1-flash")
	assert.False(t, handled, "API 线路账号不套用腾讯业务码冷却")
	assert.False(t, repo.called)
}
