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

// Scenario: 单套餐余额钳位（P0：此前只钳负值 → Remain > Size 的上游脏数据会被
// 原样显示，用户看到比上游多的余额）。
//
// 本函数是**多级退化**结构（出口 1 = Cycle 域、出口 2 = Capacity 兜底），每条出口
// 都必须钳位——A1 的教训（LESSONS.md L1）同款：只改主路径会漏掉用户实际走的那条。
// 故本表按**出口分组**，每组都含"脏数据被钳住"的用例。
func TestCodeBuddyPackageRemainClamp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		item map[string]any
		want float64
		// exit 标注该用例打在哪个出口上，断言失败时能直接看出漏了哪条。
		exit string
	}{
		// ---- 出口 1：Cycle 域（整数形态） ----
		{
			name: "Cycle 整数：脏数据 Remain > Size → 钳到 Size",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": 4999999},
			want: 500,
			exit: "出口1-Cycle-整数",
		},
		{
			name: "Cycle 整数：负 Remain → 钳到 0",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": -30},
			want: 0,
			exit: "出口1-Cycle-整数",
		},
		{
			name: "Cycle 整数：正常值原样返回（零回归）",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": 499, "CycleCapacityUsed": 1},
			want: 499,
			exit: "出口1-Cycle-整数",
		},
		{
			name: "Cycle 整数：used 反修正——used 比 size-remain 更大时按 used 反推更小的 remain",
			item: map[string]any{"CycleCapacitySize": 100, "CycleCapacityRemain": 90, "CycleCapacityUsed": 95},
			want: 5,
			exit: "出口1-Cycle-整数",
		},
		{
			name: "Cycle 整数：used > size（used 侧脏值）时不得把 remain 推成负数，保留钳后值",
			item: map[string]any{"CycleCapacitySize": 100, "CycleCapacityRemain": 90, "CycleCapacityUsed": 130},
			want: 90,
			exit: "出口1-Cycle-整数",
		},
		// ---- 出口 1：Cycle 域（Precise 精确小数形态） ----
		{
			name: "Cycle Precise：脏数据 Remain > Size → 同样钳到 Size（精确小数路径也必须钳）",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemainPrecise": "4999999.75"},
			want: 500,
			exit: "出口1-Cycle-Precise",
		},
		{
			name: "Cycle Precise：负值 → 钳到 0",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemainPrecise": "-12.5"},
			want: 0,
			exit: "出口1-Cycle-Precise",
		},
		{
			name: "Cycle Precise：正常小数原样保留（精度不回归）",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemainPrecise": "499.95"},
			want: 499.95,
			exit: "出口1-Cycle-Precise",
		},
		{
			name: "Cycle Precise：无法解析时退化整数 CycleCapacityRemain 并同样受钳",
			item: map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemainPrecise": "abc", "CycleCapacityRemain": 4999999},
			want: 500,
			exit: "出口1-Cycle-Precise退化",
		},
		// ---- 出口 1：无 Size（无从钳上界） ----
		{
			name: "Cycle 无 Size：不设上界，只钳负（上游同款语义）",
			item: map[string]any{"CycleCapacityRemain": 800},
			want: 800,
			exit: "出口1-无Size",
		},
		{
			name: "Cycle 无 Size 且负值 → 仍钳到 0",
			item: map[string]any{"CycleCapacityRemain": -5},
			want: 0,
			exit: "出口1-无Size",
		},
		// ---- 出口 2：Cycle 全缺 → Capacity 兜底 ----
		{
			name: "Capacity 兜底：脏数据 CapacityRemain > CapacitySize → 必须钳到 Size（修复前原样返回 4999999）",
			item: map[string]any{"CapacitySize": 500, "CapacityRemain": 4999999},
			want: 500,
			exit: "出口2-Capacity兜底",
		},
		{
			name: "Capacity 兜底：负 Remain → 钳到 0",
			item: map[string]any{"CapacitySize": 500, "CapacityRemain": -80},
			want: 0,
			exit: "出口2-Capacity兜底",
		},
		{
			name: "Capacity 兜底：无 Size → 不设上界（既有退化口径不回归）",
			item: map[string]any{"CapacityRemain": 45},
			want: 45,
			exit: "出口2-Capacity兜底",
		},
		{
			name: "Capacity 兜底：正常值原样返回",
			item: map[string]any{"CapacitySize": 500, "CapacityRemain": 45},
			want: 45,
			exit: "出口2-Capacity兜底",
		},
		// ---- 分支优先级 ----
		{
			name: "有 Cycle 字段时不走 Capacity 兜底（即使 Capacity 域的值更大）",
			item: map[string]any{
				"CycleCapacitySize": 500, "CycleCapacityRemain": 300,
				"CapacitySize": 999999, "CapacityRemain": 999999,
			},
			want: 300,
			exit: "出口1-优先级",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			require.InDelta(t, testCase.want, codeBuddyPackageRemain(testCase.item), 1e-9,
				"出口=%s", testCase.exit)
		})
	}
}

