//go:build unit

package service

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// zhipuSchedulerTestAccount 构造调度成本接缝测试账号；credentials 为 nil 时给空 map。
func zhipuSchedulerTestAccount(id int64, platform, accountType string, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        accountType,
		Credentials: credentials,
	}
}

// zhipuSchedulerLoginManagedAccount 构造 #03 定义的登录态智谱账号
// （platform=zhipu && type=apikey && credentials.auth_flow=bigmodel_oauth）。
func zhipuSchedulerLoginManagedAccount(id int64) *Account {
	return zhipuSchedulerTestAccount(id, PlatformZhipu, AccountTypeAPIKey, map[string]any{
		"auth_flow": "bigmodel_oauth",
		"api_key":   "k",
	})
}

// TestOpenAISchedulingRateEligibleTruthTable 钉死抽取后的平台门控真值表。
// 既有面（openai/openai-compatible apikey、openai oauth/setup_token）逐项不变；
// 新增面只有登录态智谱账号；手填 api_key 的智谱账号按既有语义不进入成本因子
// （ticket 26 要求实现固定此语义并用测试锁死）。
func TestOpenAISchedulingRateEligibleTruthTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil 账号", nil, false},
		{"openai apikey（既有）", zhipuSchedulerTestAccount(1, PlatformOpenAI, AccountTypeAPIKey, nil), true},
		{"openai oauth（既有）", zhipuSchedulerTestAccount(2, PlatformOpenAI, AccountTypeOAuth, nil), true},
		{"openai setup_token（既有 oauth-like）", zhipuSchedulerTestAccount(3, PlatformOpenAI, AccountTypeSetupToken, nil), true},
		{"opencode apikey（既有 IsOpenAIApiKey）", zhipuSchedulerTestAccount(4, PlatformOpenCode, AccountTypeAPIKey, nil), true},
		{"zhipu 登录态 apikey（新增）", zhipuSchedulerLoginManagedAccount(5), true},
		{"zhipu 手填 apikey（auth_flow 缺失）", zhipuSchedulerTestAccount(6, PlatformZhipu, AccountTypeAPIKey, map[string]any{"api_key": "hand"}), false},
		{"zhipu 手填 apikey（auth_flow 为空）", zhipuSchedulerTestAccount(7, PlatformZhipu, AccountTypeAPIKey, map[string]any{"auth_flow": ""}), false},
		{"zhipu 手填 apikey（auth_flow 异值）", zhipuSchedulerTestAccount(8, PlatformZhipu, AccountTypeAPIKey, map[string]any{"auth_flow": "manual"}), false},
		{"zhipu oauth（非登录态类型）", zhipuSchedulerTestAccount(9, PlatformZhipu, AccountTypeOAuth, map[string]any{"auth_flow": "bigmodel_oauth"}), false},
		{"zhipu setup_token", zhipuSchedulerTestAccount(10, PlatformZhipu, AccountTypeSetupToken, nil), false},
		{"kimi apikey", zhipuSchedulerTestAccount(11, PlatformKimi, AccountTypeAPIKey, nil), false},
		{"deepseek apikey", zhipuSchedulerTestAccount(12, PlatformDeepseek, AccountTypeAPIKey, nil), false},
		{"grok oauth", zhipuSchedulerTestAccount(13, PlatformGrok, AccountTypeOAuth, nil), false},
		{"anthropic apikey", zhipuSchedulerTestAccount(14, PlatformAnthropic, AccountTypeAPIKey, nil), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, openAISchedulingRateEligible(tc.account))
		})
	}
}

// TestOpenAISchedulingRateEligibleZhipuLoginManagedMatchesAuthFlowKey 锁死
// #03 的凭据键名契约（credentials["auth_flow"]=="bigmodel_oauth"）：换键名或换值
// 都会让登录态智谱账号静默退出成本调度。
func TestOpenAISchedulingRateEligibleZhipuLoginManagedMatchesAuthFlowKey(t *testing.T) {
	t.Parallel()

	require.True(t, openAISchedulingRateEligible(zhipuSchedulerLoginManagedAccount(1)))
	// type 缺失（历史/半构造缓存行）同样不进入成本因子。
	account := zhipuSchedulerLoginManagedAccount(2)
	account.Type = ""
	require.False(t, openAISchedulingRateEligible(account))
}

