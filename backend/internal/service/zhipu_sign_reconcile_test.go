//go:build unit

package service

// 智谱签名 L2 费率对账（design M5 费率对账段 / spec 硬性要求 L2 / 票 27）的表驱动用例。
//
// 断言只落在公开接缝上：
//   - 对账入口 ZhipuSignReconciler.RunIfDue（M4 周期任务的唯一调用点）；
//   - 数据源接缝 zhipuReconcileUsageSource（fake 记录每次调用的窗口，不碰真实上游）；
//   - 上报接缝 zhipuReconcileRateSink（fake 记录上报值；另一条用例用 #24 的真实
//     *ZhipuSignAlerts 做端到端）；
//   - #25 的告警契约：OpsBuiltinAlertMetricFor(OpsMetricTypeZhipuSignEffectiveRate)
//     注册项 + 评估器同款 compareMetric；
//   - 快照字段（ZhipuAccountMonitorService.ApplySignReconcileSnapshot）与 JSON 兼容性。
//
// 期望值全部是手算字面量（价格表 glm-5.3 = 6.9 / 1.7 / 24 积分每万 token；
// 闲时 ×0.5、签名渠道 ×0.67 → 10000 输入 token 的期望积分 = 10000×6.9/1e4×0.5×0.67
// = 2.3115），不复用实现里的任何计算路径。
//
// 注意：按 2026-10-06 的编译禁令，本文件只写不跑，待受控验证。

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------- 契约编译期断言

var (
	// 生产数据源：探针服务本身（票 08 的方法签名）。
	_ zhipuReconcileUsageSource = (*ZhipuAccountMonitorService)(nil)
	// 生产上报口：L1/L2 引擎（票 24/25）。
	_ zhipuReconcileRateSink = (*ZhipuSignAlerts)(nil)
	// 生产账号来源（票 28 的同一窄接缝）。
	_ zhipuSignAccountLister = (*zhipuSignReconcileFakeAccounts)(nil)
)

// ---------------------------------------------------------------- 夹具

// 时钟基点：2026-10-06（周二）06:00 UTC+8。6h 窗口 = [05:49, 11:49]，
// 全程落在闲时（高峰是周一~五 14:00–18:00 UTC+8），时段系数恒为 0.5。
var zhipuSignReconcileTestT0 = time.Date(2026, 10, 6, 6, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))

// 对账窗口手算基准：10000 输入 token、无缓存、无输出、闲时、签名渠道。
const (
	zhipuSignReconcileTestDay             = "2026-10-06"
	zhipuSignReconcileTestExpectedCredits = 2.3115
	zhipuSignReconcileTestSignedRate      = 0.67
	// zhipuSignReconcileTestSignedCredits = 2.3115 × 0.67（手算）= 有效系数 0.67 命中。
	zhipuSignReconcileTestSignedCredits = 1.548705
	// zhipuSignReconcileTestSettlement 是窗口末端的结算回退上限（research §六：4–11 分钟）。
	zhipuSignReconcileTestSettlement = 11 * time.Minute
)

func zhipuSignReconcileTestRows(input, cached, output, credits float64) []domain.MonitorQuotaModelCredit {
	return []domain.MonitorQuotaModelCredit{{
		Model:        ZhipuModelGLM53,
		Date:         zhipuSignReconcileTestDay,
		InputTokens:  input,
		CachedTokens: cached,
		OutputTokens: output,
		Credits:      credits,
	}}
}

func zhipuSignReconcileTestFlashRows(input, credits float64) []domain.MonitorQuotaModelCredit {
	return []domain.MonitorQuotaModelCredit{{
		Model:       ZhipuModelGLM53Flash,
		Date:        zhipuSignReconcileTestDay,
		InputTokens: input,
		Credits:     credits,
	}}
}

// zhipuSignReconcileTestAccount 返回「启用签名」的登录托管智谱账号
// （登录托管构造复用票 08 用例的 zhipuMonitorManagedAccount，仅补账号级灰度标记）。
func zhipuSignReconcileTestAccount(id int64, accessToken string) Account {
	account := *zhipuMonitorManagedAccount(id, accessToken)
	account.Credentials[zhipuSignCredentialKey] = zhipuSignCredentialV4
	return account
}

func zhipuSignReconcileTestConfig() *config.Config {
	return &config.Config{Gateway: config.GatewayConfig{Zhipu: config.GatewayZhipuConfig{
		SignV4Enabled:                   true,
		SignAlertEnabled:                true,
		SignReconcileIntervalHours:      6,
		SignReconcileDeviationThreshold: 0.70,
	}}}
}

// ---------------------------------------------------------------- 接缝替身

// zhipuSignReconcileUsageCall 记录一次数据源调用的账号与窗口。
type zhipuSignReconcileUsageCall struct {
	accountID int64
	start     time.Time
	end       time.Time
}

type zhipuSignReconcileUsageResponse struct {
	rows []domain.MonitorQuotaModelCredit
	err  error
}

