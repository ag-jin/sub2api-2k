package service

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/stretchr/testify/require"
)

// 成长链调度器测试（A6 批 P5）。
//
// 核心风险：**full 级通道绝不能进自动排程**（用户裁定的合规红线）。
// 其余与 A4/A5 同款：默认关闭、窗口内一次、当日去重。

// codeBuddyGrowthRunnerStub 记录调用与传入的通道键。
type codeBuddyGrowthRunnerStub struct {
	listCalls   atomic.Int64
	runCalls    atomic.Int64
	candidates  []codeBuddyGrowthCandidate
	listErr     error
	seenKeys    []string
	summary     CodeBuddyGrowthAccountSummary
	onRunCalled func()
}

func (r *codeBuddyGrowthRunnerStub) ListCodeBuddyGrowthCandidates(_ context.Context, _ int) ([]codeBuddyGrowthCandidate, error) {
	r.listCalls.Add(1)
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.candidates, nil
}

func (r *codeBuddyGrowthRunnerStub) RunCodeBuddyGrowthChannels(
	_ context.Context, _ *Account, _ string, keys []string,
) CodeBuddyGrowthAccountSummary {
	r.runCalls.Add(1)
	r.seenKeys = keys
	if r.onRunCalled != nil {
		r.onRunCalled()
	}
	return r.summary
}

func (r *codeBuddyGrowthRunnerStub) rounds() int { return int(r.listCalls.Load()) }

type codeBuddyGrowthSchedulerTestEnv struct {
	scheduler *CodeBuddyGrowthScheduler
	runner    *codeBuddyGrowthRunnerStub
	zone      *time.Location
	current   time.Time
}

func newCodeBuddyGrowthSchedulerTestEnv(t *testing.T, featuresJSON string, accounts ...*Account) *codeBuddyGrowthSchedulerTestEnv {
	t.Helper()
	repo := newPlatformFeatureTestRepo()
	if featuresJSON != "" {
		repo.vals[SettingFeatureStorageKeyForTest()] = featuresJSON
	}
	svc := newPlatformFeatureTestService(t, repo)
	runner := &codeBuddyGrowthRunnerStub{}
	accountRepo := &codeBuddyActivityAccountRepoStub{accounts: map[int64]*Account{}}
	for _, account := range accounts {
		accountRepo.accounts[account.ID] = account
	}

	scheduler := NewCodeBuddyGrowthScheduler(runner, accountRepo, svc)
	scheduler.accountDelay = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	env := &codeBuddyGrowthSchedulerTestEnv{
		scheduler: scheduler, runner: runner, zone: codeBuddyTimeZone,
	}
	scheduler.clock = func() time.Time { return env.current }
	return env
}

func (e *codeBuddyGrowthSchedulerTestEnv) at(t *testing.T, day, hhmm string) {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, e.zone)
	require.NoError(t, err)
	e.current = parsed
	e.scheduler.tick()
}

const growthEnabledWindow = `{"codebuddy":{"growth":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`

// --- 合规红线（本批最重要的一条）---