// zhipuSchedulingTestTime 用 UTC+8 显式构造时刻：2026-10-05 为周一，
// 13:59 闲时（0.5）、14:00 起高峰（1.0）。期望值全部为手算字面量。
func zhipuSchedulingTestTime(t *testing.T, hour, min int) time.Time {
	t.Helper()
	return zhipuTestTime(t, 2026, 10, 5, hour, min, 0)
}

// TestZhipuSchedulingRateNumericTable 钉死 design M5 的因子公式
// baseRate(extra["upstream_billing_rate"]，缺省 1.0) × ZhipuPeakFactor(now) × (签名 0.67 / 无签名 1.0)。
// 四组合期望值 = 手算字面量：闲时 0.5 / 高峰 1.0 × 0.67。
func TestZhipuSchedulingRateNumericTable(t *testing.T) {
	t.Parallel()

	signedCreds := func() map[string]any {
		return map[string]any{"auth_flow": "bigmodel_oauth", "zcode_client_sign": "v4"}
	}
	unsignedCreds := func() map[string]any {
		return map[string]any{"auth_flow": "bigmodel_oauth"}
	}

	cases := []struct {
		name  string
		creds map[string]any
		extra map[string]any
		at    time.Time
		want  float64
	}{
		{"闲时无签名×缺省基准", unsignedCreds(), nil, zhipuSchedulingTestTime(t, 13, 59), 0.5},
		{"闲时有签名×缺省基准", signedCreds(), nil, zhipuSchedulingTestTime(t, 13, 59), 0.335},
		{"高峰无签名×缺省基准", unsignedCreds(), nil, zhipuSchedulingTestTime(t, 14, 0), 1.0},
		{"高峰有签名×缺省基准", signedCreds(), nil, zhipuSchedulingTestTime(t, 14, 0), 0.67},
		{"闲时无签名×显式基准 2.0", unsignedCreds(), map[string]any{"upstream_billing_rate": 2.0}, zhipuSchedulingTestTime(t, 13, 59), 1.0},
		{"高峰有签名×显式基准 0.5", signedCreds(), map[string]any{"upstream_billing_rate": 0.5}, zhipuSchedulingTestTime(t, 14, 0), 0.335},
		{"闲时有签名×显式基准 0.25", signedCreds(), map[string]any{"upstream_billing_rate": 0.25}, zhipuSchedulingTestTime(t, 13, 59), 0.08375},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			account := zhipuSchedulerTestAccount(1, PlatformZhipu, AccountTypeAPIKey, tc.creds)
			account.Extra = tc.extra
			require.Equal(t, tc.want, zhipuSchedulingRate(account, tc.at))
		})
	}
}

// TestZhipuSchedulingRateUpstreamBillingRateTolerance 锁死基准倍率的容错语义：
// 只有可解析的有限正数被采纳，其余（缺失/空串/不可解析/0/负数/NaN/Inf）一律回落到 1.0——
// 坏输入绝不能把账号伪装成最便宜。
func TestZhipuSchedulingRateUpstreamBillingRateTolerance(t *testing.T) {
	t.Parallel()

	offPeak := zhipuSchedulingTestTime(t, 13, 59)
	cases := []struct {
		name  string
		extra map[string]any
	}{
		{"Extra 为 nil", nil},
		{"键缺失", map[string]any{}},
		{"值为空串", map[string]any{"upstream_billing_rate": ""}},
		{"值不可解析", map[string]any{"upstream_billing_rate": "abc"}},
		{"值为 0", map[string]any{"upstream_billing_rate": 0}},
		{"值为负数", map[string]any{"upstream_billing_rate": -2.0}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			account := zhipuSchedulerLoginManagedAccount(1)
			account.Extra = tc.extra
			require.Equal(t, 0.5, zhipuSchedulingRate(account, offPeak))
		})
	}

	// 字符串数字（GORM json.Number / 字符串落库）按显式值采纳。
	account := zhipuSchedulerLoginManagedAccount(2)
	account.Extra = map[string]any{"upstream_billing_rate": "2.5"}
	require.Equal(t, 1.25, zhipuSchedulingRate(account, offPeak))
}

// TestZhipuSchedulingRateSignMarker 签名折扣只认账号级标记取值 "v4"；
// 其它取值/缺失与无签名等价。生产环境的全局开关在签名注入点执行（见函数注释）。
func TestZhipuSchedulingRateSignMarker(t *testing.T) {
	t.Parallel()

	peak := zhipuSchedulingTestTime(t, 14, 0)
	for _, marker := range []any{nil, "", "v3", "V4", true} {
		account := zhipuSchedulerLoginManagedAccount(1)
		if marker != nil {
			account.Credentials["zcode_client_sign"] = marker
		}
		require.Equal(t, 1.0, zhipuSchedulingRate(account, peak), "marker=%v 不应触发 0.67", marker)
	}

	signed := zhipuSchedulerLoginManagedAccount(2)
	signed.Credentials["zcode_client_sign"] = "v4"
	require.Equal(t, 0.67, zhipuSchedulingRate(signed, peak))
}