// zhipuSignReconcileFakeUsage 是 credit-usage 数据源替身：按账号排队给出响应（第 N 次
// 调用用第 N 条；队列耗尽后重复最后一条，方便「基线 + 多轮增量」的场景），
// 并记录全部调用窗口——窗口口径断言全部落在它上面。
type zhipuSignReconcileFakeUsage struct {
	mu        sync.Mutex
	byAccount map[int64][]zhipuSignReconcileUsageResponse
	last      map[int64]zhipuSignReconcileUsageResponse
	calls     []zhipuSignReconcileUsageCall
}

func newZhipuSignReconcileFakeUsage() *zhipuSignReconcileFakeUsage {
	return &zhipuSignReconcileFakeUsage{
		byAccount: map[int64][]zhipuSignReconcileUsageResponse{},
		last:      map[int64]zhipuSignReconcileUsageResponse{},
	}
}

func (f *zhipuSignReconcileFakeUsage) push(accountID int64, rows []domain.MonitorQuotaModelCredit, err error) {
	f.byAccount[accountID] = append(f.byAccount[accountID], zhipuSignReconcileUsageResponse{rows: rows, err: err})
}

func (f *zhipuSignReconcileFakeUsage) FetchUsageDetailForAccount(
	_ context.Context,
	account *Account,
	start, end time.Time,
) ([]domain.MonitorQuotaModelCredit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zhipuSignReconcileUsageCall{accountID: account.ID, start: start, end: end})

	queue := f.byAccount[account.ID]
	if len(queue) == 0 {
		last, ok := f.last[account.ID]
		if !ok {
			return nil, errors.New("zhipu sign reconcile test: no scripted usage response")
		}
		return last.rows, last.err
	}
	response := queue[0]
	f.byAccount[account.ID] = queue[1:]
	f.last[account.ID] = response
	return response.rows, response.err
}

func (f *zhipuSignReconcileFakeUsage) callLog() []zhipuSignReconcileUsageCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]zhipuSignReconcileUsageCall(nil), f.calls...)
}

func (f *zhipuSignReconcileFakeUsage) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// zhipuSignReconcileFakeSink 是上报接缝替身：记录每一次上报值。
type zhipuSignReconcileFakeSink struct {
	mu    sync.Mutex
	rates []float64
}

func (s *zhipuSignReconcileFakeSink) RecordEffectiveRate(_ context.Context, rate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rates = append(s.rates, rate)
}

func (s *zhipuSignReconcileFakeSink) values() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]float64(nil), s.rates...)
}

// zhipuSignReconcileFakeAccounts 是账号来源替身（记录被查询的平台）。
type zhipuSignReconcileFakeAccounts struct {
	accounts  []Account
	err       error
	platforms []string
}

func (l *zhipuSignReconcileFakeAccounts) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	l.platforms = append(l.platforms, platform)
	if l.err != nil {
		return nil, l.err
	}
	return l.accounts, nil
}

// zhipuSignReconcileFakeConfigSource 是 #28 配置面的替身（运行层覆盖后的生效值）。
type zhipuSignReconcileFakeConfigSource struct {
	config ZhipuSignConfig
	calls  int
}

func (s *zhipuSignReconcileFakeConfigSource) Effective(context.Context) ZhipuSignConfig {
	s.calls++
	return s.config
}

// ---------------------------------------------------------------- 断言小工具

func zhipuSignReconcileTestReconciler(
	clock *zhipuSignAlertClock,
	accounts *zhipuSignReconcileFakeAccounts,
	usage *zhipuSignReconcileFakeUsage,
	sink zhipuReconcileRateSink,
	cfg *config.Config,
) *ZhipuSignReconciler {
	if cfg == nil {
		cfg = zhipuSignReconcileTestConfig()
	}
	return NewZhipuSignReconciler(cfg, accounts, usage, sink, clock.now)
}

func requireZhipuSignReconcileFinite(t *testing.T, result ZhipuSignReconcileResult) {
	t.Helper()
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"effective_rate", result.EffectiveRate},
		{"actual_credits", result.ActualCredits},
		{"expected_credits", result.ExpectedCredits},
		{"peak_factor", result.PeakFactor},
		{"threshold", result.Threshold},
	} {
		require.Falsef(t, math.IsNaN(field.value), "%s must not be NaN", field.name)
		require.Falsef(t, math.IsInf(field.value, 0), "%s must be finite", field.name)
	}
}

func zhipuSignReconcileTestRun(t *testing.T, reconciler *ZhipuSignReconciler) (ZhipuSignReconcileResult, bool) {
	t.Helper()
	result, applied := reconciler.RunIfDue(context.Background())
	requireZhipuSignReconcileFinite(t, result)
	return result, applied
}

// zhipuSignReconcileTestBaseline 跑首轮并断言「只记基线、不产出系数」。
func zhipuSignReconcileTestBaseline(t *testing.T, reconciler *ZhipuSignReconciler) {
	t.Helper()
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied, "首轮没有上一读数，只记基线")
	require.False(t, result.Applied)
	require.Equal(t, ZhipuSignReconcileReasonNoBaseline, result.Reason)
}