// Scenario: 两条退化出口的钳位必须**成对存在**——同一份脏数据（Remain > Size）
// 在 Cycle 形态与 Capacity 形态下都要被钳住，不能只修一条。
// 这条用例是"只改主路径"这类回归的守门人（A1 LESSONS.md L1 同款教训）。
func TestCodeBuddyPackageRemainClampCoversBothFallbacks(t *testing.T) {
	t.Parallel()

	cycleDirty := map[string]any{"CycleCapacitySize": 500, "CycleCapacityRemain": 4999999}
	capacityDirty := map[string]any{"CapacitySize": 500, "CapacityRemain": 4999999}

	require.InDelta(t, 500.0, codeBuddyPackageRemain(cycleDirty), 1e-9,
		"出口1（Cycle）未钳住脏数据")
	require.InDelta(t, 500.0, codeBuddyPackageRemain(capacityDirty), 1e-9,
		"出口2（Capacity 兜底）未钳住脏数据——这正是参考实现漏掉的那条")

	// 严格小于脏输入，证明不是"恰好把输入返回了"。
	require.Less(t, codeBuddyPackageRemain(cycleDirty), 4999999.0)
	require.Less(t, codeBuddyPackageRemain(capacityDirty), 4999999.0)
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

// Scenario: 付费订阅识别（"有购买订阅才显示到期时间"）——响应里同时有体验版
// (freeMon)、活动包、proMon 订阅包时，Subscription 命中 proMon；Name 透传上游
// PackageName；到期取 CycleEndTime（同一 UTC+8 墙钟口径）；AutoRenewFlag=1 →
// AutoRenew=true。体验版/活动包不触发显示。
func TestCodeBuddySubscriptionPaidPlan(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_008_cfWoLwvjU4","PackageName":"CodeBuddy个人体验版","CycleCapacitySize":500,"CycleCapacityRemain":499,"CycleEndTime":"2026-09-30 23:59:59"},
		{"PackageCode":"TCACA_code_007_nzdH5h4Nl0","PackageName":"CodeBuddy活动赠送包","CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleEndTime":"2026-09-20 00:00:00"},
		{"PackageCode":"TCACA_code_002_AkiJS3ZHF5","PackageName":"CodeBuddy专业版","CycleCapacitySize":1000,"CycleCapacityRemain":800,"CycleEndTime":"2026-10-09 12:34:56","AutoRenewFlag":1}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)

	require.NotNil(t, usage.Subscription, "有付费订阅必须产出 Subscription")
	require.Equal(t, "TCACA_code_002_AkiJS3ZHF5", usage.Subscription.PackageCode)
	require.Equal(t, "CodeBuddy专业版", usage.Subscription.Name)
	require.True(t, usage.Subscription.AutoRenew)
	// UTC+8 墙钟解释：2026-10-09 12:34:56 +08:00 == 2026-10-09 04:34:56 UTC。
	require.Equal(t, "2026-10-09T04:34:56Z", usage.Subscription.ExpiresAt.UTC().Format(time.RFC3339))
}