// TestOpenAIUpstreamCostFactorsZhipuPeakFlipsOrder 是 design M5 的意图断言：
// 登录态智谱账号（无签名，缺省基准 1.0）与一个固定倍率 0.7 的 OpenAI 探测账号比较，
// 闲时（0.5）智谱更便宜 → 成本因子更高；高峰（1.0）智谱更贵 → 因子被反超。
// 只断言方向，不重算实现公式。
func TestOpenAIUpstreamCostFactorsZhipuPeakFlipsOrder(t *testing.T) {
	t.Parallel()

	openAI := upstreamCostTestAccount(1, UpstreamBillingProbeStatusOK, 0.7, zhipuSchedulingTestTime(t, 13, 58), 30*time.Minute)
	zhipu := zhipuSchedulerLoginManagedAccount(2)

	offPeak := openAIUpstreamCostFactors([]*Account{openAI, zhipu}, zhipuSchedulingTestTime(t, 13, 59), defaultOpenAIOAuthSchedulingRateMultiplier)
	require.Greater(t, offPeak[zhipu.ID], offPeak[openAI.ID], "闲时智谱 0.5 应优于固定 0.7")
	require.Greater(t, offPeak[zhipu.ID], openAIUpstreamCostNeutralFactor)
	require.Less(t, offPeak[openAI.ID], openAIUpstreamCostNeutralFactor)

	peak := openAIUpstreamCostFactors([]*Account{openAI, zhipu}, zhipuSchedulingTestTime(t, 14, 0), defaultOpenAIOAuthSchedulingRateMultiplier)
	require.Less(t, peak[zhipu.ID], peak[openAI.ID], "高峰智谱 1.0 应劣于固定 0.7（排序翻转）")
	require.Less(t, peak[zhipu.ID], openAIUpstreamCostNeutralFactor)
	require.Greater(t, peak[openAI.ID], openAIUpstreamCostNeutralFactor)
}

// TestOpenAISchedulingRateRoutesZhipuLoginManagedAndKeepsExistingBranches
// 钉死 openAISchedulingRate 的改点：登录态智谱账号走 zhipu 分支，既有分支
// （openai oauth-like 用配置参考倍率、openai apikey 用探测倍率、其它无信号）
// 逐项不变。
func TestOpenAISchedulingRateRoutesZhipuLoginManagedAndKeepsExistingBranches(t *testing.T) {
	t.Parallel()

	offPeak := zhipuSchedulingTestTime(t, 13, 59)
	peak := zhipuSchedulingTestTime(t, 14, 0)

	zhipuUnsigned := zhipuSchedulerLoginManagedAccount(1)
	rate, ok := openAISchedulingRate(zhipuUnsigned, offPeak, 0.05)
	require.True(t, ok)
	require.Equal(t, 0.5, rate)

	zhipuSigned := zhipuSchedulerLoginManagedAccount(2)
	zhipuSigned.Credentials["zcode_client_sign"] = "v4"
	rate, ok = openAISchedulingRate(zhipuSigned, peak, 0.05)
	require.True(t, ok)
	require.Equal(t, 0.67, rate)

	openAIOAuth := zhipuSchedulerTestAccount(3, PlatformOpenAI, AccountTypeOAuth, nil)
	rate, ok = openAISchedulingRate(openAIOAuth, peak, 0.05)
	require.True(t, ok)
	require.Equal(t, 0.05, rate)

	openAIAPIKey := upstreamCostTestAccount(4, UpstreamBillingProbeStatusOK, 0.7, offPeak.Add(-time.Minute), 30*time.Minute)
	rate, ok = openAISchedulingRate(openAIAPIKey, offPeak, 0.05)
	require.True(t, ok)
	require.Equal(t, 0.7, rate)

	for _, account := range []*Account{
		nil,
		zhipuSchedulerTestAccount(5, PlatformKimi, AccountTypeAPIKey, nil),
		zhipuSchedulerTestAccount(6, PlatformZhipu, AccountTypeAPIKey, map[string]any{"api_key": "hand"}),
		zhipuSchedulerTestAccount(7, PlatformGrok, AccountTypeOAuth, nil),
	} {
		rate, ok = openAISchedulingRate(account, offPeak, 0.05)
		require.False(t, ok, "无成本信号的账号不得产生倍率")
		require.Zero(t, rate)
	}
}