func requireZhipuSignReconcileSameInstant(t *testing.T, want, got time.Time, message string) {
	t.Helper()
	require.True(t, want.Equal(got), "%s: want %s, got %s",
		message, want.Format(time.RFC3339), got.Format(time.RFC3339))
}

// ---------------------------------------------------------------- 有效系数：0.67 命中 / 0.9 / 1.0 告警

func TestZhipuSignReconcilerEffectiveRateAlignment(t *testing.T) {
	cases := []struct {
		name          string
		actualCredits float64 // 手算：期望积分 2.3115 × 目标系数
		wantRate      float64
		wantAlert     bool
	}{
		{name: "signed_factor_matches_target", actualCredits: 1.548705, wantRate: 0.67, wantAlert: false},
		{name: "deviation_threshold_boundary_0_70_is_not_alerting", actualCredits: 1.61805, wantRate: 0.70, wantAlert: false},
		{name: "silent_drift_0_9_alerts", actualCredits: 2.08035, wantRate: 0.9, wantAlert: true},
		{name: "silent_unsigned_billing_1_0_alerts", actualCredits: 2.3115, wantRate: 1.0, wantAlert: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
			accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
				zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
			}}
			usage := newZhipuSignReconcileFakeUsage()
			usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
			usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, tc.actualCredits), nil)
			sink := &zhipuSignReconcileFakeSink{}
			reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

			zhipuSignReconcileTestBaseline(t, reconciler)
			clock.advance(6 * time.Hour)
			result, applied := zhipuSignReconcileTestRun(t, reconciler)

			require.True(t, applied)
			require.True(t, result.Applied)
			require.Empty(t, result.Reason)
			require.Equal(t, 1, result.Accounts)
			require.InDelta(t, zhipuSignReconcileTestExpectedCredits, result.ExpectedCredits, 1e-9)
			require.InDelta(t, tc.actualCredits, result.ActualCredits, 1e-9)
			require.InDelta(t, tc.wantRate, result.EffectiveRate, 1e-9)
			require.Equal(t, ZhipuOffPeakFactorValue, result.PeakFactor)
			// 上报口收到的就是同一个值（#25 的指标源读它）。
			reported := sink.values()
			require.Len(t, reported, 1)
			require.InDelta(t, tc.wantRate, reported[0], 1e-9)
			// #25 的规则判定：内置注册项的算子（严格大于）与默认阈值 0.70。
			metric, found := OpsBuiltinAlertMetricFor(OpsMetricTypeZhipuSignEffectiveRate)
			require.True(t, found)
			require.Equal(t, tc.wantAlert, compareMetric(result.EffectiveRate, metric.Operator, metric.Threshold))
			require.Equal(t, tc.wantAlert, result.Deviation)
		})
	}
}

func TestZhipuSignReconcilerSubtractsBaselineReading(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	// 基线读数（当日累计）：40000 输入 token / 100.5 积分。
	usage.push(2701, zhipuSignReconcileTestRows(40000, 0, 0, 100.5), nil)
	// 本轮读数：50000 输入 token / 102.048705 积分 → 增量 10000 token / 1.548705 积分。
	usage.push(2701, zhipuSignReconcileTestRows(50000, 0, 0, 102.048705), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(6 * time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)

	require.True(t, applied)
	require.InDelta(t, 1.548705, result.ActualCredits, 1e-9)
	require.InDelta(t, zhipuSignReconcileTestExpectedCredits, result.ExpectedCredits, 1e-9)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, result.EffectiveRate, 1e-9)
}

func TestZhipuSignReconcilerExpectedCreditsUseWindowMeanPeakFactor(t *testing.T) {
	// 窗口 [13:00, 15:00) 横跨闲时（13:00–14:00）与高峰（14:00–15:00）：期望积分对窗口
	// 逐分钟采样取平均 → 时段系数 0.75，期望积分 = 6.9×0.75×0.67 = 3.46725（手算）。
	t0 := time.Date(2026, 10, 6, 13, 11, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	cfg := zhipuSignReconcileTestConfig()
	cfg.Gateway.Zhipu.SignReconcileIntervalHours = 1
	clock := newZhipuSignAlertClock(t0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	// 增量积分 = 3.46725 × 0.67 = 2.3230575 → 有效系数 0.67。
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, 2.3230575), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, cfg)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(2 * time.Hour) // 15:11 → 已结算窗口 [13:00, 15:00]
	result, applied := zhipuSignReconcileTestRun(t, reconciler)

	require.True(t, applied)
	require.InDelta(t, 3.46725, result.ExpectedCredits, 1e-9)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, result.EffectiveRate, 1e-9)
	require.Equal(t, ZhipuPeakFactorValue, result.PeakFactor, "展示口径是当前时刻的时段系数")

	calls := usage.callLog()
	require.Len(t, calls, 2)
	requireZhipuSignReconcileSameInstant(t,
		t0.Add(2*time.Hour-zhipuSignReconcileTestSettlement), calls[1].end, "窗口末端按已结算边界")
}

