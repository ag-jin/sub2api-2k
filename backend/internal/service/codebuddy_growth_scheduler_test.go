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
// 核心风险：**未授权自动的通道绝不能进自动排程**（用户裁定的合规边界）。
// 2026-09-29 用户授权 adopt / night_cat / school 进自动排程后，边界从
// "full 全部不得自动"变为"未授权的不得自动"（当前只有 lottery）。
// 其余与 A4/A5 同款：默认关闭、窗口内一次、当日去重。

// codeBuddyGrowthRunnerStub 记录调用与传入的通道键。
type codeBuddyGrowthRunnerStub struct {
	listCalls   atomic.Int64
	runCalls    atomic.Int64
	candidates  []codeBuddyGrowthCandidate
	listErr     error
	seenKeys    []string
	seenKeysLog [][]string
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
	// 累积记录：两趟（日间/夜间）各自调用一次，断言"某一趟带了哪些键"需要
	// 看全部轮次，只看最后一轮的 seenKeys 会被后一趟覆盖。
	r.seenKeysLog = append(r.seenKeysLog, keys)
	if r.onRunCalled != nil {
		r.onRunCalled()
	}
	return r.summary
}

// allSeenKeys 返回所有轮次见过的通道键（去重，保序）。
func (r *codeBuddyGrowthRunnerStub) allSeenKeys() []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, keys := range r.seenKeysLog {
		for _, key := range keys {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
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

// Scenario：调度器**只**把已授权自动的通道交给执行——未授权的（lottery）
// 一个都不跑，且夜间专属通道不会混进日间趟。
//
// 政策沿革：2026-09-22 裁定 full 全部仅手动；2026-09-29 用户授权
// adopt / night_cat / school 进自动排程（抽奖明确排除）。本测试断言的是
// **新政策**下的边界：
//   - 静态：调度器取通道列表走的是授权过滤入口，而不是遍历全表；
//   - 运行时：传给执行层的 keys 全部 `AutoRunnable()`；
//   - lottery 不在其中；
//   - night_cat 不在**日间**趟里（它属夜间窗口，见下一条测试）。
func TestCodeBuddyGrowthSchedulerNeverRunsUnauthorizedChannels(t *testing.T) {
	// 静态层：确认取通道的方式。
	source, err := os.ReadFile("codebuddy_growth_scheduler.go")
	require.NoError(t, err, "读不到调度器源码，测试失效（不是通过）")
	text := string(source)

	require.Contains(t, text, "CodeBuddyGrowthDaytimeAutoRunnableChannelKeys()",
		"日间趟必须经授权过滤入口取通道（未授权的 full 进不来）")
	require.Contains(t, text, "CodeBuddyGrowthNightAutoRunnableChannelKeys()",
		"夜间趟必须经授权过滤入口取通道")
	require.NotContains(t, text, "range codebuddy.CodeBuddyGrowthChannelSpecs",
		"不得遍历全表自行过滤——那样新增未授权通道会被静默纳入排程")

	// 运行时层：真正跑一轮日间趟，看传给执行层的 keys。
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "09:00")

	require.NotEmpty(t, env.runner.seenKeys, "应真的执行了一轮")
	for _, key := range env.runner.seenKeys {
		spec, ok := codebuddy.CodeBuddyGrowthChannelSpecByKey(key)
		require.True(t, ok, "通道 %s 未注册", key)
		require.True(t, spec.AutoRunnable(),
			"通道 %s 未获自动授权，**不得**进自动排程", key)
	}
	// 未授权通道逐个确认不在其中（比上面的循环更直白）。
	require.NotContains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelLottery,
		"抽奖未获授权（一次抽光全部次数、不可恢复），不得进自动排程")
	// 夜间专属通道不得出现在日间趟（它由 tickNight 单独跑）。
	require.NotContains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelNightCat,
		"night_cat 属夜间窗口，不应出现在日间趟")
}