// Scenario：调度器**永远不会**把 full 级通道交给执行。
//
// 这是用户裁定的合规边界（`full` = 含伪造活跃上报语义 = 仅手动）。
// 断言两层：
//  1. 静态：调度器源码里取通道列表走的是分级过滤入口，而不是遍历全表；
//  2. 运行时：实际传给执行层的 keys 里不含任何 full 级通道。
func TestCodeBuddyGrowthSchedulerNeverRunsFullTierChannels(t *testing.T) {
	// 静态层：确认取通道的方式。
	source, err := os.ReadFile("codebuddy_growth_scheduler.go")
	require.NoError(t, err, "读不到调度器源码，测试失效（不是通过）")
	text := string(source)

	require.Contains(t, text, "CodeBuddyGrowthAutoSchedulableChannelKeys()",
		"必须经分级过滤入口取通道（full 级在类型层面进不来）")
	require.NotContains(t, text, "range codebuddy.CodeBuddyGrowthChannelSpecs",
		"不得遍历全表自行过滤——那样新增 full 通道会被静默纳入排程")

	// 运行时层：真正跑一轮，看传给执行层的 keys。
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "09:00")

	require.NotEmpty(t, env.runner.seenKeys, "应真的执行了一轮")
	for _, key := range env.runner.seenKeys {
		spec, ok := codebuddy.CodeBuddyGrowthChannelSpecByKey(key)
		require.True(t, ok, "通道 %s 未注册", key)
		require.True(t, spec.Tier.AutoSchedulable(),
			"通道 %s 是 %s 级（含伪造活跃上报语义），**不得**进自动排程", key, spec.Tier)
	}
	// full 级通道名逐个确认不在其中（比上面的循环更直白）。
	for _, full := range []string{
		codebuddy.CodeBuddyGrowthChannelAdopt,
		codebuddy.CodeBuddyGrowthChannelNightCat,
		codebuddy.CodeBuddyGrowthChannelSchool,
	} {
		require.NotContains(t, env.runner.seenKeys, full)
	}
}

// Scenario：调度器**可以**自动（与活跃上报不同）——claim/preview 级会真的执行。
//
// 这条与上一条是一对：只测"不跑 full"会退化成"什么都不跑"也算通过。
func TestCodeBuddyGrowthSchedulerRunsAutoRunnableChannels(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "09:00")

	require.Equal(t, int64(1), env.runner.runCalls.Load(), "claim/preview 级通道应可自动执行")
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelStreak)
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelTravelRun)
}

// --- 默认关闭 ---

// Scenario：未配置平台功能时，一个请求都不发。
func TestCodeBuddyGrowthSchedulerDefaultOffSendsNothing(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, "")

	for _, hhmm := range []string{"08:59", "09:00", "10:00", "11:00", "12:00"} {
		env.at(t, "2026-09-23", hhmm)
	}
	require.Equal(t, 0, env.runner.rounds(), "未配置时连候选都不该查（默认关闭）")
	require.Equal(t, int64(0), env.runner.runCalls.Load())
}