// ---------------------------------------------------------------- 窗口口径

func TestZhipuSignReconcilerUsesSettledWindow(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	zhipuSignReconcileTestBaseline(t, reconciler)
	baselineCalls := usage.callLog()
	require.Len(t, baselineCalls, 1)
	requireZhipuSignReconcileSameInstant(t,
		zhipuSignReconcileTestT0.Add(-zhipuSignReconcileTestSettlement), baselineCalls[0].end,
		"基线读数也按已结算时刻取")

	clock.advance(6 * time.Hour)
	_, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)

	calls := usage.callLog()
	require.Len(t, calls, 2)
	// 末端回退结算延时上限（research §六：4–11 分钟，取 11）。
	requireZhipuSignReconcileSameInstant(t,
		zhipuSignReconcileTestT0.Add(6*time.Hour-zhipuSignReconcileTestSettlement), calls[1].end,
		"窗口末端必须是已结算边界")
	// 相邻窗口首尾相接：本轮起点 = 上轮窗口末端。
	requireZhipuSignReconcileSameInstant(t, calls[0].end, calls[1].start, "窗口首尾相接")
}

func TestZhipuSignReconcilerSkipsShortWindowWithoutAdvancingBaseline(t *testing.T) {
	cfg := zhipuSignReconcileTestConfig()
	cfg.Gateway.Zhipu.SignReconcileIntervalHours = 1
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, cfg)

	zhipuSignReconcileTestBaseline(t, reconciler)
	baselineEnd := usage.callLog()[0].end

	// 40 分钟后：窗口 40 分钟 < 1h 下限 → 不计算、不再打数据源、不推进基线。
	clock.advance(40 * time.Minute)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied)
	require.Equal(t, ZhipuSignReconcileReasonWindowTooShort, result.Reason)
	require.Equal(t, 1, usage.callCount(), "窗口不足时不得调用数据源")
	require.Empty(t, sink.values())

	// 再过 35 分钟（距基线 1h15m）：窗口达标 → 用累积后的完整窗口对账。
	clock.advance(35 * time.Minute)
	result, applied = zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, result.EffectiveRate, 1e-9)
	calls := usage.callLog()
	require.Len(t, calls, 2)
	requireZhipuSignReconcileSameInstant(t, baselineEnd, calls[1].start, "跳过的一轮不推进基线")
	requireZhipuSignReconcileSameInstant(t,
		zhipuSignReconcileTestT0.Add(75*time.Minute-zhipuSignReconcileTestSettlement), calls[1].end,
		"窗口末端按已结算边界")
}

func TestZhipuSignReconcilerSkipsBeforeIntervalElapsed(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	zhipuSignReconcileTestBaseline(t, reconciler)
	// 1 小时后：窗口 1h 已过 1h 下限，但不足配置的 6h 周期 → 未到期。
	clock.advance(time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied)
	require.Equal(t, ZhipuSignReconcileReasonNotDue, result.Reason)
	require.Equal(t, 1, usage.callCount())
	require.Empty(t, sink.values())
}

// ---------------------------------------------------------------- 跳过与失败口径

func TestZhipuSignReconcilerNoSignedAccounts(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		// 登录托管但未开账号级签名灰度。
		*zhipuMonitorManagedAccount(2801, "bm-token-2801"),
		// 非智谱平台账号。
		{ID: 2802, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive},
	}}
	usage := newZhipuSignReconcileFakeUsage()
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied, "没有启用签名的账号时不产生指标")
	require.Equal(t, ZhipuSignReconcileReasonNoSignedAccounts, result.Reason)
	require.Equal(t, []string{PlatformZhipu}, accounts.platforms)
	require.Empty(t, usage.callLog())
	require.Empty(t, sink.values())
}

func TestZhipuSignReconcilerRequiresGlobalSignSwitch(t *testing.T) {
	deploymentDisabled := zhipuSignReconcileTestConfig()
	deploymentDisabled.Gateway.Zhipu.SignV4Enabled = false

	// 全局开关关（部署层或运行层覆盖）→ 不采集：签名没生效时算出的 ≈1.0 不是失效，
	// 报上去只会制造误告警。
	t.Run("deployment_layer_disabled", func(t *testing.T) {
		clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
		accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
			zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
		}}
		usage := newZhipuSignReconcileFakeUsage()
		sink := &zhipuSignReconcileFakeSink{}
		reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, deploymentDisabled)

		result, applied := zhipuSignReconcileTestRun(t, reconciler)
		require.False(t, applied)
		require.Equal(t, ZhipuSignReconcileReasonSignDisabled, result.Reason)
		require.Empty(t, usage.callLog())
		require.Empty(t, sink.values())
	})

	t.Run("runtime_override_disabled", func(t *testing.T) {
		clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
		accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
			zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
		}}
		usage := newZhipuSignReconcileFakeUsage()
		sink := &zhipuSignReconcileFakeSink{}
		source := &zhipuSignReconcileFakeConfigSource{config: ZhipuSignConfig{
			SignV4Enabled:                   false,
			SignReconcileIntervalHours:      6,
			SignReconcileDeviationThreshold: 0.70,
		}}
		reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, zhipuSignReconcileTestConfig())
		reconciler.SetConfigSource(source)

		result, applied := zhipuSignReconcileTestRun(t, reconciler)
		require.False(t, applied)
		require.Equal(t, ZhipuSignReconcileReasonSignDisabled, result.Reason)
		require.Equal(t, 1, source.calls)
		require.Empty(t, usage.callLog())
	})
}

