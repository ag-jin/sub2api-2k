//go:build unit

package service

// v1.2 — 503 流式断开自动续跑的重试预算/节奏设置化与默认值加大。
//
// 用户决策：只做"同模型加大重试力度"，不做模型回退、不做并行竞速。
// 生产实证（2026-10-21）：触发 113 / 救回 48 / 预算耗尽 22（reason=budget，
// continuation_index 到 3，elapsed ~5s）——次数预算先于时间窗耗尽。
//
// 覆盖四组行为：
//  1. 零配置默认值：6 次 / 150ms 起指数爬坡封顶 2s / 总预算窗 30s；
//  2. gateway 设置覆盖（自定义小预算在测试内可跑完）；
//  3. 越硬限 / 非法值逐字段回默认（回退，不是钳制）；
//  4. 放弃 reason 细分 budget_attempts / budget_time，exhausted 日志补
//     budget_left / elapsed_ms 供生产对比救回率。

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func budgetTestService(settingService *SettingService) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg:            &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		settingService: settingService,
	}
}

// --- 1. 零配置默认值 ---

func TestOpenAIStream503ContinueBudgetDefaultsApply(t *testing.T) {
	svc := budgetTestService(nil)
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	st := contTestState(t, svc, c, account)

	require.Equal(t, 6, st.EffectiveMaxContinuations(),
		"未配置时默认续跑次数上限必须是新默认 6（旧常量是 2）")
	for i := 0; i < 6; i++ {
		ok, reason := st.CanContinue(time.Now())
		require.True(t, ok, "默认预算下第 %d 次续跑应被允许: %s", i+1, reason)
		st.BeginContinuation(OpenAIUsage{}, nil)
	}
	require.Equal(t, 6, st.Continuations())

	ok, reason := st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetAttempts, reason,
		"次数耗尽的 reason 必须细分为 budget_attempts")
}

// 默认退避序列：150ms 起每次翻倍、封顶 2000ms，±20% 抖动。
// 独立来源：150/300/600/1200/2000(封顶)/2000，累计 ≈6.25s，落在 30s 总预算窗内。
func TestOpenAIStream503ContinueBackoffExponentialRamp(t *testing.T) {
	svc := budgetTestService(nil)
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	st := contTestState(t, svc, c, account)

	assertInRange := func(d time.Duration, min, max time.Duration) {
		t.Helper()
		require.GreaterOrEqual(t, d, min)
		require.LessOrEqual(t, d, max)
	}
	cases := []struct {
		attempt int
		min     time.Duration
		max     time.Duration
	}{
		{1, 120 * time.Millisecond, 180 * time.Millisecond},
		{2, 240 * time.Millisecond, 360 * time.Millisecond},
		{3, 480 * time.Millisecond, 720 * time.Millisecond},
		{4, 960 * time.Millisecond, 1440 * time.Millisecond},
		{5, 1600 * time.Millisecond, 2400 * time.Millisecond}, // 150<<4=2400 → 封顶 2000
		{6, 1600 * time.Millisecond, 2400 * time.Millisecond},
	}
	for _, tc := range cases {
		st.BeginContinuation(OpenAIUsage{}, nil)
		assertInRange(st.Backoff(), tc.min, tc.max)
	}
}

func TestOpenAIStream503ContinueBudgetTimeDefault(t *testing.T) {
	svc := budgetTestService(nil)
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)

	st := contTestState(t, svc, c, account)
	st.firstTriggerAt = time.Now().Add(-29 * time.Second)
	ok, reason := st.CanContinue(time.Now())
	require.True(t, ok, "默认 30s 预算窗内应仍可续跑: %s", reason)

	st.firstTriggerAt = time.Now().Add(-31 * time.Second)
	ok, reason = st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetTime, reason,
		"时间窗耗尽的 reason 必须细分为 budget_time")
}