// Scenario: 订阅到期时刻取官方链——DeductionEndTime → ExpiredTime → CycleEndTime
// 第一个**可解析**值（官方客户端 `endTime = isDaily ? CycleEndTime : DeductionEndTime`；
// CycleEndTime 是周期刷新的口径，不是订阅本身的到期时刻，只在其他字段缺失时兜底）。
// 上游字段名是这三个，与请求体 PackageEndTimeRange* 无关。
func TestCodeBuddySubscriptionExpiryChain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		item map[string]any
		want string // UTC RFC3339
	}{
		{
			name: "三字段齐备 → DeductionEndTime 胜出",
			item: map[string]any{
				"PackageCode":      "TCACA_code_003_FAnt7lcmRT",
				"DeductionEndTime": "2027-03-15 08:00:00",
				"ExpiredTime":      "2026-12-31 08:00:00",
				"CycleEndTime":     "2026-10-31 23:59:59",
			},
			want: "2027-03-15T00:00:00Z",
		},
		{
			name: "DeductionEndTime 缺失 → ExpiredTime（先于 CycleEndTime）",
			item: map[string]any{
				"PackageCode":  "TCACA_code_003_FAnt7lcmRT",
				"ExpiredTime":  "2026-12-31 08:00:00",
				"CycleEndTime": "2026-10-31 23:59:59",
			},
			want: "2026-12-31T00:00:00Z",
		},
		{
			name: "只有 CycleEndTime → 兜底取它",
			item: map[string]any{
				"PackageCode":  "TCACA_code_003_FAnt7lcmRT",
				"CycleEndTime": "2026-10-31 23:59:59",
			},
			want: "2026-10-31T15:59:59Z",
		},
		{
			name: "首字段在场但不可解析 → 退下一个可解析字段（不是「有键就用」）",
			item: map[string]any{
				"PackageCode":      "TCACA_code_003_FAnt7lcmRT",
				"DeductionEndTime": "not-a-time",
				"CycleEndTime":     "2026-10-31 23:59:59",
			},
			want: "2026-10-31T15:59:59Z",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			subscription := codeBuddySubscription([]map[string]any{testCase.item})
			require.NotNil(t, subscription, "付费套餐必须产出订阅")
			require.Equal(t, testCase.want, subscription.ExpiresAt.UTC().Format(time.RFC3339))
		})
	}
}

// Scenario: 多档付费订阅并存时取**优先级最高**的一档（官方
// `activePlan = flagship || advanced || youth || proPlan`，与列表顺序无关）。
func TestCodeBuddySubscriptionPriority(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		accounts []map[string]any
		wantCode string
		wantName string
	}{
		{
			name: "旗舰版 vs 专业版月付（低档在前）→ 旗舰版",
			accounts: []map[string]any{
				{"PackageCode": "TCACA_code_002_AkiJS3ZHF5", "PackageName": "CodeBuddy专业版", "CycleEndTime": "2026-11-01 08:00:00"},
				{"PackageCode": "TCACA_code_027_0FCGVA6vSa", "PackageName": "CodeBuddy旗舰版", "CycleEndTime": "2026-12-01 08:00:00"},
			},
			wantCode: "TCACA_code_027_0FCGVA6vSa",
			wantName: "CodeBuddy旗舰版",
		},
		{
			name: "进阶版 vs 青春版 → 进阶版",
			accounts: []map[string]any{
				{"PackageCode": "TCACA_code_023_4xbGhMrE6q", "PackageName": "CodeBuddy青春版", "CycleEndTime": "2026-11-01 08:00:00"},
				{"PackageCode": "TCACA_code_026_BaESVICNoi", "PackageName": "CodeBuddy进阶版", "CycleEndTime": "2026-12-01 08:00:00"},
			},
			wantCode: "TCACA_code_026_BaESVICNoi",
			wantName: "CodeBuddy进阶版",
		},
		{
			name: "青春版 vs 专业版月付 → 青春版",
			accounts: []map[string]any{
				{"PackageCode": "TCACA_code_002_AkiJS3ZHF5", "PackageName": "CodeBuddy专业版", "CycleEndTime": "2026-11-01 08:00:00"},
				{"PackageCode": "TCACA_code_023_4xbGhMrE6q", "PackageName": "CodeBuddy青春版", "CycleEndTime": "2026-12-01 08:00:00"},
			},
			wantCode: "TCACA_code_023_4xbGhMrE6q",
			wantName: "CodeBuddy青春版",
		},
		{
			name: "月付 vs 月付Plus（年付不在场）→ 月付",
			accounts: []map[string]any{
				{"PackageCode": "TCACA_code_005_maRGyrHhw1", "PackageName": "CodeBuddy专业版Plus", "CycleEndTime": "2026-11-01 08:00:00"},
				{"PackageCode": "TCACA_code_002_AkiJS3ZHF5", "PackageName": "CodeBuddy专业版", "CycleEndTime": "2026-12-01 08:00:00"},
			},
			wantCode: "TCACA_code_002_AkiJS3ZHF5",
			wantName: "CodeBuddy专业版",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			subscription := codeBuddySubscription(testCase.accounts)
			require.NotNil(t, subscription)
			require.Equal(t, testCase.wantCode, subscription.PackageCode)
			require.Equal(t, testCase.wantName, subscription.Name)
		})
	}
}