func TestZhipuSignReconcilerUsesRuntimeConfigSource(t *testing.T) {
	// 运行层覆盖（#28）：周期 1h（部署层是 6h）、偏离阈值 0.5（0.67 > 0.5 → 偏离）。
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	source := &zhipuSignReconcileFakeConfigSource{config: ZhipuSignConfig{
		SignV4Enabled:                   true,
		SignAlertEnabled:                true,
		SignReconcileIntervalHours:      1,
		SignReconcileDeviationThreshold: 0.5,
	}}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, zhipuSignReconcileTestConfig())
	reconciler.SetConfigSource(source)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied, "周期取运行层覆盖的 1h，而不是部署层的 6h")
	require.InDelta(t, zhipuSignReconcileTestSignedRate, result.EffectiveRate, 1e-9)
	require.InDelta(t, 0.5, result.Threshold, 1e-9)
	require.True(t, result.Deviation, "偏离判定用生效阈值 0.5，而不是内置默认 0.70")
}

func TestZhipuSignReconcilerSkipsZeroUsageAndUnknownModel(t *testing.T) {
	cases := []struct {
		name       string
		baseline   []domain.MonitorQuotaModelCredit
		current    []domain.MonitorQuotaModelCredit
		wantReason string
	}{
		{
			name:       "empty_window_has_no_delta",
			baseline:   zhipuSignReconcileTestRows(0, 0, 0, 0),
			current:    zhipuSignReconcileTestRows(0, 0, 0, 0),
			wantReason: ZhipuSignReconcileReasonNoUsage,
		},
		{
			name:     "unknown_model_has_no_cost_basis",
			baseline: []domain.MonitorQuotaModelCredit{{Model: "glm-9.9-unknown", Date: zhipuSignReconcileTestDay}},
			current: []domain.MonitorQuotaModelCredit{{
				Model: "glm-9.9-unknown", Date: zhipuSignReconcileTestDay,
				InputTokens: 10000, Credits: 2.0,
			}},
			wantReason: ZhipuSignReconcileReasonNoExpectedCost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
			accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
				zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
			}}
			usage := newZhipuSignReconcileFakeUsage()
			usage.push(2701, tc.baseline, nil)
			usage.push(2701, tc.current, nil)
			sink := &zhipuSignReconcileFakeSink{}
			reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

			zhipuSignReconcileTestBaseline(t, reconciler)
			clock.advance(6 * time.Hour)
			result, applied := zhipuSignReconcileTestRun(t, reconciler)

			require.False(t, applied)
			require.Equal(t, tc.wantReason, result.Reason)
			require.Zero(t, result.EffectiveRate, "跳过时不得上报 0 充当系数")
			require.Empty(t, sink.values())
		})
	}
}

func TestZhipuSignReconcilerSkipsNonFiniteReadings(t *testing.T) {
	cases := []struct {
		name string
		rows []domain.MonitorQuotaModelCredit
	}{
		{name: "nan_credits", rows: zhipuSignReconcileTestRows(math.NaN(), 0, 0, math.NaN())},
		{name: "inf_tokens_and_credits", rows: zhipuSignReconcileTestRows(math.Inf(1), 0, 0, math.Inf(1))},
		{name: "negative_delta_is_clamped", rows: zhipuSignReconcileTestRows(0, 0, 0, -3.0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
			accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
				zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
			}}
			usage := newZhipuSignReconcileFakeUsage()
			usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
			usage.push(2701, tc.rows, nil)
			sink := &zhipuSignReconcileFakeSink{}
			reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

			zhipuSignReconcileTestBaseline(t, reconciler)
			clock.advance(6 * time.Hour)
			result, applied := zhipuSignReconcileTestRun(t, reconciler)

			require.False(t, applied)
			require.Equal(t, ZhipuSignReconcileReasonNoUsage, result.Reason)
			require.Empty(t, sink.values(), "坏数据绝不进指标")
			// requireZhipuSignReconcileFinite（在 TestRun 内）已断言无 NaN/Inf。
		})
	}
}