// Scenario：日间趟**真的**会跑已授权的 full 通道（adopt / school）。
//
// 这条与上一条是一对：只测"不跑未授权的"会退化成"什么都不跑"也算通过。
// 它同时钉住本次授权的落地——adopt 与 school 必须真的被日间趟带上。
func TestCodeBuddyGrowthSchedulerRunsAuthorizedFullChannels(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "09:00")

	require.Equal(t, int64(1), env.runner.runCalls.Load(), "已授权通道应可自动执行")
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelStreak)
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelTravelRun)
	// 本次授权的 full 通道（日间趟）：
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelAdopt,
		"adopt 已获用户 2026-09-29 授权，应在日间趟里")
	require.Contains(t, env.runner.seenKeys, codebuddy.CodeBuddyGrowthChannelSchool,
		"school 已获用户 2026-09-29 授权，应在日间趟里")
}

// Scenario：夜间趟在夜猫窗口内跑 night_cat，且在窗口外不跑。
//
// 夜猫窗口是 23:00–08:00（跨零点），与日间窗口（09:00–11:00）**不重叠**——
// 这正是必须有夜间趟的原因：混在日间趟里它永远是 skip。
func TestCodeBuddyGrowthSchedulerRunsNightCatInNightWindow(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	// 窗口外（日间窗口也不行——日间趟的通道集合里没有 night_cat）。
	env.at(t, "2026-09-23", "10:00")
	require.NotContains(t, env.runner.allSeenKeys(), codebuddy.CodeBuddyGrowthChannelNightCat,
		"night_cat 不得在日间窗口跑")

	// 窗口内（23:30）→ 夜间趟应带上它。
	env.at(t, "2026-09-23", "23:30")
	require.Contains(t, env.runner.allSeenKeys(), codebuddy.CodeBuddyGrowthChannelNightCat,
		"夜间窗口内应跑 night_cat")
}

// Scenario：夜间趟**不受管理员配置的日间窗口**影响（它是动作性质的硬约束）。
//
// 若有人把夜间趟也挂到 `growth` 的配置窗口上，23:30 就永远不会跑——
// 而 night_cat 的有效期恰好只在那个时段。
func TestCodeBuddyGrowthNightPassIgnoresConfiguredDaytimeWindow(t *testing.T) {
	// 配置一个**狭窄的**日间窗口（09:00–10:00），夜间趟仍应在 23:30 跑。
	env := newCodeBuddyGrowthSchedulerTestEnv(t,
		`{"codebuddy":{"growth":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":10,"minute":0}}}}`,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "23:30")
	require.Contains(t, env.runner.allSeenKeys(), codebuddy.CodeBuddyGrowthChannelNightCat,
		"夜间趟应独立于日间配置窗口")
}

// Scenario：夜间趟与日间趟各自去重，互不占用对方的"今天已跑"位。
func TestCodeBuddyGrowthNightAndDayPassesDeduplicateSeparately(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []codeBuddyGrowthCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.summary = CodeBuddyGrowthAccountSummary{AccountID: 1}

	env.at(t, "2026-09-23", "09:30") // 日间趟跑
	dayCalls := env.runner.runCalls.Load()
	require.Equal(t, int64(1), dayCalls)

	env.at(t, "2026-09-23", "23:30") // 夜间趟应**仍能**跑（不被日间趟占位）
	require.Greater(t, env.runner.runCalls.Load(), dayCalls,
		"夜间趟不应被日间趟的当日去重挡住")

	require.Contains(t, env.runner.allSeenKeys(), codebuddy.CodeBuddyGrowthChannelNightCat)

	// 同一天再进夜间窗口第二次 → 夜间趟自己的去重应挡住。
	before := env.runner.runCalls.Load()
	env.at(t, "2026-09-23", "23:40")
	require.Equal(t, before, env.runner.runCalls.Load(),
		"夜间趟在当日第二次 tick 应被自身去重挡住")
}