// Scenario：显式 enabled=false 同样不发。
func TestCodeBuddyGrowthSchedulerExplicitlyDisabledSendsNothing(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t,
		`{"codebuddy":{"growth":{"enabled":false,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	env.at(t, "2026-09-23", "09:00")
	require.Equal(t, 0, env.runner.rounds())
}

// --- 窗口内一次 + 当日去重 ---

// Scenario：窗口内触发**恰好一次**（不是每分钟一遍）。
func TestCodeBuddyGrowthSchedulerTriggersExactlyOnceInsideWindow(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow)

	for hour := 9; hour < 11; hour++ {
		for minute := 0; minute < 60; minute++ {
			env.at(t, "2026-09-23", time.Date(2026, 9, 23, hour, minute, 0, 0, env.zone).Format("15:04"))
		}
	}
	require.Equal(t, 1, env.runner.rounds(), "窗口内恰好一次")
}

// Scenario：窗口外不触发。
func TestCodeBuddyGrowthSchedulerOutsideWindowSendsNothing(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow)

	for _, hhmm := range []string{"00:00", "08:59", "11:00", "23:59"} {
		env.at(t, "2026-09-23", hhmm)
	}
	require.Equal(t, 0, env.runner.rounds(), "窗口外不得触发")
}

// Scenario：次日重新触发（"每天一次"不是"只一次"）。
func TestCodeBuddyGrowthSchedulerRunsAgainNextDay(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow)

	env.at(t, "2026-09-23", "09:00")
	require.Equal(t, 1, env.runner.rounds())
	env.at(t, "2026-09-23", "10:00")
	require.Equal(t, 1, env.runner.rounds(), "同日不重复")
	env.at(t, "2026-09-24", "09:00")
	require.Equal(t, 2, env.runner.rounds(), "次日应重新触发")
}

// --- 单号失败不影响其他号 ---

// Scenario：一个账号出错，其余照常（号级隔离）。
func TestCodeBuddyGrowthSchedulerContinuesAfterAccountFailure(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"),
		newCodeBuddyActivityAccount(2, "u-2", "t-2"),
		newCodeBuddyActivityAccount(3, "u-3", "t-3"))
	env.runner.candidates = []codeBuddyGrowthCandidate{
		{AccountID: 1, Name: "cb-1"}, {AccountID: 2, Name: "cb-2"}, {AccountID: 3, Name: "cb-3"},
	}
	env.runner.summary = CodeBuddyGrowthAccountSummary{Error: "boom"}

	env.at(t, "2026-09-23", "09:00")

	require.Equal(t, int64(3), env.runner.runCalls.Load(), "三个候选都要尝试（坏号不拖停后续）")
	summary := env.scheduler.codeBuddyGrowthSchedulerSummaryForTest()
	require.Equal(t, 0, summary.Succeeded)
	require.Equal(t, 3, summary.Failed)
}

// Scenario：跳过项（停调/缺凭据）不发请求。
func TestCodeBuddyGrowthSchedulerSkipsMarkedCandidates(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{
		{AccountID: 1, Name: "cb-1"},
		{AccountID: 2, Name: "cb-2", SkipReason: "账号已停调"},
	}

	env.at(t, "2026-09-23", "09:00")

	require.Equal(t, int64(1), env.runner.runCalls.Load(), "被标记跳过的候选不该发请求")
	require.Equal(t, 1, env.scheduler.codeBuddyGrowthSchedulerSummaryForTest().Skipped)
}

// --- 夜猫子跨零点窗口（必写测试 ③）---

// Scenario：夜猫窗口 23:00–08:00 是**跨零点**窗口，两侧与中间都要判对。
func TestCodeBuddyNightCatWindowIsCrossMidnight(t *testing.T) {
	inWindow := []string{"23:00", "23:30", "00:00", "03:00", "07:59"}
	outside := []string{"08:00", "08:01", "12:00", "22:59"}

	for _, hhmm := range inWindow {
		moment := parseCST(t, "2026-09-23", hhmm)
		require.True(t, codeBuddyGrowthInNightCatWindow(moment),
			"%s 应在夜猫窗口内（23:00–08:00 跨零点）", hhmm)
	}
	for _, hhmm := range outside {
		moment := parseCST(t, "2026-09-23", hhmm)
		require.False(t, codeBuddyGrowthInNightCatWindow(moment),
			"%s 应在夜猫窗口外", hhmm)
	}
}

func parseCST(t *testing.T, day, hhmm string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, codeBuddyTimeZone)
	require.NoError(t, err)
	return parsed
}

// --- 手动通道枚举 ---

// Scenario：手动通道集合 = 全表里 full 级的那几个（按分级推导，不硬编码）。
func TestCodeBuddyGrowthManualChannelsDerivedFromTier(t *testing.T) {
	manual := codeBuddyGrowthManualChannels()
	require.NotEmpty(t, manual)

	keys := map[string]bool{}
	for _, spec := range manual {
		require.False(t, spec.Tier.AutoSchedulable(),
			"手动通道集合里不该出现可自动的 %s", spec.Key)
		keys[spec.Key] = true
	}
	// 三个已知 full 级通道必须都在（新增 full 级通道时会自动进来）。
	for _, want := range []string{
		codebuddy.CodeBuddyGrowthChannelAdopt,
		codebuddy.CodeBuddyGrowthChannelNightCat,
		codebuddy.CodeBuddyGrowthChannelSchool,
	} {
		require.True(t, keys[want], "%s 是 full 级，必须可手动", want)
	}
}

// Scenario：通道键规范化（去空白、去空项、保序去重）。
func TestCodeBuddyGrowthNormalizeChannelKeys(t *testing.T) {
	got := codeBuddyGrowthNormalizeChannelKeys([]string{" streak ", "", "adopt", "streak", "  "})
	require.Equal(t, []string{"streak", "adopt"}, got)
}

// Scenario：单个通道派发时，full 级通道**不执行**（自动路径的兜底防线）。
//
// 即使调用方（错误地）把 full 级 key 传进来，派发层也不执行并告警。
func TestDispatchSkipsFullTierChannelEvenIfRequested(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)

	summary := svc.RunCodeBuddyGrowthChannels(context.Background(), account, "2026-09-23",
		[]string{codebuddy.CodeBuddyGrowthChannelAdopt, codebuddy.CodeBuddyGrowthChannelNightCat})

	require.NotEmpty(t, summary.Error, "没有可执行通道时应显式报错，而不是静默当成功")
	require.Empty(t, recorder.snapshot(), "full 级通道不该发出任何请求")
}

// Scenario：未注册通道键 → 跳过且不执行（不静默跑掉）。
func TestDispatchSkipsUnknownChannelKey(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	})

	summary := svc.RunCodeBuddyGrowthChannels(context.Background(), newGrowthTestAccount(1),
		"2026-09-23", []string{"nonexistent_channel"})

	require.NotEmpty(t, summary.Error)
	require.Empty(t, recorder.snapshot())
}

// 确保 strings 被用到（提取函数体时用）。
var _ = strings.TrimSpace

// Scenario：抽奖是 `full` 级（不可逆消耗），**自动排程不得执行它**。
//
// 2026-09-23 裁定：抽奖不幂等（每次 draw 必须新 client_token、次数不可恢复），
// 按扩展后的 full 定义归 full。它已从 streak 通道拆出——此用例锁住"拆出"这个事实，
// 防有人图省事把它挪回 streak（那会让整条 streak 的"可自动"结论变错，
// 且抽奖会开始自动消耗次数）。
func TestCodeBuddyGrowthSchedulerNeverRunsLottery(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "09:00")

	for _, key := range env.runner.seenKeys {
		require.NotEqual(t, codebuddy.CodeBuddyGrowthChannelLottery, key,
			"抽奖是不可逆消耗，属 full 级，不得进自动排程")
	}
	// 且 streak 通道仍在（确认"拆出抽奖"没把整条链一起拆掉）。
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelStreak)
}

// Scenario：抽奖手动入口在有次数时真的抽，无次数时跳过（不谎报）。
func TestRunLotteryNowDrawsAndSkips(t *testing.T) {
	t.Run("有次数则抽光", func(t *testing.T) {
		svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == codebuddy.CodeBuddyGrowthLotteryChancesPath {
				_, _ = w.Write([]byte(`{"code":0,"data":{"balance":2}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"prize_type":"credit","credit_amount":5}}`))
		})

		result := svc.RunCodeBuddyGrowthLotteryNow(context.Background(), newGrowthTestAccount(1))

		require.Empty(t, result.Error)
		require.Equal(t, 2, result.Chances)
		require.Equal(t, 2, result.Drawn, "有 2 次就抽 2 次")

		draws := 0
		for _, req := range recorder.snapshot() {
			if req.Path == codebuddy.CodeBuddyGrowthLotteryDrawPath {
				draws++
			}
		}
		require.Equal(t, 2, draws)
	})

	t.Run("无次数则不抽", func(t *testing.T) {
		svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == codebuddy.CodeBuddyGrowthLotteryChancesPath {
				_, _ = w.Write([]byte(`{"code":0,"data":{"balance":0}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0}`))
		})

		result := svc.RunCodeBuddyGrowthLotteryNow(context.Background(), newGrowthTestAccount(1))

		require.Empty(t, result.Error)
		require.Equal(t, 0, result.Drawn)
		require.Contains(t, result.SkipReason, "无抽奖次数")
		for _, req := range recorder.snapshot() {
			require.NotEqual(t, codebuddy.CodeBuddyGrowthLotteryDrawPath, req.Path,
				"无次数时不该发抽奖请求")
		}
	})
}