func TestZhipuSignReconcilerAccountListFailureKeepsLastValue(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(6 * time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, result.EffectiveRate, 1e-9)

	accounts.err = errors.New("account repo unavailable")
	clock.advance(6 * time.Hour)
	result, applied = zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied)
	require.Equal(t, ZhipuSignReconcileReasonAccountListError, result.Reason)
	require.True(t, result.Stale)
	require.Len(t, sink.values(), 1, "失败轮不得改写已上报的有效值")
}

func TestZhipuSignReconcilerUpstreamFailureKeepsLastValueAndMarksStale(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(6 * time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)
	goodWindowEnd := usage.callLog()[1].end

	// 上游错误：保留上次有效值、标记陈旧（不写 0、不上报）。
	usage.push(2701, nil, errors.New("zhipu credit usage: upstream request failed"))
	clock.advance(6 * time.Hour)
	result, applied = zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied)
	require.Equal(t, ZhipuSignReconcileReasonUpstreamError, result.Reason)
	require.True(t, result.Stale)
	require.Len(t, sink.values(), 1)

	display, ok := reconciler.LastResult()
	require.True(t, ok)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, display.EffectiveRate, 1e-9, "陈旧的是上次的有效值，不是 0")
	require.True(t, display.Stale)
	requireZhipuSignReconcileSameInstant(t, goodWindowEnd, display.WindowEnd, "保留值仍指向它自己的结算窗口")

	// 基线未被失败轮推进：下一轮窗口仍从上一成功窗口末端接起。读数是累计口径，
	// 重试轮必须超过基线（10000）才有增量——推 20000（tokens/credits 同比放大，
	// 有效系数不变）。
	clock.advance(time.Hour)
	usage.push(2701, zhipuSignReconcileTestRows(20000, 0, 0, zhipuSignReconcileTestSignedCredits*2), nil)
	result, applied = zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)
	calls := usage.callLog()
	requireZhipuSignReconcileSameInstant(t, goodWindowEnd, calls[len(calls)-1].start, "失败轮不推进基线")
}

func TestZhipuSignReconcilerWithoutBaselineNeverReports(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, nil, errors.New("zhipu credit usage: upstream request failed"))
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied)
	require.Equal(t, ZhipuSignReconcileReasonUpstreamError, result.Reason)
	require.False(t, result.Stale, "从未有过有效值时没有「陈旧」可言")
	require.Empty(t, sink.values())
	_, ok := reconciler.LastResult()
	require.False(t, ok)
}

// ---------------------------------------------------------------- 多账号聚合与端到端

func TestZhipuSignReconcilerAggregatesSignedAccounts(t *testing.T) {
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
		zhipuSignReconcileTestAccount(2702, "bm-token-2702"),
		*zhipuMonitorManagedAccount(2703, "bm-token-2703"), // 未开签名灰度，不参与
	}}
	usage := newZhipuSignReconcileFakeUsage()
	// A：10000 输入 token（基准 6.9；期望 6.9×0.5×0.67 = 2.3115），
	// 读数增量积分 = 2.3115×0.67 = 1.548705。
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	// B：20000 glm-5.3-flash 输入 token（基准 20000×2.3/1e4 = 4.6；期望 4.6×0.5×0.67 = 1.541），
	// 读数增量积分 = 1.541×0.67 = 1.03247。
	usage.push(2702, zhipuSignReconcileTestFlashRows(0, 0), nil)
	usage.push(2702, zhipuSignReconcileTestFlashRows(20000, 1.03247), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(6 * time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)

	require.True(t, applied)
	require.Equal(t, 2, result.Accounts)
	require.InDelta(t, 3.8525, result.ExpectedCredits, 1e-9) // 2.3115 + 1.541
	require.InDelta(t, 2.581175, result.ActualCredits, 1e-9) // 1.548705 + 1.03247
	require.InDelta(t, 0.67, result.EffectiveRate, 1e-9)

	calls := usage.callLog()
	require.Len(t, calls, 4, "两个启用签名的账号各读一次基线一次增量；未启用的账号不读")
	require.Equal(t, []int64{2701, 2702, 2701, 2702},
		[]int64{calls[0].accountID, calls[1].accountID, calls[2].accountID, calls[3].accountID})
}

