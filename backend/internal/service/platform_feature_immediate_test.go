package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// --- 夜猫窗口跨午夜的去重口径（本轮修的真 bug）---

// Scenario：**同一个夜**里的两次 tick 必须得到同一个去重日期。
//
// 夜猫窗口 23:00–08:00 跨零点，所以 23:30 与次日 07:30 属于同一个夜。
// 修前口径用"当前日历日期"：23:30 → 09-29、07:30 → 09-30，
// 两次都认为"今天还没跑"→ **同一夜跑两轮**（探针实测确认）。
//
// 这条测试钉住修后的口径：日出前那一段归到"夜的开始日"。
func TestCodeBuddyGrowthNightAnchorDateSpansMidnight(t *testing.T) {
	cases := []struct {
		moment   string
		wantDate string
		why      string
	}{
		{"2026-09-29 22:59", "2026-09-29", "窗口前一刻（不在窗口内，但锚定仍给出该日）"},
		{"2026-09-29 23:00", "2026-09-29", "窗口起点"},
		{"2026-09-29 23:59", "2026-09-29", "午夜前"},
		{"2026-09-30 00:00", "2026-09-29", "刚过午夜 → **仍归 09-29**（同一个夜）"},
		{"2026-09-30 07:59", "2026-09-29", "窗口结束前 → 仍归 09-29"},
		{"2026-09-30 08:00", "2026-09-30", "窗口结束后 → 新的一天，锚定到当日"},
		{"2026-09-30 12:00", "2026-09-30", "日间"},
		{"2026-09-30 23:00", "2026-09-30", "下一个夜的起点"},
	}
	for _, tc := range cases {
		at := parseCST(t, strings.Split(tc.moment, " ")[0], strings.Split(tc.moment, " ")[1])
		got := codeBuddyGrowthNightAnchorDate(at)
		require.Equal(t, tc.wantDate, got, "%s：%s", tc.moment, tc.why)
	}
}

// Scenario：同一个夜的两个时刻**锚定到同一天**（这正是修 bug 的目的）。
//
// 与上一条的区别：上一条逐点核对数值，这条直接表达"两者必须相等"这个**不变量**
// ——它才是"去重生效"的充分条件，也最容易在将来被改坏。
func TestCodeBuddyGrowthNightAnchorKeepsOneNightToOneKey(t *testing.T) {
	beforeMidnight := parseCST(t, "2026-09-29", "23:30")
	afterMidnight := parseCST(t, "2026-09-30", "07:30")

	// 前置：两者确实都在夜猫窗口内（否则本测试失去意义）。
	require.True(t, codeBuddyGrowthInNightCatWindow(beforeMidnight))
	require.True(t, codeBuddyGrowthInNightCatWindow(afterMidnight))

	require.Equal(t,
		codeBuddyGrowthNightAnchorDate(beforeMidnight),
		codeBuddyGrowthNightAnchorDate(afterMidnight),
		"同一个夜的两个时刻必须锚定到同一日期，否则调度层去重失效、会跑两轮")
}

// Scenario：夜间趟在同一个夜的两次 tick 只跑**一轮**（端到端）。
func TestCodeBuddyGrowthNightPassRunsOncePerNight(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	// 夜的前半段。
	env.at(t, "2026-09-29", "23:30")
	afterFirst := env.runner.runCalls.Load()
	require.Equal(t, int64(1), afterFirst, "夜猫窗口内应跑一轮")

	// 同一个夜的后半段（已过午夜，日历日期变了）。
	env.at(t, "2026-09-30", "07:30")
	require.Equal(t, afterFirst, env.runner.runCalls.Load(),
		"同一个夜的后半段不得再跑一轮（跨午夜去重必须生效）")

	// 次日夜里应重新跑（"每夜一次"不是"只一次"）。
	env.at(t, "2026-09-30", "23:30")
	require.Greater(t, env.runner.runCalls.Load(), afterFirst,
		"下一个夜应重新触发")
}

// --- 立即执行（设置页按钮的后端）---

// Scenario：函数级覆盖——三个 codebuddy 功能都注册了立即执行者。
//
// ⚠️ 这条测试是**结构性**的：它不检查"哪个功能该注册"（那是 wire 的职责），
// 而是断言注册机制本身可用、且派发能到达执行者。
// 通道/功能清单的完整性由 `TestPlatformFeatureImmediateRegistrationCoverage`
// 在 handler 层用真实注册表断言。
func TestRunPlatformFeatureNowDispatchesToRunner(t *testing.T) {
	const (
		platform = "cb-test-dispatch"
		key      = "run"
	)
	called := 0
	RegisterPlatformFeatureImmediateRunner(platform, key, func(context.Context) (*PlatformFeatureRunResult, error) {
		called++
		return &PlatformFeatureRunResult{Summary: "已执行"}, nil
	})
	t.Cleanup(func() { UnregisterPlatformFeatureImmediateRunner(platform, key) })

	require.True(t, PlatformFeatureSupportsImmediateRun(platform, key),
		"注册后应报告支持立即执行")

	svc := &SettingService{}
	// 注册表里没有这个平台 → 先造一个（用真实注册函数，避免绕过注册路径）。
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: platform,
		Features: []PlatformFeatureDefinition{
			{Key: key, Kind: PlatformFeatureBool, Title: "T"},
		},
	})

	result, err := svc.RunPlatformFeatureNow(context.Background(), platform, key)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "已执行", result.Summary)
	require.Equal(t, 1, called, "应派发到执行者")
}

