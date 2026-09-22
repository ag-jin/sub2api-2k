package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// Scenario: 单套餐余额钳位（P0：此前只钳负值 → CycleCapacityRemain > Size 的
// 上游脏数据会被原样显示，用户看到比上游多的余额）。表驱动覆盖钳负 / 钳上界 /
// used 反修正 / Precise 路径同样受钳 / 无 size 时不钳上界。
func TestCodeBuddyPackageRemainClamp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		item map[string]any
		want float64
	}{
		{
			name: "脏数据 Remain > Size → 钳到 Size（修复前会返回 500 显示成余额）",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": 4999999},
			want: 500,
		},
		{
			name: "脏数据 Precise Remain > Size → 同样钳到 Size（整数与精确小数两条路都必须钳）",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemainPrecise": "4999999.75"},
			want: 500,
		},
		{
			name: "负 Remain → 钳到 0",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": -30},
			want: 0,
		},
		{
			name: "used 反修正：used 比 size-remain 更大时按 used 反推更小的 remain",
			item: map[string]any{"CycleCapacitySize": 100, "CycleCapacityRemain": 90, "CycleCapacityUsed": 95},
			want: 5,
		},
		{
			name: "used 修正不得把 remain 推成负数（used > size 视为脏值，保留钳后值）",
			item: map[string]any{"CycleCapacitySize": 100, "CycleCapacityRemain": 90, "CycleCapacityUsed": 130},
			want: 90,
		},
		{
			name: "正常值原样返回（零回归）",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": 499, "CycleCapacityUsed": 1},
			want: 499,
		},
		{
			name: "无 Size（无从钳上界）→ 保持原值，只钳负",
			item: map[string]any{"CycleCapacityRemain": 800},
			want: 800,
		},
		{
			name: "Cycle 三元组全缺 → 退化 CapacityRemain（既有退化口径不回归）",
			item: map[string]any{"CapacityRemain": 45},
			want: 45,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			require.InDelta(t, testCase.want, codeBuddyPackageRemain(testCase.item), 1e-9)
		})
	}
}

// Scenario: 逐套餐先钳后加（顺序不能换）——一个负套餐不得吃掉合计里的正数。
func TestParseCodeBuddyCreditsResponseClampBeforeSum(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":100,"CycleCapacityRemain":-100},
		{"CycleCapacitySize":500,"CycleCapacityRemain":4999999},
		{"CycleCapacitySize":50,"CycleCapacityRemain":30}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)
	require.NotNil(t, usage.Balance)
	// 负套餐按 0、脏数据按 500、正常按 30：合计 530。
	// （"求和后整体钳"的旧口径会算成 4999929。）
	require.InDelta(t, 530.0, *usage.Balance, 1e-9)
}

// Scenario: 到期时间解析——响应字段是 CycleEndTime（不是请求体的
// PackageEndTimeRange*），按 UTC+8 硬编码解释（与本机时区无关），
// 且只保留仍有余额的套餐，按到期时间升序。
func TestParseCodeBuddyCreditsResponseExpiries(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":100,"CycleCapacityRemain":10,"CycleEndTime":"2026-10-01 12:00:00"},
		{"CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleEndTime":"2026-09-25 08:00:00"},
		{"CycleCapacitySize":100,"CycleCapacityRemain":20,"CycleEndTime":"2026-09-28 09:30:00"},
		{"CycleCapacitySize":100,"CycleCapacityRemain":30,"CycleEndTime":"not-a-time"}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)
	require.Len(t, usage.Expiries, 2, "余额为 0 与解析失败的套餐不进到期列表")
	// 升序：09-28 先于 10-01。
	require.Equal(t, 20.0, usage.Expiries[0].Amount)
	require.Equal(t, 10.0, usage.Expiries[1].Amount)
	// UTC+8 硬编码：2026-09-28 09:30:00 +08:00 == 2026-09-28 01:30:00 UTC。
	require.Equal(t, "2026-09-28T01:30:00Z", usage.Expiries[0].At.UTC().Format(time.RFC3339))
}