func TestZhipuSignReconcilerFeedsOpsAlertMetricSource(t *testing.T) {
	cases := []struct {
		name          string
		actualCredits float64
		wantRate      float64
		wantAlert     bool
	}{
		{name: "signed_target_is_healthy", actualCredits: zhipuSignReconcileTestSignedCredits, wantRate: 0.67, wantAlert: false},
		{name: "silent_unsigned_billing_alerts", actualCredits: zhipuSignReconcileTestExpectedCredits, wantRate: 1.0, wantAlert: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := zhipuSignReconcileTestConfig()
			clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
			// 真实上报口（#24）：对账结果 → RecordEffectiveRate → #25 的指标源。
			alerts := NewZhipuSignAlerts(cfg, nil, nil, clock.now)
			accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
				zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
			}}
			usage := newZhipuSignReconcileFakeUsage()
			usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
			usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, tc.actualCredits), nil)
			reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, alerts, cfg)

			zhipuSignReconcileTestBaseline(t, reconciler)
			clock.advance(6 * time.Hour)
			result, applied := zhipuSignReconcileTestRun(t, reconciler)
			require.True(t, applied)
			require.InDelta(t, tc.wantRate, result.EffectiveRate, 1e-9)

			// 1）引擎存活值：最近一次对账上报。
			rate, ok := alerts.EffectiveRate(context.Background())
			require.True(t, ok)
			require.InDelta(t, tc.wantRate, rate, 1e-9)

			// 2）#25 评估器的取值路径（内置指标类型 → 指标源 → 告警判定）。
			evaluator := &OpsAlertEvaluatorService{}
			evaluator.SetZhipuSignMetrics(alerts)
			value, ok := evaluator.zhipuSignEffectiveRateMetric(context.Background())
			require.True(t, ok)
			require.InDelta(t, tc.wantRate, value, 1e-9)
			metric, found := OpsBuiltinAlertMetricFor(OpsMetricTypeZhipuSignEffectiveRate)
			require.True(t, found)
			require.Equal(t, tc.wantAlert, compareMetric(value, metric.Operator, metric.Threshold))
		})
	}
}

// ---------------------------------------------------------------- 快照与 JSON 契约

// zhipuSignReconcileTestRanReconciler 返回一个已跑完「基线 + 增量」的对账器
// （0.67 命中，上报一次）。usage 一并返回：调用方要驱动下一轮对账时需先 push
// 超过基线的累计读数（读数为累计口径，不 push 新读数则窗口增量为 0、按 NoUsage 跳过）。
func zhipuSignReconcileTestRanReconciler(t *testing.T) (*ZhipuSignReconciler, *zhipuSignAlertClock, *zhipuSignReconcileFakeSink, *zhipuSignReconcileFakeUsage) {
	t.Helper()
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestSignedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)
	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(6 * time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, result.EffectiveRate, 1e-9)
	return reconciler, clock, sink, usage
}

func TestZhipuAccountMonitorServiceRunDueSignReconcile(t *testing.T) {
	service := NewZhipuAccountMonitorService(nil, nil, zhipuSignReconcileTestConfig())
	require.NotPanics(t, func() { service.RunDueSignReconcile(context.Background()) },
		"未接线时周期入口是空操作")
	_, ok := service.SignReconcileResult()
	require.False(t, ok)

	reconciler, clock, sink, usage := zhipuSignReconcileTestRanReconciler(t)
	service.SetSignReconciler(reconciler)

	// 跑完一轮「基线 + 增量」后立刻再调用（未到期）不得重复上报。
	service.RunDueSignReconcile(context.Background())
	require.Len(t, sink.values(), 1)

	// 到期后由同一入口完成下一轮对账：先推超过基线的累计读数（同比放大）。
	clock.advance(6 * time.Hour)
	usage.push(2701, zhipuSignReconcileTestRows(20000, 0, 0, zhipuSignReconcileTestSignedCredits*2), nil)
	service.RunDueSignReconcile(context.Background())
	require.Len(t, sink.values(), 2, "到期后由同一入口完成下一轮对账")

	result, ok := service.SignReconcileResult()
	require.True(t, ok)
	// 第二轮窗口 [T0+6h-11min, T0+12h-11min] 跨峰谷边界（周二 11:49→17:49 UTC+8，
	// 逐分钟均值系数≈0.83），速率按口径低于首轮全谷窗口的 0.67（首轮已锚定精确值）；
	// 这里用同一成本模型推导第二窗口的期望成本再断言，不引入第二套口径。
	delta := zhipuSignReconcileDelta{model: ZhipuModelGLM53, input: 10000}
	windowStart := zhipuSignReconcileTestT0.Add(6*time.Hour - zhipuSignReconcileSettlementDelay)
	windowEnd := zhipuSignReconcileTestT0.Add(12*time.Hour - zhipuSignReconcileSettlementDelay)
	expected2 := zhipuSignReconcileMeanEffectiveCost(delta, windowStart, windowEnd)
	require.InDelta(t, zhipuSignReconcileTestSignedCredits/expected2, result.EffectiveRate, 1e-9)
}