// Scenario: 只有非付费套餐（体验版/试用/签到/加量/活动/礼包）时**不产出** Subscription
// ——用户口径是"如果购买了订阅才显示"。这些码全部来自官方 CommodityCode 枚举，
// 逐码覆盖，避免"手滑把 008 freeMon 当订阅码"这类回归。
func TestCodeBuddySubscriptionAbsentForNonPaidPackages(t *testing.T) {
	t.Parallel()

	nonPaidCodes := []struct {
		code string
		note string
	}{
		{"TCACA_code_001_PqouKr6QWV", "free 免费版"},
		{"TCACA_code_006_DbXS0lrypC", "gift 礼包/试用"},
		{"TCACA_code_008_cfWoLwvjU4", "freeMon 体验版月付（V1 实测样本码）"},
		{"TCACA_code_035_ArVxJcGDsm", "freeMonIntl 国际版体验"},
		{"TCACA_code_039_KRcQj7wUat", "proTrialMon 专业版试用月付"},
		{"TCACA_code_040_mi9rCYg46x", "proTrialYear 专业版试用年付"},
		{"TCACA_code_009_0XmEQc2xOf", "extra 加量包"},
		{"TCACA_code_038_OhvqZtiPKr", "extra38 加量包"},
		{"TCACA_code_036_lupO5WgNdG", "extraIntl 国际版加量包"},
		{"TCACA_code_007_nzdH5h4Nl0", "activity 活动包（V1 签到来源）"},
		{"TCACA_code_028_NtpWi0jzXs", "bonus28 活动加赠"},
		{"TCACA_code_029_6wCGEWquYy", "bonus29 活动加赠"},
		{"TCACA_code_030_BjSt89qTvr", "bonus30 活动加赠"},
		{"TCACA_code_037_WxOD3MpI2o", "bonusIntl 国际版活动加赠"},
	}
	for _, item := range nonPaidCodes {
		t.Run(item.note, func(t *testing.T) {
			t.Parallel()
			subscription := codeBuddySubscription([]map[string]any{{
				"PackageCode":      item.code,
				"PackageName":      "非付费套餐",
				"CapacityRemain":   100,
				"DeductionEndTime": "2026-12-01 08:00:00",
			}})
			require.Nil(t, subscription, "非付费套餐不得产出订阅（码 %s）", item.code)
		})
	}
}

// Scenario: 命中付费订阅但上游没给 PackageName → Name 退 PackageCode
// （不自建中文翻译映射：youth/advanced 的官方中文名未实证）；PackageName 带空白
// 也按缺失处理。
func TestCodeBuddySubscriptionNameFallback(t *testing.T) {
	t.Parallel()

	subscription := codeBuddySubscription([]map[string]any{{
		"PackageCode":  "TCACA_code_026_BaESVICNoi",
		"CycleEndTime": "2026-12-01 08:00:00",
	}})
	require.NotNil(t, subscription)
	require.Equal(t, "TCACA_code_026_BaESVICNoi", subscription.Name)

	blank := codeBuddySubscription([]map[string]any{{
		"PackageCode":  "TCACA_code_026_BaESVICNoi",
		"PackageName":  "   ",
		"CycleEndTime": "2026-12-01 08:00:00",
	}})
	require.NotNil(t, blank)
	require.Equal(t, "TCACA_code_026_BaESVICNoi", blank.Name)
}