// 手写构造的零值 state（历史测试/兜底路径）必须回退默认预算，不得把 0 当合法上限。
func TestOpenAIStream503ContinueBudgetZeroValueStateFallsBackToDefaults(t *testing.T) {
	st := &openAIStreamContinuationState{enabled: true}
	require.Equal(t, 6, st.EffectiveMaxContinuations())
	ok, reason := st.CanContinue(time.Now())
	require.True(t, ok, "零值预算回退默认后应可续跑: %s", reason)
}

// --- 2. 设置覆盖 ---

func TestOpenAIStream503ContinueBudgetSettingsOverride(t *testing.T) {
	repo := newMockSettingRepo()
	ctx := context.Background()
	require.NoError(t, repo.Set(ctx, SettingKeyOpenAIStream503MaxContinuations, "3"))
	require.NoError(t, repo.Set(ctx, SettingKeyOpenAIStream503BackoffInitialMs, "100"))
	require.NoError(t, repo.Set(ctx, SettingKeyOpenAIStream503TotalBudgetMs, "60000"))

	svc := budgetTestService(NewSettingService(repo, &config.Config{}))
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	st := contTestState(t, svc, c, account)

	require.Equal(t, 3, st.EffectiveMaxContinuations(), "设置覆盖必须生效")
	for i := 0; i < 3; i++ {
		ok, reason := st.CanContinue(time.Now())
		require.True(t, ok, "覆盖预算下第 %d 次续跑应被允许: %s", i+1, reason)
		st.BeginContinuation(OpenAIUsage{}, nil)
	}
	ok, reason := st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetAttempts, reason)

	// 未覆盖的字段（退避封顶）保持默认 2000ms。
	// 5 次 Begin 后 st.continuations=5，Backoff=delay(5)=100<<4=1600ms < 2000ms 封顶。
	st2 := contTestState(t, svc, c, account)
	st2.BeginContinuation(OpenAIUsage{}, nil)
	st2.BeginContinuation(OpenAIUsage{}, nil)
	st2.BeginContinuation(OpenAIUsage{}, nil)
	st2.BeginContinuation(OpenAIUsage{}, nil)
	st2.BeginContinuation(OpenAIUsage{}, nil)
	backoff := st2.Backoff()
	require.GreaterOrEqual(t, backoff, 1600*time.Millisecond)
	require.LessOrEqual(t, backoff, 2400*time.Millisecond)
}

func TestOpenAIStream503ContinueBudgetTimeSettingsOverride(t *testing.T) {
	repo := newMockSettingRepo()
	ctx := context.Background()
	// 自定义小预算窗：250ms，测试内跑得完。
	require.NoError(t, repo.Set(ctx, SettingKeyOpenAIStream503TotalBudgetMs, "250"))

	svc := budgetTestService(NewSettingService(repo, &config.Config{}))
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	st := contTestState(t, svc, c, account)

	st.BeginContinuation(OpenAIUsage{}, nil)
	st.firstTriggerAt = time.Now().Add(-400 * time.Millisecond)
	ok, reason := st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetTime, reason,
		"小预算窗配置必须让 budget_time 在测试内可触发")
}

// --- 3. 硬限 / 非法值逐字段回默认 ---