func TestZhipuAccountMonitorServiceApplySignReconcileSnapshot(t *testing.T) {
	service := NewZhipuAccountMonitorService(nil, nil, zhipuSignReconcileTestConfig())
	require.NotPanics(t, func() { service.ApplySignReconcileSnapshot(nil) })

	// 未接线 / 无结果：快照保持原样（老历史行与既有消费方零变化）。
	untouched := &domain.MonitorQuotaSnapshot{Source: "cn_quota", Success: true}
	service.ApplySignReconcileSnapshot(untouched)
	require.Zero(t, untouched.SignEffectiveRate)
	require.Nil(t, untouched.SignReconciledAt)
	require.False(t, untouched.SignReconcileStale)
	require.False(t, untouched.SignReconcileDeviation)

	reconciler, _, _, _ := zhipuSignReconcileTestRanReconciler(t)
	service.SetSignReconciler(reconciler)

	snapshot := &domain.MonitorQuotaSnapshot{Source: "cn_quota", Success: true}
	service.ApplySignReconcileSnapshot(snapshot)
	require.InDelta(t, zhipuSignReconcileTestSignedRate, snapshot.SignEffectiveRate, 1e-9)
	require.Equal(t, ZhipuOffPeakFactorValue, snapshot.SignPeakFactor)
	require.NotNil(t, snapshot.SignReconciledAt)
	requireZhipuSignReconcileSameInstant(t,
		zhipuSignReconcileTestT0.Add(6*time.Hour-zhipuSignReconcileTestSettlement), *snapshot.SignReconciledAt,
		"快照带出的是有效系数对应的结算窗口末端")
	require.False(t, snapshot.SignReconcileStale)
	require.False(t, snapshot.SignReconcileDeviation)

	// JSON 契约（#30 的字段名）。
	payload, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.InDelta(t, zhipuSignReconcileTestSignedRate, decoded["sign_effective_rate"], 1e-9)
	require.InDelta(t, ZhipuOffPeakFactorValue, decoded["sign_peak_factor"], 1e-9)
	require.Equal(t,
		zhipuSignReconcileTestT0.Add(6*time.Hour-zhipuSignReconcileTestSettlement).Format(time.RFC3339),
		decoded["sign_reconciled_at"])
	require.NotContains(t, decoded, "sign_reconcile_stale")
	require.NotContains(t, decoded, "sign_reconcile_deviation")

	// 零值快照：五个键全部省略。
	empty, err := json.Marshal(domain.MonitorQuotaSnapshot{Source: "cn_quota"})
	require.NoError(t, err)
	require.NotContains(t, string(empty), "sign_")
}

func TestMonitorQuotaSnapshotDecodesLegacyPayloadWithoutSignFields(t *testing.T) {
	legacy := []byte(`{"source":"cn_quota","success":true,"tiers":[{"window":"5h","used_percent":42}]}`)
	var snapshot domain.MonitorQuotaSnapshot
	require.NoError(t, json.Unmarshal(legacy, &snapshot))
	require.Equal(t, "cn_quota", snapshot.Source)
	require.True(t, snapshot.Success)
	require.Zero(t, snapshot.SignEffectiveRate)
	require.Zero(t, snapshot.SignPeakFactor)
	require.Nil(t, snapshot.SignReconciledAt)
	require.False(t, snapshot.SignReconcileStale)
	require.False(t, snapshot.SignReconcileDeviation)
}

func TestZhipuSignReconcilerSnapshotCarriesDeviationAndStale(t *testing.T) {
	// 偏离（1.0）→ 快照带 sign_reconcile_deviation；失败轮 → 带 sign_reconcile_stale。
	clock := newZhipuSignAlertClock(zhipuSignReconcileTestT0)
	accounts := &zhipuSignReconcileFakeAccounts{accounts: []Account{
		zhipuSignReconcileTestAccount(2701, "bm-token-2701"),
	}}
	usage := newZhipuSignReconcileFakeUsage()
	usage.push(2701, zhipuSignReconcileTestRows(0, 0, 0, 0), nil)
	usage.push(2701, zhipuSignReconcileTestRows(10000, 0, 0, zhipuSignReconcileTestExpectedCredits), nil)
	sink := &zhipuSignReconcileFakeSink{}
	reconciler := zhipuSignReconcileTestReconciler(clock, accounts, usage, sink, nil)
	service := NewZhipuAccountMonitorService(nil, nil, zhipuSignReconcileTestConfig())
	service.SetSignReconciler(reconciler)

	zhipuSignReconcileTestBaseline(t, reconciler)
	clock.advance(6 * time.Hour)
	result, applied := zhipuSignReconcileTestRun(t, reconciler)
	require.True(t, applied)
	require.InDelta(t, 1.0, result.EffectiveRate, 1e-9)
	require.True(t, result.Deviation)

	deviationSnapshot := &domain.MonitorQuotaSnapshot{}
	service.ApplySignReconcileSnapshot(deviationSnapshot)
	require.True(t, deviationSnapshot.SignReconcileDeviation)
	require.False(t, deviationSnapshot.SignReconcileStale)

	usage.push(2701, nil, errors.New("zhipu credit usage: upstream request failed"))
	clock.advance(6 * time.Hour)
	result, applied = zhipuSignReconcileTestRun(t, reconciler)
	require.False(t, applied)
	require.True(t, result.Stale)

	staleSnapshot := &domain.MonitorQuotaSnapshot{}
	service.ApplySignReconcileSnapshot(staleSnapshot)
	require.True(t, staleSnapshot.SignReconcileStale)
	require.InDelta(t, 1.0, staleSnapshot.SignEffectiveRate, 1e-9, "陈旧时保留上次有效值")
}