// TestOpenAIUpstreamCostFactorsZhipuSignedBeatsUnsigned 签名（0.67 渠道系数）在
// 同一时刻把因子推过中性值，无签名账号被压到中性值以下：0.335 vs 0.5。
func TestOpenAIUpstreamCostFactorsZhipuSignedBeatsUnsigned(t *testing.T) {
	t.Parallel()

	offPeak := zhipuSchedulingTestTime(t, 13, 59)
	signed := zhipuSchedulerLoginManagedAccount(1)
	signed.Credentials["zcode_client_sign"] = "v4"
	unsigned := zhipuSchedulerLoginManagedAccount(2)

	factors := openAIUpstreamCostFactors([]*Account{signed, unsigned}, offPeak, defaultOpenAIOAuthSchedulingRateMultiplier)
	require.Greater(t, factors[signed.ID], openAIUpstreamCostNeutralFactor)
	require.Less(t, factors[unsigned.ID], openAIUpstreamCostNeutralFactor)
	require.Greater(t, factors[signed.ID], factors[unsigned.ID])
}

// TestOpenAILegacyUpstreamRateOrderIncludesOnlyEligiblePlatformRates 钉死 legacy
// 低倍率优先排序的信任面：登录态智谱账号进入 rates（与 OpenAI 账号同池比较），
// 手填 api_key 的智谱账号与 kimi/deepseek/anthropic/grok 即便持有新鲜探测倍率
// 也不进入——门控抽取（ticket 26）没有扩大既有信任面。
func TestOpenAILegacyUpstreamRateOrderIncludesOnlyEligiblePlatformRates(t *testing.T) {
	t.Parallel()

	now := zhipuSchedulingTestTime(t, 13, 59)
	freshProbe := func(id int64, platform string, rate float64) *Account {
		account := upstreamCostTestAccount(id, UpstreamBillingProbeStatusOK, rate, now.Add(-time.Minute), 30*time.Minute)
		account.Platform = platform
		return account
	}
	zhipuSigned := zhipuSchedulerLoginManagedAccount(1)
	zhipuSigned.Credentials["zcode_client_sign"] = "v4"
	zhipuUnsigned := zhipuSchedulerLoginManagedAccount(2)
	openAI := freshProbe(3, PlatformOpenAI, 0.7)
	handFilledZhipu := freshProbe(4, PlatformZhipu, 0.01)
	kimi := freshProbe(5, PlatformKimi, 0.01)
	deepseek := freshProbe(6, PlatformDeepseek, 0.01)
	anthropic := freshProbe(7, PlatformAnthropic, 0.01)
	grok := freshProbe(8, PlatformGrok, 0.01)

	order := newOpenAILegacyUpstreamRateOrder(
		[]*Account{zhipuSigned, zhipuUnsigned, openAI, handFilledZhipu, kimi, deepseek, anthropic, grok, nil},
		now, defaultOpenAIOAuthSchedulingRateMultiplier,
	)
	require.True(t, order.enabled)
	require.Len(t, order.rates, 3)
	require.Equal(t, 0.335, order.rates[zhipuSigned.ID])
	require.Equal(t, 0.5, order.rates[zhipuUnsigned.ID])
	require.Equal(t, 0.7, order.rates[openAI.ID])
	for _, excluded := range []*Account{handFilledZhipu, kimi, deepseek, anthropic, grok} {
		require.NotContains(t, order.rates, excluded.ID)
		// 无已知倍率的账号排在所有有倍率账号之后。
		require.Positive(t, order.compare(excluded, zhipuSigned))
	}
	require.Negative(t, order.compare(zhipuSigned, zhipuUnsigned))
	require.Negative(t, order.compare(zhipuUnsigned, openAI))
}

