//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var _ OpsRepository = (*stubOpsRepo)(nil)

type stubOpsRepo struct {
	OpsRepository
	overview *OpsDashboardOverview
	err      error
}

func (s *stubOpsRepo) GetDashboardOverview(ctx context.Context, filter *OpsDashboardFilter) (*OpsDashboardOverview, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.overview != nil {
		return s.overview, nil
	}
	return &OpsDashboardOverview{}, nil
}

func TestComputeGroupAvailableRatio(t *testing.T) {
	t.Parallel()

	t.Run("正常情况: 10个账号, 8个可用 = 80%", func(t *testing.T) {
		t.Parallel()

		got := computeGroupAvailableRatio(&GroupAvailability{
			TotalAccounts:  10,
			AvailableCount: 8,
		})
		require.InDelta(t, 80.0, got, 0.0001)
	})

	t.Run("边界情况: TotalAccounts = 0 应返回 0", func(t *testing.T) {
		t.Parallel()

		got := computeGroupAvailableRatio(&GroupAvailability{
			TotalAccounts:  0,
			AvailableCount: 8,
		})
		require.Equal(t, 0.0, got)
	})

	t.Run("边界情况: AvailableCount = 0 应返回 0%", func(t *testing.T) {
		t.Parallel()

		got := computeGroupAvailableRatio(&GroupAvailability{
			TotalAccounts:  10,
			AvailableCount: 0,
		})
		require.Equal(t, 0.0, got)
	})
}

func TestCountAccountsByCondition(t *testing.T) {
	t.Parallel()

	t.Run("测试限流账号统计: acc.IsRateLimited", func(t *testing.T) {
		t.Parallel()

		accounts := map[int64]*AccountAvailability{
			1: {IsRateLimited: true},
			2: {IsRateLimited: false},
			3: {IsRateLimited: true},
		}

		got := countAccountsByCondition(accounts, func(acc *AccountAvailability) bool {
			return acc.IsRateLimited
		})
		require.Equal(t, int64(2), got)
	})

	t.Run("测试错误账号统计（排除临时不可调度）: acc.HasError && acc.TempUnschedulableUntil == nil", func(t *testing.T) {
		t.Parallel()

		until := time.Now().UTC().Add(5 * time.Minute)
		accounts := map[int64]*AccountAvailability{
			1: {HasError: true},
			2: {HasError: true, TempUnschedulableUntil: &until},
			3: {HasError: false},
		}

		got := countAccountsByCondition(accounts, func(acc *AccountAvailability) bool {
			return acc.HasError && acc.TempUnschedulableUntil == nil
		})
		require.Equal(t, int64(1), got)
	})

	t.Run("边界情况: 空 map 应返回 0", func(t *testing.T) {
		t.Parallel()

		got := countAccountsByCondition(map[int64]*AccountAvailability{}, func(acc *AccountAvailability) bool {
			return acc.IsRateLimited
		})
		require.Equal(t, int64(0), got)
	})
}

// TestComputeRuleMetric_AccountTempUnscheduledCount verifies the new
// account_temp_unscheduled_count metric counts accounts currently in the
// temp-unscheduled window and ignores those whose window has expired or
// were never temp-unscheduled.
func TestComputeRuleMetric_AccountTempUnscheduledCount(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	futureUntil := now.Add(5 * time.Minute)
	pastUntil := now.Add(-1 * time.Minute)

	availability := &OpsAccountAvailability{
		Accounts: map[int64]*AccountAvailability{
			// currently temp-unscheduled (window active)
			1: {TempUnschedulableUntil: &futureUntil},
			2: {TempUnschedulableUntil: &futureUntil},
			// temp-unsched window already expired → should NOT count
			3: {TempUnschedulableUntil: &pastUntil},
			// never temp-unscheduled
			4: {HasError: true},
			5: {IsRateLimited: true},
		},
	}

	opsService := &OpsService{
		getAccountAvailability: func(_ context.Context, _ string, _ *int64) (*OpsAccountAvailability, error) {
			return availability, nil
		},
	}
	svc := &OpsAlertEvaluatorService{
		opsService: opsService,
		opsRepo:    &stubOpsRepo{},
	}

	rule := &OpsAlertRule{MetricType: "account_temp_unscheduled_count"}
	val, ok := svc.computeRuleMetric(context.Background(), rule, nil,
		now.Add(-5*time.Minute), now, "", nil)

	require.True(t, ok)
	require.InDelta(t, 2.0, val, 0.0001, "only 2 accounts have an active temp-unsched window")
}