// Scenario：夜间窗口的调度判据与通道内部的判据一致（两处常量不许漂移）。
//
// 漂移的后果：调度起来了但通道全 skip（浪费 tick），或通道想做但调度没起来（当日漏做）。
func TestCodeBuddyGrowthNightWindowMatchesChannelGuard(t *testing.T) {
	// 通道内部判据（`codeBuddyGrowthInNightCatWindow`）与调度窗口常量应一致。
	for _, moment := range []string{"22:59", "23:00", "23:30", "00:30", "07:59", "08:00", "12:00"} {
		at := parseCST(t, "2026-09-23", moment)
		inner := codeBuddyGrowthInNightCatWindow(at)
		outer := WithinTimeRange(
			at,
			TimeOfDay{Hour: codeBuddyGrowthNightStartHour},
			TimeOfDay{Hour: codeBuddyGrowthNightEndHour},
			codeBuddyTimeZone,
		)
		require.Equal(t, inner, outer,
			"%s：通道判据=%v 与调度判据=%v 不一致（两处窗口漂移）", moment, inner, outer)
	}
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

// Scenario：**两个窗口之外**不触发。
//
// ⚠️ 取的时刻必须避开**两个**窗口：日间（配置的 09:00–11:00）与夜间
// （23:00–08:00 跨零点）。原用例用过 00:00 / 23:59，它们现在落在夜间窗口里
// ——夜间趟会跑 night_cat，那是预期行为，不是"窗口外触发"。
func TestCodeBuddyGrowthSchedulerOutsideWindowSendsNothing(t *testing.T) {
	env := newCodeBuddyGrowthSchedulerTestEnv(t, growthEnabledWindow)

	for _, hhmm := range []string{"08:30", "11:00", "12:00", "22:59"} {
		env.at(t, "2026-09-23", hhmm)
	}
	require.Equal(t, 0, env.runner.rounds(),
		"日间与夜间两个窗口之外都不得触发（08:30/11:00/12:00/22:59）")
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

// Scenario：派发层的授权兜底——**未授权**通道即使被误传进来也不执行。
//
// 这是自动路径的第二道防线（第一道是取列表时按授权过滤）。当前未授权的
// 只有 lottery：它一次抽光全部次数且不可恢复，误跑代价高。
//
// ⚠️ 本用例此前用 adopt / night_cat 做样本（当时它们是 full=仅手动）。
// 2026-09-29 授权后那两个**应该**被执行，断言随之失效——换成 lottery，
// 因为兜底防线本身仍然存在且仍然需要被守住。
func TestDispatchSkipsUnauthorizedChannelEvenIfRequested(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)

	summary := svc.RunCodeBuddyGrowthChannels(context.Background(), account, "2026-09-23",
		[]string{codebuddy.CodeBuddyGrowthChannelLottery})

	require.NotEmpty(t, summary.Error, "没有可执行通道时应显式报错，而不是静默当成功")
	require.Empty(t, recorder.snapshot(), "未授权通道不该发出任何请求")
}

// Scenario：已授权的 full 通道**会被**派发执行（授权落地的正向断言）。
//
// 与上一条配对：只测"跳过未授权的"会退化成"授权了也不跑"也算通过。
// adopt 的动作会先查 buddy/info——所以断言"发出了请求"即可证明它真的进了执行。
func TestDispatchRunsAuthorizedFullChannel(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		// buddy/info 回无猫，让流程继续走到 agreement。
		_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
	})
	account := newGrowthTestAccount(1)

	summary := svc.RunCodeBuddyGrowthChannels(context.Background(), account, "2026-09-23",
		[]string{codebuddy.CodeBuddyGrowthChannelAdopt})

	require.Empty(t, summary.Error, "已授权通道应可执行（不应报'没有可执行通道'）")
	require.NotEmpty(t, recorder.snapshot(),
		"adopt 已获授权，应真的发出请求（查 buddy/info）")
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