// Scenario: 订阅判定**与余额无关**——额度耗尽的付费订阅（Remain=0）仍要显示到期
// 时间，而它**不进** Expiries（到期列表恒为"仅仍有余额的套餐"口径）。
// 两条口径刻意分家：Expiries 答"积分什么时候作废"，Subscription 答"订阅什么时候到期"。
func TestCodeBuddySubscriptionIndependentOfBalance(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_002_AkiJS3ZHF5","PackageName":"CodeBuddy专业版","CycleCapacitySize":1000,"CycleCapacityRemain":0,"CycleEndTime":"2026-10-09 12:34:56"}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)
	require.NotNil(t, usage.Subscription, "额度耗尽的订阅仍必须展示到期时间")
	require.Equal(t, "2026-10-09T04:34:56Z", usage.Subscription.ExpiresAt.UTC().Format(time.RFC3339))
	require.Empty(t, usage.Expiries, "Expiries 恒为仅含仍有余额的套餐——两条口径不得混")
	require.NotNil(t, usage.Balance)
	require.InDelta(t, 0.0, *usage.Balance, 1e-9)
}

// Scenario: 付费套餐在册但到期时刻全链不可解析 → 不产出 Subscription（也不报错，
// 余额照常返回）。到期时间对订阅是必填展示值：宁可不显示这一行，
// 也不能下发零值时刻（前端会渲染成 0001 年）。
func TestCodeBuddySubscriptionSkippedWhenExpiryUnparseable(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_027_0FCGVA6vSa","PackageName":"CodeBuddy旗舰版","CycleCapacitySize":1000,"CycleCapacityRemain":800,"DeductionEndTime":"","ExpiredTime":"not-a-time"}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)
	require.Nil(t, usage.Subscription)
	require.NotNil(t, usage.Balance, "展示字段缺失不得影响余额解析")
	require.InDelta(t, 800.0, *usage.Balance, 1e-9)
}

// Scenario: 高优先级付费套餐拿不到合理到期时刻时**整行忽略、不下沉**到低优先级
// 付费套餐——否则会把低档套餐的到期时间冒充成用户订阅的到期时间。这条钉住
// codeBuddySubscription 里 `return nil`（而非 continue）的刻意选择：改回 continue
// 不会让任何其他测试变红，但语义会静默漂移。
func TestCodeBuddySubscriptionNoFallThroughToLowerPriorityPlan(t *testing.T) {
	t.Parallel()

	subscription := codeBuddySubscription([]map[string]any{
		{
			"PackageCode":      "TCACA_code_027_0FCGVA6vSa", // flagship，最高优先级
			"PackageName":      "CodeBuddy旗舰版",
			"DeductionEndTime": "not-a-time",
			"CycleEndTime":     "",
		},
		{
			"PackageCode":  "TCACA_code_002_AkiJS3ZHF5", // proMon，可解析
			"PackageName":  "CodeBuddy专业版",
			"CycleEndTime": "2026-12-01 08:00:00",
		},
	})
	require.Nil(t, subscription, "旗舰版到期不可解析时不得拿专业版的到期顶上")
}

// Scenario: 上游 Dart 栈的零值/纪元**占位日期**能被 layout 成功解析（"0001-01-01
// 00:00:00"、"1970-01-01 08:00:00"）——只判解析错误挡不住，会真的渲染出"到期
// 1/1/1"。年份下界（codeBuddySubscriptionMinExpiryYear）把占位串当"不可解析"
// 处理：链上有占位串时继续退下一键，全链只有占位串时整行不产出。
func TestCodeBuddySubscriptionPlaceholderDatesRejected(t *testing.T) {
	t.Parallel()

	t.Run("DeductionEndTime 是零值占位 → 退到 CycleEndTime", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": "0001-01-01 00:00:00",
			"CycleEndTime":     "2026-12-01 08:00:00",
		}})
		require.NotNil(t, subscription)
		require.Equal(t, "2026-12-01T00:00:00Z", subscription.ExpiresAt.UTC().Format(time.RFC3339))
	})

	t.Run("纪元占位串同样被拒", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": "1970-01-01 08:00:00",
			"CycleEndTime":     "2026-12-01 08:00:00",
		}})
		require.NotNil(t, subscription)
		require.Equal(t, "2026-12-01T00:00:00Z", subscription.ExpiresAt.UTC().Format(time.RFC3339))
	})

	t.Run("全链只有占位串 → 不产出订阅", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": "0001-01-01 00:00:00",
			"ExpiredTime":      "0001-01-01 00:00:00",
			"CycleEndTime":     "1970-01-01 08:00:00",
		}})
		require.Nil(t, subscription, "占位日期不是到期时间，不得下发")
	})
}