// Scenario：未注册的功能 → not found（调用方拼错 key 不该得到含糊的 500）。
func TestRunPlatformFeatureNowUnknownFeatureIsNotFound(t *testing.T) {
	svc := &SettingService{}
	_, err := svc.RunPlatformFeatureNow(context.Background(), "no-such-platform", "no-such-key")
	require.Error(t, err)
	require.True(t, infraerrors.IsNotFound(err),
		"未注册的功能应为 not found 语义（拼错 key 要能一眼看出来），实际：%v", err)
}

// Scenario：功能存在但**没有**立即执行者 → bad request（而不是 not found）。
//
// 区分这两者是刻意的：not found 是"你拼错了"，bad request 是"这个功能
// 就是不支持手动触发"。混在一起会让调用方以为 key 写错了。
func TestRunPlatformFeatureNowFeatureWithoutRunnerIsBadRequest(t *testing.T) {
	const (
		platform = "cb-test-norunner"
		key      = "noorunner"
	)
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: platform,
		Features: []PlatformFeatureDefinition{
			{Key: key, Kind: PlatformFeatureBool, Title: "T"},
		},
	})
	// 故意不注册执行者。
	UnregisterPlatformFeatureImmediateRunner(platform, key)

	svc := &SettingService{}
	_, err := svc.RunPlatformFeatureNow(context.Background(), platform, key)
	require.Error(t, err)
	require.True(t, infraerrors.IsBadRequest(err),
		"已注册但无立即执行者应为 bad request，实际：%v", err)
}

// Scenario：空 platform/key → bad request（不进入注册表查找）。
func TestRunPlatformFeatureNowRejectsEmptyArgs(t *testing.T) {
	svc := &SettingService{}
	for _, tc := range [][2]string{{"", "k"}, {"p", ""}, {"", ""}, {"  ", "  "}} {
		_, err := svc.RunPlatformFeatureNow(context.Background(), tc[0], tc[1])
		require.Error(t, err, "空参数应报错：%q/%q", tc[0], tc[1])
	}
}

// Scenario：执行者返回错误 → 原样上抛（不被吞成"成功"）。
func TestRunPlatformFeatureNowPropagatesRunnerError(t *testing.T) {
	const (
		platform = "cb-test-err"
		key      = "boom"
	)
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: platform,
		Features: []PlatformFeatureDefinition{{Key: key, Kind: PlatformFeatureBool, Title: "T"}},
	})
	RegisterPlatformFeatureImmediateRunner(platform, key,
		func(context.Context) (*PlatformFeatureRunResult, error) {
			return nil, infraerrors.InternalServer("BOOM", "runner failed")
		})
	t.Cleanup(func() { UnregisterPlatformFeatureImmediateRunner(platform, key) })

	svc := &SettingService{}
	_, err := svc.RunPlatformFeatureNow(context.Background(), platform, key)
	require.Error(t, err, "执行者报错必须上抛，不能静默当成功")
	require.Contains(t, err.Error(), "runner failed")
}

// --- 文案口径 ---

// Scenario：失败数必须在文案里出现（不谎报成功）。
func TestPlatformFeatureRunTextNeverHidesFailures(t *testing.T) {
	checkin := codeBuddyCheckinSummaryText(&CodeBuddyCheckinBatchResponse{
		Total: 3, Succeeded: 1, Failed: 2,
	})
	require.Contains(t, checkin, "失败 2", "签到文案必须报出失败数")

	activity := codeBuddyActivitySummaryText(CodeBuddyActivityRunSummary{
		Attempted: 3, Reported: 1, Failed: 2,
	})
	require.Contains(t, activity, "失败 2", "上报文案必须报出失败数")

	growth := codeBuddyGrowthSummaryText(CodeBuddyGrowthRunSummary{
		Attempted: 3, Succeeded: 1, Failed: 2,
	})
	require.Contains(t, growth, "失败 2", "成长链文案必须报出失败数")
}

// Scenario：上报的自检异常必须显式提示（坑 1：200 ≠ 真点亮）。
func TestPlatformFeatureRunTextFlagsActivitySelfCheck(t *testing.T) {
	text := codeBuddyActivitySummaryText(CodeBuddyActivityRunSummary{
		Attempted: 2, Reported: 2, SelfCheckSuspicious: 1,
	})
	require.Contains(t, text, "自检异常",
		"自检可疑 = 可能被上游静默丢弃，必须提示，否则运维会以为一定点亮了")
}

// Scenario：全成功时文案不出现"失败"字样（避免噪音）。
func TestPlatformFeatureRunTextOmitsZeroFailures(t *testing.T) {
	text := codeBuddyGrowthSummaryText(CodeBuddyGrowthRunSummary{
		Attempted: 2, Succeeded: 2, Channels: []string{"streak", "travel_run"},
	})
	require.NotContains(t, text, "失败")
	require.Contains(t, text, "streak")
}

// 保证 time 被使用（parseCST 在别处定义，这里显式引用一下避免 lint 噪音）。
var _ = time.Now