func TestNormalizeOpenAIStream503ContinueBudget(t *testing.T) {
	d := defaultOpenAIStream503ContinueBudget()

	tests := []struct {
		name   string
		values map[string]string
		want   openAIStream503ContinueBudget
	}{
		{"empty_uses_defaults", nil, d},
		{"all_valid", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "8",
			SettingKeyOpenAIStream503BackoffInitialMs: "80",
			SettingKeyOpenAIStream503BackoffMaxMs:     "5000",
			SettingKeyOpenAIStream503TotalBudgetMs:    "45000",
		}, openAIStream503ContinueBudget{8, 80 * time.Millisecond, 5 * time.Second, 45 * time.Second}},
		{"continuations_at_hard_cap", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "10",
		}, openAIStream503ContinueBudget{10, d.backoffInitial, d.backoffMax, d.totalBudget}},
		{"continuations_over_hard_cap_falls_back", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "11",
		}, d},
		{"continuations_zero_falls_back", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "0",
		}, d},
		{"continuations_negative_falls_back", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "-2",
		}, d},
		{"continuations_garbage_falls_back", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "abc",
		}, d},
		{"continuations_empty_falls_back", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "",
		}, d},
		{"backoff_initial_at_floor", map[string]string{
			SettingKeyOpenAIStream503BackoffInitialMs: "50",
		}, openAIStream503ContinueBudget{d.maxContinuations, 50 * time.Millisecond, d.backoffMax, d.totalBudget}},
		{"backoff_initial_below_floor_falls_back", map[string]string{
			SettingKeyOpenAIStream503BackoffInitialMs: "49",
		}, d},
		{"backoff_initial_over_guard_falls_back", map[string]string{
			SettingKeyOpenAIStream503BackoffInitialMs: "999999999",
		}, d},
		{"backoff_max_at_hard_cap", map[string]string{
			SettingKeyOpenAIStream503BackoffMaxMs: "10000",
		}, openAIStream503ContinueBudget{d.maxContinuations, d.backoffInitial, 10 * time.Second, d.totalBudget}},
		{"backoff_max_over_hard_cap_falls_back", map[string]string{
			SettingKeyOpenAIStream503BackoffMaxMs: "10001",
		}, d},
		{"backoff_max_zero_falls_back", map[string]string{
			SettingKeyOpenAIStream503BackoffMaxMs: "0",
		}, d},
		{"total_budget_at_hard_cap", map[string]string{
			SettingKeyOpenAIStream503TotalBudgetMs: "120000",
		}, openAIStream503ContinueBudget{d.maxContinuations, d.backoffInitial, d.backoffMax, 120 * time.Second}},
		{"total_budget_over_hard_cap_falls_back", map[string]string{
			SettingKeyOpenAIStream503TotalBudgetMs: "120001",
		}, d},
		{"total_budget_zero_falls_back", map[string]string{
			SettingKeyOpenAIStream503TotalBudgetMs: "0",
		}, d},
		{"bad_field_keeps_valid_ones", map[string]string{
			SettingKeyOpenAIStream503MaxContinuations: "7",
			SettingKeyOpenAIStream503BackoffInitialMs: "not-a-number",
			SettingKeyOpenAIStream503BackoffMaxMs:     "3000",
		}, openAIStream503ContinueBudget{7, d.backoffInitial, 3 * time.Second, d.totalBudget}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeOpenAIStream503ContinueBudget(tt.values)
			require.Equal(t, tt.want, got)
		})
	}
}

// --- 4. exhausted reason 细分 + 放弃日志字段 ---

func TestOpenAIStream503AutoContinueExhaustedLogBudgetAttempts(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := budgetTestService(nil)
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)
	cont.continuations = openAIStream503ContinueDefaultMaxContinuations

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-budget-attempts")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal, "预算耗尽时不得续跑")

	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
	require.NotNil(t, fields)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetAttempts, fields["reason"])
	require.EqualValues(t, 0, fields["budget_left"])
	require.Contains(t, fields, "elapsed_ms", "放弃日志必须保留 elapsed_ms 供生产对比救回率")
}

func TestOpenAIStream503AutoContinueExhaustedLogBudgetTime(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := budgetTestService(nil)
	c, _ := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)
	cont.BeginContinuation(OpenAIUsage{}, nil)
	cont.firstTriggerAt = time.Now().Add(-31 * time.Second)

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-budget-time")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal, "总预算窗耗尽时不得续跑")

	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
	require.NotNil(t, fields)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetTime, fields["reason"])
	require.EqualValues(t, openAIStream503ContinueDefaultMaxContinuations-1, fields["budget_left"],
		"时间窗耗尽时次数预算尚有余量")
	elapsedMS, ok := fields["elapsed_ms"].(int64)
	require.True(t, ok, "elapsed_ms 必须是数值字段")
	require.GreaterOrEqual(t, elapsedMS, int64(30000))
}