// Scenario: AutoRenewFlag 上游有数字与字符串两形态（官方客户端按 Number(...) 容忍
// 两态），字符串 "1" 必须识别为自动续费、非 1 值（"0"/2/缺省）按非自动续费。
func TestCodeBuddySubscriptionAutoRenewFlagStringForm(t *testing.T) {
	t.Parallel()

	subscription := codeBuddySubscription([]map[string]any{{
		"PackageCode":   "TCACA_code_002_AkiJS3ZHF5",
		"CycleEndTime":  "2026-12-01 08:00:00",
		"AutoRenewFlag": "1",
	}})
	require.NotNil(t, subscription)
	require.True(t, subscription.AutoRenew, `字符串 "1" 必须识别为自动续费`)

	off := codeBuddySubscription([]map[string]any{{
		"PackageCode":   "TCACA_code_002_AkiJS3ZHF5",
		"CycleEndTime":  "2026-12-01 08:00:00",
		"AutoRenewFlag": "0",
	}})
	require.NotNil(t, off)
	require.False(t, off.AutoRenew)
}

// Scenario: 付费码集与官方 CommodityCode 枚举的**不变量**——官方枚举（提取件
// renderer__assets__common-DmYVkoTi.js:23849-23871，共 20 码）必须恰好划分为
// "付费订阅码（codeBuddyPaidSubscriptionCodes）"与"其余（非付费，逐码断言不产出
// 订阅）"，不允许既不在付费列表又没被当非付费覆盖的码。上游枚举扩充时这条会
// 以计数差不匹配的方式报警，而不是静默把新付费码归入非付费。
func TestCodeBuddyPaidSubscriptionCodeSetInvariant(t *testing.T) {
	t.Parallel()

	official := map[string]bool{
		"TCACA_code_001_PqouKr6QWV": true, // free
		"TCACA_code_002_AkiJS3ZHF5": true, // proMon
		"TCACA_code_003_FAnt7lcmRT": true, // proYear
		"TCACA_code_005_maRGyrHhw1": true, // proMonPlus
		"TCACA_code_006_DbXS0lrypC": true, // gift
		"TCACA_code_007_nzdH5h4Nl0": true, // activity
		"TCACA_code_008_cfWoLwvjU4": true, // freeMon
		"TCACA_code_009_0XmEQc2xOf": true, // extra
		"TCACA_code_023_4xbGhMrE6q": true, // youth
		"TCACA_code_026_BaESVICNoi": true, // advanced
		"TCACA_code_027_0FCGVA6vSa": true, // flagship
		"TCACA_code_028_NtpWi0jzXs": true, // bonus28
		"TCACA_code_029_6wCGEWquYy": true, // bonus29
		"TCACA_code_030_BjSt89qTvr": true, // bonus30
		"TCACA_code_035_ArVxJcGDsm": true, // freeMonIntl
		"TCACA_code_036_lupO5WgNdG": true, // extraIntl
		"TCACA_code_037_WxOD3MpI2o": true, // bonusIntl
		"TCACA_code_038_OhvqZtiPKr": true, // extra38
		"TCACA_code_039_KRcQj7wUat": true, // proTrialMon
		"TCACA_code_040_mi9rCYg46x": true, // proTrialYear
	}

	paid := map[string]bool{}
	for _, code := range codeBuddyPaidSubscriptionCodes {
		require.False(t, paid[code], "付费码重复：%s", code)
		require.True(t, official[code], "付费码不在官方枚举里（写错码？）：%s", code)
		paid[code] = true
	}

	// 枚举的其余码全部按非付费处理：不产出订阅。
	nonPaidCount := 0
	for code := range official {
		if paid[code] {
			continue
		}
		nonPaidCount++
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":  code,
			"CycleEndTime": "2026-12-01 08:00:00",
		}})
		require.Nil(t, subscription, "官方枚举里未被划入付费的码不得产出订阅（码 %s）", code)
	}
	require.Equal(t, len(official)-len(paid), nonPaidCount)
	require.Equal(t, 6, len(paid), "付费码集是 6 档（flagship/advanced/youth/proMon/proMonPlus/proYear）")
	require.Equal(t, 20, len(official), "官方枚举现值 20 码；上游扩充时请同步更新本测试与付费列表")
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

