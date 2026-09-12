package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const codeBuddyCreditsFixtureEnvelope = `{"code":0,"msg":"OK","requestId":"r1","data":{"Response":{"Data":{"TotalDosage":600,"Accounts":[
  {"CycleCapacityRemainPrecise":"499.95","CapacityRemainPrecise":"500","CycleCapacityRemain":499,"CycleCapacitySize":500,"CapacityRemain":500},
  {"CycleCapacityRemainPrecise":"100","CycleCapacityRemain":100,"CapacityRemain":100}
]}}}}`

func codeBuddyCreditsTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// Scenario: Precise 字符串字段求和（实证口径，499.95 + 100 = 599.95）。
func TestParseCodeBuddyCreditsResponsePreciseSum(t *testing.T) {
	t.Parallel()

	usage, err := parseCodeBuddyCreditsResponse([]byte(codeBuddyCreditsFixtureEnvelope))
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.InDelta(t, 599.95, *usage.Balance, 1e-9)
	require.Equal(t, "credits", usage.Unit)
	require.Equal(t, "ok", usage.Status)
}

// Scenario: 整数字段退化（无 Precise 键 → CycleCapacityRemain；Cycle 全零 →
// CapacityRemain，照抄参考实现 _package_remain 退化口径）。
func TestParseCodeBuddyCreditsResponseIntegerFallback(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacityRemain":123,"CycleCapacitySize":200},
		{"CapacityRemain":45}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	require.InDelta(t, 168.0, *usage.Balance, 1e-9)
}

// Scenario: 非 0 业务码 → 错误信封（含业务码），HTTP 200 也按信封判。
func TestParseCodeBuddyCreditsResponseEnvelopeError(t *testing.T) {
	t.Parallel()

	_, err := parseCodeBuddyCreditsResponse([]byte(`{"code":12153,"msg":"session expired"}`))
	require.Error(t, err)
	creditsErr, ok := err.(*CodeBuddyCreditsError)
	require.True(t, ok)
	require.Equal(t, "upstream_error", creditsErr.Code)
	require.Equal(t, 12153, creditsErr.BizCode)
}

// Scenario: 出站请求形态（A2/A6）——固定 headers、Bearer、无身份头、恒用
// 计费常量 base（不注入 X-User-Id/X-Enterprise-Id/X-Tenant-Id/X-Domain）；
// 请求体为实证固定形态。HTTP 401 → unauthenticated 错误分类。
func TestCodeBuddyCreditsFetcherHeadersAnd401(t *testing.T) {
	t.Parallel()

	var gotAuth, gotUA, gotIdentity []string
	var body map[string]any
	server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v2/billing/meter/get-user-resource", r.URL.Path)
		gotAuth = r.Header.Values("Authorization")
		gotUA = r.Header.Values("User-Agent")
		for _, key := range []string{"X-User-Id", "X-Enterprise-Id", "X-Tenant-Id", "X-Domain"} {
			gotIdentity = append(gotIdentity, r.Header.Get(key))
		}
		require.NotEmpty(t, r.Header.Get("X-Requested-With"))
		require.NotEmpty(t, r.Header.Get("Origin"))
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
	})

	fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
	_, err := fetcher.FetchCredits(context.Background(), &CodeBuddyCreditsFetchOptions{
		AccessToken: "token-for-test",
		AccountID:   9,
	})
	require.Error(t, err)
	creditsErr, ok := err.(*CodeBuddyCreditsError)
	require.True(t, ok)
	require.Equal(t, "unauthenticated", creditsErr.Code)
	require.Equal(t, http.StatusUnauthorized, creditsErr.HTTPStatus)

	require.Equal(t, []string{"Bearer token-for-test"}, gotAuth)
	require.Equal(t, TENCENT_CODEBUDDY_USER_AGENT, gotUA[0])
	for _, value := range gotIdentity {
		require.Empty(t, value, "计费接口不得注入身份头")
	}

	// 请求体固定形态（实证 V1）。
	require.Equal(t, float64(1), body["PageNumber"])
	require.Equal(t, "p_tcaca", body["ProductCode"])
	status, ok := body["Status"].([]any)
	require.True(t, ok)
	require.Len(t, status, 2)
	require.Equal(t, float64(0), status[0])
	require.NotEmpty(t, body["PackageEndTimeRangeBegin"])
	require.NotEmpty(t, body["PackageEndTimeRangeEnd"])
}

// Scenario: 成功解析走 testBaseURL（httptest），返回 UpstreamBalanceUsage
// balance 求和与冻结键（upstream_balance 同构）。
func TestCodeBuddyCreditsFetcherSuccess(t *testing.T) {
	t.Parallel()

	server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(codeBuddyCreditsFixtureEnvelope))
	})
	fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
	usage, err := fetcher.FetchCredits(context.Background(), &CodeBuddyCreditsFetchOptions{AccessToken: "t"})
	require.NoError(t, err)
	require.InDelta(t, 599.95, *usage.Balance, 1e-9)
}

// Scenario: getCodeBuddyCredits —— 成功缓存 3min（force 刷新可穿透）；错误负缓存
// 1min（force 也命中，防重试风暴）；失败路径走值通道 stale/lastSuccess，
// 绝不返回 error 到调用方（不改账号状态）。
func TestGetCodeBuddyCreditsCacheAndDegraded(t *testing.T) {
	t.Parallel()

	calls := 0
	server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(codeBuddyCreditsFixtureEnvelope))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401}`))
	})
	fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
	repo := &codebuddyUsageTestRepo{
		accounts: []*Account{{
			ID:          21,
			Platform:    PlatformCodeBuddy,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{"access_token": "t1", "uid": "u21"},
		}},
	}
	usageSvc := NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, NewUsageCache(), nil, nil, nil)
	usageSvc.SetCodeBuddyCreditsFetcher(fetcher)
	account := repo.accounts[0]

	// 第一次：成功 + 记 lastSuccess。
	usage, err := usageSvc.getCodeBuddyCredits(context.Background(), account, false)
	require.NoError(t, err)
	require.InDelta(t, 599.95, *usage.UpstreamBalance.Balance, 1e-9)
	require.Equal(t, 1, calls)

	// 3min 缓存内二次调用不再打上游。
	cached, err := usageSvc.getCodeBuddyCredits(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.InDelta(t, 599.95, *cached.UpstreamBalance.Balance, 1e-9)

	// force：上游 401 → 值通道 stale + lastSuccess（上一轮成功余额）。
	degraded, err := usageSvc.getCodeBuddyCredits(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	degradedBalance := usage.UpstreamBalance.Balance
	require.NotNil(t, degradedBalance)
	require.True(t, degraded.UpstreamBalance.Stale)
	require.Equal(t, "stale", degraded.UpstreamBalance.Status)
	require.Equal(t, "unauthenticated", degraded.UpstreamBalance.Error)
	require.Equal(t, "unauthenticated", degraded.ErrorCode)
	require.True(t, degraded.NeedsReauth)

	// 负缓存 1min 内（force 也命中）。
	_, err = usageSvc.getCodeBuddyCredits(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "负缓存 TTL 内不得重复打上游")

	_ = time.Now
}

// codebuddyUsageTestRepo 最小账号仓储桩（GetByID 按 ID 命中）。
type codebuddyUsageTestRepo struct {
	AccountRepository
	accounts []*Account
}

func (r *codebuddyUsageTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, account := range r.accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return nil, ErrAccountNotFound
}

func (r *codebuddyUsageTestRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	out := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, *account)
		}
	}
	return out, nil
}