func TestComputeRuleMetricNewIndicators(t *testing.T) {
	t.Parallel()

	groupID := int64(101)
	platform := "openai"

	availability := &OpsAccountAvailability{
		Group: &GroupAvailability{
			GroupID:        groupID,
			TotalAccounts:  10,
			AvailableCount: 8,
		},
		Accounts: map[int64]*AccountAvailability{
			1: {IsRateLimited: true},
			2: {IsRateLimited: true},
			3: {HasError: true},
			4: {HasError: true, TempUnschedulableUntil: timePtr(time.Now().UTC().Add(2 * time.Minute))},
			5: {HasError: false, IsRateLimited: false},
		},
	}

	opsService := &OpsService{
		getAccountAvailability: func(_ context.Context, _ string, _ *int64) (*OpsAccountAvailability, error) {
			return availability, nil
		},
	}

	svc := &OpsAlertEvaluatorService{
		opsService: opsService,
		opsRepo:    &stubOpsRepo{overview: &OpsDashboardOverview{}},
	}

	start := time.Now().UTC().Add(-5 * time.Minute)
	end := time.Now().UTC()
	ctx := context.Background()

	tests := []struct {
		name       string
		metricType string
		groupID    *int64
		wantValue  float64
		wantOK     bool
	}{
		{
			name:       "group_available_accounts",
			metricType: "group_available_accounts",
			groupID:    &groupID,
			wantValue:  8,
			wantOK:     true,
		},
		{
			name:       "group_available_ratio",
			metricType: "group_available_ratio",
			groupID:    &groupID,
			wantValue:  80.0,
			wantOK:     true,
		},
		{
			name:       "account_rate_limited_count",
			metricType: "account_rate_limited_count",
			groupID:    nil,
			wantValue:  2,
			wantOK:     true,
		},
		{
			name:       "account_error_count",
			metricType: "account_error_count",
			groupID:    nil,
			wantValue:  1,
			wantOK:     true,
		},
		{
			name:       "group_available_accounts without group_id returns false",
			metricType: "group_available_accounts",
			groupID:    nil,
			wantValue:  0,
			wantOK:     false,
		},
		{
			name:       "group_available_ratio without group_id returns false",
			metricType: "group_available_ratio",
			groupID:    nil,
			wantValue:  0,
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rule := &OpsAlertRule{
				MetricType: tt.metricType,
			}
			gotValue, gotOK := svc.computeRuleMetric(ctx, rule, nil, start, end, platform, tt.groupID)
			require.Equal(t, tt.wantOK, gotOK)
			if !tt.wantOK {
				return
			}
			require.InDelta(t, tt.wantValue, gotValue, 0.0001)
		})
	}
}

// ---------------------------------------------------------------------------
// 票 25：智谱签名内置指标（L1 zhipu_sign_fail_window / L2 zhipu_sign_effective_rate）
// 接入既有 ops 告警框架（design M3.1(b)(d)）。
//
// 断言落在既有接缝上：
//   - computeRuleMetric：评估周期唯一的指标取值入口（含 sign_alert_enabled 短路）；
//   - OpsBuiltinAlertRule + compareMetric：默认规则参数与阈值语义（0 / 0.70 边界）；
//   - evaluateOnce：评估周期的单次执行（规则 → 静默/冷却 → OpsAlertEvent 落库）；
//   - *ZhipuSignAlerts 的公开访问器：计数与有效系数的采集口径。
// ---------------------------------------------------------------------------

const zhipuSignAlertEvalRuleID = 6401