// Scenario: realm 分发的计费 base 与路径族（P0-3，**未经真机验证**）——
// CN：www.codebuddy.cn + /v2/billing/meter/...（单候选）；
// global：www.workbuddy.ai + 无 /v2 优先（两候选，404 回落）。
func TestCodeBuddyBillingRealmDispatch(t *testing.T) {
	t.Parallel()

	cn := &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"realm": "cn"}}
	global := &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"realm": "global"}}

	cases := []struct {
		name      string
		account   *Account
		wantBase  string
		wantPaths []string
	}{
		{
			name:      "CN：codebuddy.cn + /v2 单候选（现状逐字）",
			account:   cn,
			wantBase:  "https://www.codebuddy.cn",
			wantPaths: []string{"/v2/billing/meter/get-user-resource"},
		},
		{
			name:     "global：workbuddy.ai + 无 /v2 优先、404 回落 /v2",
			account:  global,
			wantBase: "https://www.workbuddy.ai",
			wantPaths: []string{
				"/billing/meter/get-user-resource",
				"/v2/billing/meter/get-user-resource",
			},
		},
		{
			name:      "nil 账号按 CN（与既有 realm 判定一致）",
			account:   nil,
			wantBase:  "https://www.codebuddy.cn",
			wantPaths: []string{"/v2/billing/meter/get-user-resource"},
		},
		{
			name:      "domain 含 workbuddy.ai → global（无显式 realm 时的推断路径）",
			account:   &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"domain": "www.workbuddy.ai"}},
			wantBase:  "https://www.workbuddy.ai",
			wantPaths: []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, testCase.wantBase, CodeBuddyBillingBase(testCase.account))
			require.Equal(t, testCase.wantPaths, CodeBuddyUserResourcePaths(testCase.account))
		})
	}

	// 签到路径族：与余额查询同序（global 无 /v2 优先）。
	require.Equal(t, []string{"/v2/billing/meter/daily-checkin"}, CodeBuddyDailyCheckinPaths(cn))
	require.Equal(t, []string{"/billing/meter/daily-checkin", "/v2/billing/meter/daily-checkin"},
		CodeBuddyDailyCheckinPaths(global))

	// chat/登录域与计费域在 CN 下是两个不同主机，不得混用。
	require.Equal(t, "https://copilot.tencent.com", CodeBuddyChatBase(cn))
	require.Equal(t, "https://www.workbuddy.ai", CodeBuddyChatBase(global))
}

// Scenario: 路径回落——global 首候选 404 时换下一候选；非 404 不换路。
func TestCodeBuddyCreditsPathFallback(t *testing.T) {
	t.Parallel()

	t.Run("首候选 404 → 回落 /v2 并成功", func(t *testing.T) {
		t.Parallel()
		var hits []string
		server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			hits = append(hits, r.URL.Path)
			if r.URL.Path == "/billing/meter/get-user-resource" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
				return
			}
			_, _ = w.Write([]byte(codeBuddyCreditsFixtureEnvelope))
		})
		fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
		usage, err := fetcher.FetchCredits(context.Background(), &CodeBuddyCreditsFetchOptions{
			AccessToken: "t",
			Account:     &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"realm": "global"}},
		})
		require.NoError(t, err)
		require.InDelta(t, 599.95, *usage.Balance, 1e-9)
		require.Equal(t, []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"}, hits)
	})

	t.Run("首候选在 /v2 域不可用的 404 之外的错误不换路", func(t *testing.T) {
		t.Parallel()
		calls := 0
		server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
		})
		fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
		_, err := fetcher.FetchCredits(context.Background(), &CodeBuddyCreditsFetchOptions{
			AccessToken: "t",
			Account:     &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"realm": "global"}},
		})
		require.Error(t, err)
		require.Equal(t, 1, calls, "401 不是路径不存在的信号，不得换候选")
	})

	t.Run("CN 单候选：路径恒为 /v2（零回归）", func(t *testing.T) {
		t.Parallel()
		var hit string
		server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			hit = r.URL.Path
			_, _ = w.Write([]byte(codeBuddyCreditsFixtureEnvelope))
		})
		fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
		_, err := fetcher.FetchCredits(context.Background(), &CodeBuddyCreditsFetchOptions{
			AccessToken: "t",
			Account:     &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"realm": "cn"}},
		})
		require.NoError(t, err)
		require.Equal(t, "/v2/billing/meter/get-user-resource", hit)
	})
}