// Scenario: 2026-10-09 dev 真机抓包复现——高级版套餐（026，"Buddy AI个人高级版"）
// 的 DeductionEndTime 是**纪元毫秒数**（1792473737000 = 2026-10-20 13:22:17 UTC+8，
// 官方 JS 客户端 parseTime 两态都吃）。Go 的墙钟 layout 解不了数字，必须按纪元
// 绝对时刻解释——否则会错误退链到 ExpiredTime（2026-09-23，已过期）显示错日期。
// 夹具逐字来自 dev 账号 11 的真实上游响应。
func TestCodeBuddySubscriptionRealAdvancedFixture(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_026_BaESVICNoi","PackageName":"Buddy AI个人高级版","CycleEndTime":"2026-10-20 13:22:17","DeductionEndTime":1792473737000,"ExpiredTime":"2026-09-23 16:04:17","AutoRenewFlag":0,"CycleCapacityRemain":0,"CycleCapacityRemainPrecise":"0","Status":3},
		{"PackageCode":"TCACA_code_007_nzdH5h4Nl0","PackageName":"CodeBuddy个人版国内运营裂变包","CycleEndTime":"2026-11-09 09:58:04","DeductionEndTime":1794189484000,"ExpiredTime":"","AutoRenewFlag":0,"CycleCapacityRemain":50,"CycleCapacityRemainPrecise":"50.96000011","Status":0}
	]}}}}`)
	usage, err := parseCodeBuddyCreditsResponse(raw)
	require.NoError(t, err)
	require.NotNil(t, usage.Subscription, "真机 026 套餐必须产出订阅")
	require.Equal(t, "TCACA_code_026_BaESVICNoi", usage.Subscription.PackageCode)
	require.Equal(t, "Buddy AI个人高级版", usage.Subscription.Name)
	// 纪元毫秒优先于墙钟串的 ExpiredTime：1792473737000ms = 2026-10-20T05:22:17Z
	// （= UTC+8 墙钟 2026-10-20 13:22:17，与该套餐 CycleEndTime 一致，官方客户端同值）。
	require.Equal(t, "2026-10-20T05:22:17Z", usage.Subscription.ExpiresAt.UTC().Format(time.RFC3339))
	require.False(t, usage.Subscription.AutoRenew)
	// 余额与到期列表口径不受影响（对照真机：balance 50.96、1 条 expiry）。
	// 容差 1e-7：Precise 是字符串小数，float64 往返有表示误差。
	require.NotNil(t, usage.Balance)
	require.InDelta(t, 50.96000011, *usage.Balance, 1e-7)
	require.Len(t, usage.Expiries, 1)
}

// Scenario: 纪元数字三形态（毫秒数字 / 秒数字 / 纯数字串）都按绝对时刻解释；
// 纪元占位（0 → 1970）被 minYear 挡下后**继续退链**取下一键，不整行丢弃。
func TestCodeBuddySubscriptionEpochTimeForms(t *testing.T) {
	t.Parallel()

	t.Run("纪元毫秒（数字）", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": 1792473737000.0,
		}})
		require.NotNil(t, subscription)
		require.Equal(t, "2026-10-20T05:22:17Z", subscription.ExpiresAt.UTC().Format(time.RFC3339))
	})

	t.Run("纪元秒（数字）", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": 1792473737.0,
		}})
		require.NotNil(t, subscription)
		require.Equal(t, "2026-10-20T05:22:17Z", subscription.ExpiresAt.UTC().Format(time.RFC3339))
	})

	t.Run("纪元毫秒（纯数字串）", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": "1792473737000",
		}})
		require.NotNil(t, subscription)
		require.Equal(t, "2026-10-20T05:22:17Z", subscription.ExpiresAt.UTC().Format(time.RFC3339))
	})

	t.Run("纪元占位 0 被 minYear 挡下 → 退 ExpiredTime", func(t *testing.T) {
		t.Parallel()
		subscription := codeBuddySubscription([]map[string]any{{
			"PackageCode":      "TCACA_code_002_AkiJS3ZHF5",
			"DeductionEndTime": 0,
			"ExpiredTime":      "2026-12-01 08:00:00",
		}})
		require.NotNil(t, subscription)
		require.Equal(t, "2026-12-01T00:00:00Z", subscription.ExpiresAt.UTC().Format(time.RFC3339))
	})
}