// zhipuSignAlertEvalHarness 是「真实 L1/L2 引擎 + 评估器 + 可观测 OpsRepository」的最小
// 装配，与 wire 的 SetZhipuSignMetrics 接线同款。计数器不给 Redis：引擎退化为进程内存，
// 正是本用例覆盖的路径；时钟固定在 5 分钟桶边界上，桶归属完全确定。
type zhipuSignAlertEvalHarness struct {
	evaluator *OpsAlertEvaluatorService
	alerts    *ZhipuSignAlerts
	repo      *opsRepoMock
	rule      *OpsAlertRule
	created   []*OpsAlertEvent

	// silenced 由 IsAlertSilenced 钩子读取；silencePlatforms 记录平台维度实参，
	// 用于断言「platform=zhipu 的静默」确实走到了既有静默查询。
	silenced         bool
	silencePlatforms []string
}

func newZhipuSignAlertEvalHarness(t *testing.T, metricType string, alertEnabled bool) *zhipuSignAlertEvalHarness {
	t.Helper()

	rule, ok := OpsBuiltinAlertRule(metricType)
	require.Truef(t, ok, "expected a builtin rule template for %s", metricType)
	rule.ID = zhipuSignAlertEvalRuleID
	// 规则里的窗口故意写成 1 分钟：内置指标的统计窗口由指标自身决定（L1 = 5 分钟桶、
	// L2 = 对账周期），文案必须按指标口径渲染而不是按规则里可能写错的值。
	rule.WindowMinutes = 1

	harness := &zhipuSignAlertEvalHarness{rule: rule}
	harness.repo = &opsRepoMock{
		ListAlertRulesFn: func(context.Context) ([]*OpsAlertRule, error) {
			return []*OpsAlertRule{harness.rule}, nil
		},
		CreateAlertEventFn: func(_ context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error) {
			created := *event
			created.ID = int64(len(harness.created) + 1)
			harness.created = append(harness.created, &created)
			return &created, nil
		},
		IsAlertSilencedFn: func(_ context.Context, _ int64, platform string, _ *int64, _ *string, _ time.Time) (bool, error) {
			harness.silencePlatforms = append(harness.silencePlatforms, platform)
			return harness.silenced, nil
		},
	}

	configSource := newZhipuSignAlertConfigStub()
	configSource.setSignAlertEnabled(alertEnabled)
	harness.alerts = NewZhipuSignAlerts(nil, nil, configSource, newZhipuSignAlertClock(zhipuSignAlertTestBase).now)

	harness.evaluator = &OpsAlertEvaluatorService{
		opsService:   &OpsService{opsRepo: harness.repo},
		opsRepo:      harness.repo,
		emailLimiter: newSlidingWindowLimiter(0, time.Hour),
		ruleStates:   map[int64]*opsAlertRuleState{},
	}
	harness.evaluator.SetZhipuSignMetrics(harness.alerts)
	return harness
}

// evaluate 执行评估周期的单次执行（既有 evaluateOnce 接缝，与后台 loop 同款）。
func (h *zhipuSignAlertEvalHarness) evaluate() {
	h.evaluator.evaluateOnce(time.Minute)
}

func (h *zhipuSignAlertEvalHarness) createdEvents() []*OpsAlertEvent {
	return append([]*OpsAlertEvent(nil), h.created...)
}