// TestZhipuSchedulingRateUsesSamePeakFactorAsCostModel 集成点（ticket 26）：
// 调度因子与 14 的时段函数在同一 now 输入下结果一致，且与进程时区无关
// （同一瞬时用 UTC 表示必须得到同一结果）。
func TestZhipuSchedulingRateUsesSamePeakFactorAsCostModel(t *testing.T) {
	t.Parallel()

	account := zhipuSchedulerLoginManagedAccount(1)
	instants := []struct {
		name string
		at   time.Time
	}{
		{"周一 13:59:59 闲时", zhipuTestTime(t, 2026, 10, 5, 13, 59, 59)},
		{"周一 14:00 高峰起始", zhipuTestTime(t, 2026, 10, 5, 14, 0, 0)},
		{"周一 17:59 高峰末尾", zhipuTestTime(t, 2026, 10, 5, 17, 59, 0)},
		{"周一 18:00 闲时起始", zhipuTestTime(t, 2026, 10, 5, 18, 0, 0)},
		{"周六 14:00 周末闲时", zhipuTestTime(t, 2026, 10, 10, 14, 0, 0)},
	}
	for _, instant := range instants {
		instant := instant
		t.Run(instant.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t,
				zhipuSchedulingRate(account, instant.at),
				zhipuSchedulingRate(account, instant.at.UTC()),
				"时段判定必须与进程/传入时区表示无关")
			require.InDelta(t, ZhipuPeakFactor(instant.at),
				zhipuSchedulingRate(account, instant.at)/ZhipuUnsignedChannelFactor, 1e-12,
				"调度因子中的时段系数必须等于 14 的 ZhipuPeakFactor")
		})
	}
}

// TestOpenAIUpstreamCostFactorsNonZhipuAccountsUnchanged 回归向量：kimi / deepseek /
// anthropic / grok 与手填 api_key 的智谱账号，即使持有新鲜且极低的探测倍率，成本因子
// 仍恒为中性值，且两种候选集（有无登录态智谱账号）下逐项相同——门控抽取没有把这些
// 平台放进成本调度。同时 OpenAI 两个既有分支（oauth 参考倍率 / apikey 探测倍率）
// 仍照旧参与。
func TestOpenAIUpstreamCostFactorsNonZhipuAccountsUnchanged(t *testing.T) {
	t.Parallel()

	now := zhipuSchedulingTestTime(t, 13, 59)
	freshProbe := func(id int64, platform string, rate float64) *Account {
		account := upstreamCostTestAccount(id, UpstreamBillingProbeStatusOK, rate, now.Add(-time.Minute), 30*time.Minute)
		account.Platform = platform
		return account
	}
	kimi := freshProbe(1, PlatformKimi, 0.01)
	deepseek := freshProbe(2, PlatformDeepseek, 0.02)
	anthropic := freshProbe(3, PlatformAnthropic, 0.03)
	grok := freshProbe(4, PlatformGrok, 0.04)
	handFilledZhipu := freshProbe(5, PlatformZhipu, 0.005)
	openAIAPIKey := freshProbe(6, PlatformOpenAI, 0.7)
	openAIOAuth := zhipuSchedulerTestAccount(7, PlatformOpenAI, AccountTypeOAuth, nil)
	ineligible := []*Account{kimi, deepseek, anthropic, grok, handFilledZhipu}

	baseline := openAIUpstreamCostFactors([]*Account{kimi, deepseek, anthropic, grok, handFilledZhipu, openAIAPIKey, openAIOAuth}, now, 0.05)
	// 两个有倍率的账号：{0.05, 0.7}，覆盖度 2/2，center = sqrt(0.035)。
	center := math.Sqrt(0.05 * 0.7)
	require.InDelta(t, 1/(1+0.05/center), baseline[openAIOAuth.ID], 1e-12)
	require.InDelta(t, 1/(1+0.7/center), baseline[openAIAPIKey.ID], 1e-12)

	withZhipu := openAIUpstreamCostFactors([]*Account{kimi, deepseek, anthropic, grok, handFilledZhipu, openAIAPIKey, openAIOAuth, zhipuSchedulerLoginManagedAccount(8)}, now, 0.05)
	for _, account := range ineligible {
		require.Equal(t, openAIUpstreamCostNeutralFactor, baseline[account.ID], "无 zhipu 候选集 %s", account.ID)
		require.Equal(t, openAIUpstreamCostNeutralFactor, withZhipu[account.ID], "有 zhipu 候选集 %s", account.ID)
	}
	// 三个有倍率的账号 {0.05, 0.5, 0.7}，center 为中位数 0.5。
	require.InDelta(t, 1/(1+0.05/0.5), withZhipu[openAIOAuth.ID], 1e-12)
	require.InDelta(t, 1/(1+0.7/0.5), withZhipu[openAIAPIKey.ID], 1e-12)
	require.InDelta(t, 1/(1+0.5/0.5), withZhipu[8], 1e-12)
}