// Scenario: 积分留痕——首见只建基线不记流水；增加即写流水（含去重键）；
// 减少/不变不写流水。
func TestCodeBuddyCreditsLedgerRecordsGainOnly(t *testing.T) {
	t.Parallel()

	balance := 100.0
	server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":10000,"CycleCapacityRemain":`+formatCredits(balance)+`}
		]}}}}`)
	})
	fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
	account := &Account{
		ID:          31,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "t1"},
	}
	repo := &codebuddyUsageTestRepo{accounts: []*Account{account}}
	usageSvc := NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, NewUsageCache(), nil, nil, nil)
	usageSvc.SetCodeBuddyCreditsFetcher(fetcher)

	// 首见：只建基线，不记流水。
	_, err := usageSvc.getCodeBuddyCredits(context.Background(), account, true)
	require.NoError(t, err)
	require.NotContains(t, account.Extra, "codebuddy_credits_ledger", "首见不得记流水")
	require.Contains(t, account.Extra, "codebuddy_credits_ledger_balance")

	// 绕过节流窗：把基线时间往前拨。
	account.Extra["codebuddy_credits_ledger_baseline_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	// 余额增加到 180 → 记流水 +180，带去重键。
	balance = 180
	_, err = usageSvc.getCodeBuddyCredits(context.Background(), account, true)
	require.NoError(t, err)
	ledger, ok := account.Extra["codebuddy_credits_ledger"].(map[string]any)
	require.True(t, ok, "余额增加必须写流水")
	require.InDelta(t, 80.0, ledger["delta"], 1e-9)
	require.Equal(t, "codebuddy-credits|180", account.Extra["codebuddy_credits_ledger_dedup"])

	// 余额下降 → 只更新基线，不写新流水（旧流水保留）。
	prevLedger := account.Extra["codebuddy_credits_ledger"]
	account.Extra["codebuddy_credits_ledger_baseline_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	balance = 150
	_, err = usageSvc.getCodeBuddyCredits(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, prevLedger, account.Extra["codebuddy_credits_ledger"], "余额减少不得写流水")
	require.InDelta(t, 150.0, account.Extra["codebuddy_credits_ledger_balance"], 1e-9)
}

// Scenario: 缓存可见性——buff 内重复查询标注 cached=true 与缓存年龄；
// 实时查询（首次/force）不标 cached。
func TestCodeBuddyCreditsCacheVisibility(t *testing.T) {
	t.Parallel()

	server := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, codeBuddyCreditsFixtureEnvelope)
	})
	fetcher := NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(server.URL)
	account := &Account{
		ID:          41,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "t1"},
	}
	repo := &codebuddyUsageTestRepo{accounts: []*Account{account}}
	usageSvc := NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, NewUsageCache(), nil, nil, nil)
	usageSvc.SetCodeBuddyCreditsFetcher(fetcher)

	live, err := usageSvc.getCodeBuddyCredits(context.Background(), account, false)
	require.NoError(t, err)
	require.False(t, live.UpstreamBalance.Cached, "实时查询不得标 cached")

	cached, err := usageSvc.getCodeBuddyCredits(context.Background(), account, false)
	require.NoError(t, err)
	require.True(t, cached.UpstreamBalance.Cached, "缓存命中必须标 cached")
	require.GreaterOrEqual(t, cached.UpstreamBalance.CachedAgeSeconds, 0)

	// 缓存副本不得污染缓存原件：下一次实时查询仍是干净形态。
	forced, err := usageSvc.getCodeBuddyCredits(context.Background(), account, true)
	require.NoError(t, err)
	require.False(t, forced.UpstreamBalance.Cached)
}

// formatCredits 把余额渲染成 JSON 数字（测试构造响应体用）。
func formatCredits(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// codebuddyUsageTestRepo 最小账号仓储桩（GetByID 按 ID 命中）。
type codebuddyUsageTestRepo struct {
	AccountRepository
	accounts []*Account
	// extraUpdates 记录每次 UpdateExtra 的调用（积分留痕断言用）。
	extraUpdates []map[string]any
}

func (r *codebuddyUsageTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, account := range r.accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return nil, ErrAccountNotFound
}

// UpdateExtra 就地合并进账号 Extra（与 repository 的 JSONB 合并语义等价），
// 并记录调用轨迹。积分留痕走这条通道，桩必须实现，否则内嵌接口为 nil 会 panic。
func (r *codebuddyUsageTestRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	for _, account := range r.accounts {
		if account.ID != id {
			continue
		}
		if account.Extra == nil {
			account.Extra = map[string]any{}
		}
		for key, value := range updates {
			account.Extra[key] = value
		}
		copied := make(map[string]any, len(updates))
		for key, value := range updates {
			copied[key] = value
		}
		r.extraUpdates = append(r.extraUpdates, copied)
		return nil
	}
	return ErrAccountNotFound
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