// TestOpsAlertEvaluatorZhipuSignMetricSources 覆盖指标源适配（表驱动）：
// L1 = 三类计数之和；L2 = 票 27 上报的有效系数；sign_alert_enabled 短路但指标仍采集。
func TestOpsAlertEvaluatorZhipuSignMetricSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Now().UTC().Add(-5 * time.Minute)
	end := time.Now().UTC()

	t.Run("L1 失效窗口 = 验签失效 + 握手失败 + 降级发送", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, true)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordSignFailure(ctx, zhipuSignAlertTestAccountID, errors.New("handshake failed"))
		h.alerts.RecordFailOpen(ctx, zhipuSignAlertTestAccountID+1)
		// 自愈观测量（replay_ok）不属于失效窗口，不得计入。
		h.alerts.RecordReplay(ctx, zhipuSignAlertTestAccountID, true)

		value, ok := h.evaluator.computeRuleMetric(ctx, h.rule, nil, start, end, "zhipu", nil)
		require.True(t, ok)
		require.InDelta(t, 4.0, value, 0.0001)
	})

	t.Run("L1 窗口无计数：指标可计算且为 0，默认阈值下不触发", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, true)

		value, ok := h.evaluator.computeRuleMetric(ctx, h.rule, nil, start, end, "zhipu", nil)
		require.True(t, ok)
		require.InDelta(t, 0.0, value, 0.0001)
		require.False(t, compareMetric(value, h.rule.Operator, h.rule.Threshold))
	})

	t.Run("sign_alert_enabled=false：两个指标都不评估，但计数与有效系数仍采集", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, false)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)

		_, ok := h.evaluator.computeRuleMetric(ctx, h.rule, nil, start, end, "zhipu", nil)
		require.False(t, ok, "告警开关关闭时 L1 指标不参与评估")
		require.Equal(t, int64(1), h.alerts.CounterSnapshot(ctx, ZhipuSignMetricVerifyInvalid).Global,
			"告警开关不改变采集：计数器仍在累计")

		h.alerts.RecordEffectiveRate(ctx, 1.0)
		rateRule, ok := OpsBuiltinAlertRule(OpsMetricTypeZhipuSignEffectiveRate)
		require.True(t, ok)
		_, ok = h.evaluator.computeRuleMetric(ctx, rateRule, nil, start, end, "zhipu", nil)
		require.False(t, ok, "告警开关关闭时 L2 指标不参与评估")
		rate, reported := h.alerts.EffectiveRate(ctx)
		require.True(t, reported, "告警开关不改变采集：有效系数仍被记录")
		require.InDelta(t, 1.0, rate, 1e-9)
	})

	t.Run("未接线（指标源为 nil）：两个内置指标都不可计算", func(t *testing.T) {
		t.Parallel()
		svc := &OpsAlertEvaluatorService{}
		_, ok := svc.computeRuleMetric(ctx, &OpsAlertRule{MetricType: OpsMetricTypeZhipuSignFailWindow}, nil, start, end, "zhipu", nil)
		require.False(t, ok)
		_, ok = svc.computeRuleMetric(ctx, &OpsAlertRule{MetricType: OpsMetricTypeZhipuSignEffectiveRate}, nil, start, end, "zhipu", nil)
		require.False(t, ok)
	})

	t.Run("L2 未上报不可计算，上报后取最近一次值", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignEffectiveRate, true)
		rateRule, ok := OpsBuiltinAlertRule(OpsMetricTypeZhipuSignEffectiveRate)
		require.True(t, ok)

		_, ok = h.evaluator.computeRuleMetric(ctx, rateRule, nil, start, end, "zhipu", nil)
		require.False(t, ok, "从未对账就不评估，不能用 0 冒充「签名未生效」")

		h.alerts.RecordEffectiveRate(ctx, 0.67)
		value, ok := h.evaluator.computeRuleMetric(ctx, rateRule, nil, start, end, "zhipu", nil)
		require.True(t, ok)
		require.InDelta(t, 0.67, value, 1e-9)
		require.False(t, compareMetric(value, rateRule.Operator, rateRule.Threshold), "0.67 是签名生效的基准")

		h.alerts.RecordEffectiveRate(ctx, 1.0)
		value, ok = h.evaluator.computeRuleMetric(ctx, rateRule, nil, start, end, "zhipu", nil)
		require.True(t, ok)
		require.InDelta(t, 1.0, value, 1e-9)
		require.True(t, compareMetric(value, rateRule.Operator, rateRule.Threshold), "1.0 = 签名被静默按无签名计费")
	})
}

// TestOpsBuiltinAlertRuleDefaultsForZhipuSign 覆盖默认规则参数、默认文案与阈值边界。
func TestOpsBuiltinAlertRuleDefaultsForZhipuSign(t *testing.T) {
	t.Parallel()

	failRule, ok := OpsBuiltinAlertRule(OpsMetricTypeZhipuSignFailWindow)
	require.True(t, ok)
	rateRule, ok := OpsBuiltinAlertRule(OpsMetricTypeZhipuSignEffectiveRate)
	require.True(t, ok)

	// L1：出现即告警（threshold=0）、5 分钟窗口、platform=zhipu 维度（静默与过滤共用）。
	require.Equal(t, ">", failRule.Operator)
	require.Equal(t, 0.0, failRule.Threshold)
	require.Equal(t, 5, failRule.WindowMinutes)
	require.Equal(t, "P1", failRule.Severity)
	require.True(t, failRule.NotifyEmail)
	require.Equal(t, "zhipu", failRule.Filters["platform"])
	require.Equal(t, OpsMetricTypeZhipuSignFailWindow, failRule.MetricType)

	// L2：> 0.70，窗口按对账周期（默认 6h）。
	require.Equal(t, ">", rateRule.Operator)
	require.InDelta(t, 0.70, rateRule.Threshold, 1e-9)
	require.Equal(t, 360, rateRule.WindowMinutes)
	require.Equal(t, "P1", rateRule.Severity)
	require.True(t, rateRule.NotifyEmail)
	require.Equal(t, "zhipu", rateRule.Filters["platform"])

	// 文案：给出建议动作，且只提配置键，不暴露任何凭据。
	for _, rule := range []*OpsAlertRule{failRule, rateRule} {
		require.Contains(t, rule.Name, "智谱")
		require.NotEmpty(t, rule.Description)

		metric, found := OpsBuiltinAlertMetricFor(rule.MetricType)
		require.True(t, found)
		require.Contains(t, metric.SuggestedAction, "X-Client-Version")
		require.Contains(t, metric.SuggestedAction, "sign_v4_enabled")
		for _, forbidden := range []string{"api_key", "Authorization", "Bearer", "sk-"} {
			require.NotContains(t, metric.SuggestedAction, forbidden)
			require.NotContains(t, metric.Description, forbidden)
		}
	}

	// 事件文案 = 通用描述 + 建议动作；窗口按指标口径而不是规则里的 window_minutes。
	failDescription := buildOpsAlertEventDescription(failRule, 2, opsAlertEffectiveWindowMinutes(failRule), "zhipu", nil)
	require.Contains(t, failDescription, "zhipu_sign_fail_window > 0.00 (current 2.00) over last 5m (platform=zhipu)")
	require.Contains(t, failDescription, "建议动作")
	require.Contains(t, failDescription, "X-Client-Version")

	rateDescription := buildOpsAlertEventDescription(rateRule, 1, opsAlertEffectiveWindowMinutes(rateRule), "zhipu", nil)
	require.Contains(t, rateDescription, "over last 360m")
	require.Contains(t, rateDescription, "建议动作")

	// 规则里的 window_minutes 不改变内置指标的统计窗口。
	override := *failRule
	override.WindowMinutes = 1
	require.Equal(t, 5, opsAlertEffectiveWindowMinutes(&override))
	require.Equal(t, 360, opsAlertEffectiveWindowMinutes(rateRule))

	// 阈值语义（0 与 0.70 的边界）。
	thresholds := []struct {
		name      string
		value     float64
		rule      *OpsAlertRule
		wantFired bool
	}{
		{"L1 窗口计数 0 不告警", 0, failRule, false},
		{"L1 窗口计数 1 出现即告警", 1, failRule, true},
		{"L2 有效系数 0.67 命中基准不告警", 0.67, rateRule, false},
		{"L2 有效系数等于阈值 0.70 不告警（严格大于）", 0.70, rateRule, false},
		{"L2 有效系数 0.71 告警", 0.71, rateRule, true},
		{"L2 有效系数 1.0 告警（静默按无签名计费）", 1.0, rateRule, true},
	}
	for _, tc := range thresholds {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.wantFired, compareMetric(tc.value, tc.rule.Operator, tc.rule.Threshold))
		})
	}

	// 默认规则模板每次返回新副本：调用方改动不会污染注册表。
	template, ok := OpsBuiltinAlertRule(OpsMetricTypeZhipuSignFailWindow)
	require.True(t, ok)
	template.ID = 7
	template.Threshold = 99
	template.Filters["platform"] = "other"
	again, ok := OpsBuiltinAlertRule(OpsMetricTypeZhipuSignFailWindow)
	require.True(t, ok)
	require.Equal(t, int64(0), again.ID)
	require.Equal(t, 0.0, again.Threshold)
	require.Equal(t, "zhipu", again.Filters["platform"])
	require.Len(t, OpsBuiltinAlertRules(), 2)
}

// TestOpsAlertEvaluatorZhipuSignFiresOpsAlertEvent 是端到端用例：注入计数/有效系数 →
// 单次评估 → OpsAlertEvent 落库（含静默、冷却、开关与无数据路径）。
func TestOpsAlertEvaluatorZhipuSignFiresOpsAlertEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("L1 计数出现即告警：事件含账号维度与建议动作", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, true)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)

		h.evaluate()

		events := h.createdEvents()
		require.Len(t, events, 1)
		event := events[0]
		require.Equal(t, int64(zhipuSignAlertEvalRuleID), event.RuleID)
		require.Equal(t, OpsAlertStatusFiring, event.Status)
		require.Equal(t, "P1", event.Severity)
		require.Contains(t, event.Title, "智谱签名失效")
		require.NotNil(t, event.MetricValue)
		require.InDelta(t, 2.0, *event.MetricValue, 0.0001)
		require.NotNil(t, event.ThresholdValue)
		require.InDelta(t, 0.0, *event.ThresholdValue, 0.0001)
		require.Equal(t, "zhipu", event.Dimensions["platform"])
		require.Equal(t, []int64{zhipuSignAlertTestAccountID}, event.Dimensions["zhipu_sign_accounts"])
		require.Equal(t, zhipuSignMetricBucketID(zhipuSignAlertTestBase), event.Dimensions["zhipu_sign_window_bucket"])
		require.Contains(t, event.Description, "over last 5m")
		require.Contains(t, event.Description, "建议动作")
		require.NotContains(t, event.Description, "api_key")
		require.Equal(t, []string{"zhipu"}, h.silencePlatforms)
	})

	t.Run("窗口内无计数：不产生事件", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, true)

		h.evaluate()

		require.Empty(t, h.createdEvents())
		require.Empty(t, h.silencePlatforms, "未触发时不查询静默")
	})

	t.Run("platform=zhipu 静默生效：不产生事件", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, true)
		h.silenced = true
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)

		h.evaluate()

		require.Empty(t, h.createdEvents())
		require.Equal(t, []string{"zhipu"}, h.silencePlatforms)
	})

	t.Run("冷却窗口内不重复产生事件", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, true)
		h.alerts.RecordFailOpen(ctx, zhipuSignAlertTestAccountID)
		h.repo.GetLatestAlertEventFn = func(context.Context, int64) (*OpsAlertEvent, error) {
			return &OpsAlertEvent{ID: 99, RuleID: h.rule.ID, FiredAt: time.Now().UTC().Add(-time.Minute)}, nil
		}

		h.evaluate()

		require.Empty(t, h.createdEvents(), "上次告警在 30 分钟冷却内")
	})

	t.Run("sign_alert_enabled=false：不产生事件，计数仍采集", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignFailWindow, false)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)

		h.evaluate()

		require.Empty(t, h.createdEvents())
		require.Equal(t, int64(1), h.alerts.CounterSnapshot(ctx, ZhipuSignMetricVerifyInvalid).Global)
	})

	t.Run("L2 有效系数 1.0：产生事件（阈值 0.70，窗口按对账周期）", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignEffectiveRate, true)
		h.alerts.RecordEffectiveRate(ctx, 1.0)

		h.evaluate()

		events := h.createdEvents()
		require.Len(t, events, 1)
		require.NotNil(t, events[0].MetricValue)
		require.InDelta(t, 1.0, *events[0].MetricValue, 1e-9)
		require.NotNil(t, events[0].ThresholdValue)
		require.InDelta(t, 0.70, *events[0].ThresholdValue, 1e-9)
		require.Contains(t, events[0].Description, "over last 360m")
		require.Contains(t, events[0].Description, "建议动作")
	})

	t.Run("L2 有效系数 0.67：不产生事件", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignEffectiveRate, true)
		h.alerts.RecordEffectiveRate(ctx, 0.67)

		h.evaluate()

		require.Empty(t, h.createdEvents())
	})

	t.Run("L2 从未对账：不产生事件", func(t *testing.T) {
		t.Parallel()
		h := newZhipuSignAlertEvalHarness(t, OpsMetricTypeZhipuSignEffectiveRate, true)

		h.evaluate()

		require.Empty(t, h.createdEvents())
	})
}
