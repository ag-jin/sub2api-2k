package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 活跃上报调度器测试（A5 批 5.2）。
//
// 三条核心风险，必须钉死：
//   1. **默认关闭**：未配置时一个上游请求都不发（对外写操作，显式开启原则）
//   2. **每号每天 1 次**（坑 4）：当日已执行后，窗口内其余 tick 全部静默
//   3. **窗口语义**：窗口外不发；窗口内触发恰好一次
//
// 做法与签到调度器测试一致：直接调 tick()、注入 clock 固定"现在"，
// runner stub 记录调用次数——"没发上游请求"由调用次数为 0 直接证明。

// codeBuddyActivityRunnerStub 记录上报调用，可注入错误与结果。
type codeBuddyActivityRunnerStub struct {
	reportCalls atomic.Int64
	listCalls   atomic.Int64
	candidates  []CodeBuddyCheckinCandidate
	listErr     error
	result      CodeBuddyActivityReportResult
	// onReport 在每次上报**期间**调用（模拟执行慢于 tick 间隔时的重入）。
	onReport func()
	// lastCount 记录最后一次收到的 count 参数（验证 N 条是 5）。
	lastCount atomic.Int64
}

func (r *codeBuddyActivityRunnerStub) ListCodeBuddyActivityCandidates(_ context.Context, _ int) ([]CodeBuddyCheckinCandidate, error) {
	r.listCalls.Add(1)
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.candidates, nil
}

func (r *codeBuddyActivityRunnerStub) ReportCodeBuddyActivity(
	_ context.Context,
	_ *Account,
	count int,
) CodeBuddyActivityReportResult {
	r.reportCalls.Add(1)
	r.lastCount.Store(int64(count))
	if r.onReport != nil {
		r.onReport()
	}
	return r.result
}

func (r *codeBuddyActivityRunnerStub) reportCount() int { return int(r.reportCalls.Load()) }

// rounds 本轮"调度器真的执行了一轮"的次数。
//
// 窗口/去重类的断言要用它而不是 reportCount：那类用例关心的是"这一轮跑没跑"，
// 而 reportCount 还取决于有没有候选账号——没有候选时一轮跑完也不会有上报调用，
// 用它断言会把"没触发"与"触发了但没账号"混为一谈。
func (r *codeBuddyActivityRunnerStub) rounds() int { return int(r.listCalls.Load()) }

// codeBuddyActivityAccountRepoStub 供调度器按 ID 取账号。
type codeBuddyActivityAccountRepoStub struct {
	AccountRepository
	accounts map[int64]*Account
	loadErr  error
}

func (r *codeBuddyActivityAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	if account, ok := r.accounts[id]; ok {
		return account, nil
	}
	return nil, ErrAccountNotFound
}

// codeBuddyActivitySchedulerTestEnv 可推进时钟的调度器测试环境。
type codeBuddyActivitySchedulerTestEnv struct {
	scheduler *CodeBuddyActivityScheduler
	runner    *codeBuddyActivityRunnerStub
	repo      *platformFeatureTestRepo
	zone      *time.Location
	current   time.Time
}

func newCodeBuddyActivitySchedulerTestEnv(t *testing.T, featuresJSON string, accounts ...*Account) *codeBuddyActivitySchedulerTestEnv {
	t.Helper()
	repo := newPlatformFeatureTestRepo()
	if featuresJSON != "" {
		repo.vals[SettingFeatureStorageKeyForTest()] = featuresJSON
	}
	svc := newPlatformFeatureTestService(t, repo)
	runner := &codeBuddyActivityRunnerStub{}
	accountRepo := &codeBuddyActivityAccountRepoStub{accounts: map[int64]*Account{}}
	for _, account := range accounts {
		accountRepo.accounts[account.ID] = account
	}

	scheduler := NewCodeBuddyActivityScheduler(runner, accountRepo, svc)
	// 账号间间隔置 0：否则多账号用例要真等 800ms×N。
	scheduler.accountDelay = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	env := &codeBuddyActivitySchedulerTestEnv{
		scheduler: scheduler,
		runner:    runner,
		repo:      repo,
		zone:      codeBuddyTimeZone,
	}
	scheduler.clock = func() time.Time { return env.current }
	return env
}

// at 把"现在"设为 UTC+8 当地的某天某时刻并立即 tick。
func (e *codeBuddyActivitySchedulerTestEnv) at(t *testing.T, day, hhmm string) {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, e.zone)
	require.NoError(t, err)
	e.current = parsed
	e.scheduler.tick()
}

// --- 必写测试 6：默认关闭 ---

// Scenario：**从未配置**平台功能时，调度器一个上游请求都不发。
//
// 行为证据（而非断言配置里写着 false）：list/report 调用次数均为 0。
func TestCodeBuddyActivitySchedulerDefaultOffSendsNoUpstreamRequest(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t, "")

	for _, hhmm := range []string{"09:59", "10:00", "10:30", "10:59", "11:00", "12:00"} {
		env.at(t, "2026-09-22", hhmm)
	}
	require.Equal(t, 0, env.runner.reportCount(), "未配置时不得发出任何上报请求（默认关闭）")
	require.Equal(t, 0, env.runner.rounds(), "连候选列表都不该查")
}

// Scenario：显式 enabled=false 同样不发。
func TestCodeBuddyActivitySchedulerExplicitlyDisabledSendsNoRequest(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":false,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	for _, hhmm := range []string{"10:00", "10:30", "10:59"} {
		env.at(t, "2026-09-22", hhmm)
	}
	require.Equal(t, 0, env.runner.rounds(), "显式关闭时不得发出上报请求")
}

// --- 必写测试 5 + 3：当日一次 / 窗口语义 ---

// Scenario：窗口内触发**恰好一次**，窗口内其余 tick 全部静默（坑 4：每号每天 1 次）。
func TestCodeBuddyActivitySchedulerTriggersExactlyOnceInsideWindow(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	// 10:00 … 10:59 共 60 次 tick。
	for minute := 0; minute < 60; minute++ {
		env.at(t, "2026-09-22", time.Date(2026, 9, 22, 10, minute, 0, 0, env.zone).Format("15:04"))
	}
	require.Equal(t, 1, env.runner.rounds(),
		"窗口内必须恰好触发一次（风控口径：每号每天 1 次，不做高频上报）")
}

// Scenario：窗口外不触发（窗口语义的"不触发"半边）。
func TestCodeBuddyActivitySchedulerOutsideWindowSendsNoRequest(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	for _, hhmm := range []string{"00:00", "09:59", "11:00", "11:01", "23:59"} {
		env.at(t, "2026-09-22", hhmm)
	}
	require.Equal(t, 0, env.runner.rounds(), "窗口外不得触发（连一轮都不该跑）")
}

// Scenario：次日窗口再次触发（"每天一次"不是"只执行一次"）。
func TestCodeBuddyActivitySchedulerRunsAgainNextDay(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	env.at(t, "2026-09-22", "10:00")
	require.Equal(t, 1, env.runner.rounds())

	// 同日再 tick：静默。
	env.at(t, "2026-09-22", "10:30")
	require.Equal(t, 1, env.runner.rounds(), "同日重复 tick 不得再触发")

	// 次日同窗口：应再次触发。
	env.at(t, "2026-09-23", "10:00")
	require.Equal(t, 2, env.runner.rounds(), "次日应重新触发（每天一次）")
}

// Scenario：迟到启动仍能补报当天（窗口语义相对一次性定时器的关键优势）。
//
// 参考实现用 nextFire 算下一个 10:00 点起一次性定时器；进程 10:30 重启会把
// 当天整个跳过。窗口形态下 10:30 起来仍然补报。
func TestCodeBuddyActivitySchedulerCatchesUpAfterLateStart(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	// 当天首次 tick 就落在窗口中间（模拟进程 10:30 才起来）。
	env.at(t, "2026-09-22", "10:30")
	require.Equal(t, 1, env.runner.rounds(), "迟到启动仍应补报当天")
}

// --- 单号失败不影响其他号 ---

// Scenario：一个账号上报出错，其余账号照常上报（号级隔离）。
func TestCodeBuddyActivitySchedulerContinuesAfterAccountFailure(t *testing.T) {
	accounts := []*Account{
		newCodeBuddyActivityAccount(1, "u-1", "t-1"),
		newCodeBuddyActivityAccount(2, "u-2", "t-2"),
		newCodeBuddyActivityAccount(3, "u-3", "t-3"),
	}
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`,
		accounts...)

	// 第 2 个账号上报失败；其余成功。
	var calls atomic.Int64
	env.runner.candidates = []CodeBuddyCheckinCandidate{
		{AccountID: 1, Name: "cb-1"},
		{AccountID: 2, Name: "cb-2"},
		{AccountID: 3, Name: "cb-3"},
	}
	env.runner.result = CodeBuddyActivityReportResult{Reported: 5, Expected: 5, Verified: true}
	env.scheduler.runner = &codeBuddyActivityFailingMiddleStub{inner: env.runner, failID: 2, calls: &calls}

	env.at(t, "2026-09-22", "10:00")

	require.Equal(t, int64(3), calls.Load(), "3 个候选都要被尝试（坏号不得拖停后续号）")
}

// codeBuddyActivityFailingMiddleStub 让指定账号的上报失败，其余走 inner。
type codeBuddyActivityFailingMiddleStub struct {
	inner  *codeBuddyActivityRunnerStub
	failID int64
	calls  *atomic.Int64
}

func (s *codeBuddyActivityFailingMiddleStub) ListCodeBuddyActivityCandidates(ctx context.Context, limit int) ([]CodeBuddyCheckinCandidate, error) {
	return s.inner.ListCodeBuddyActivityCandidates(ctx, limit)
}

func (s *codeBuddyActivityFailingMiddleStub) ReportCodeBuddyActivity(
	ctx context.Context,
	account *Account,
	count int,
) CodeBuddyActivityReportResult {
	s.calls.Add(1)
	if account != nil && account.ID == s.failID {
		return CodeBuddyActivityReportResult{
			AccountID: account.ID,
			Err:       errCodeBuddyActivityMissingToken,
		}
	}
	return s.inner.ReportCodeBuddyActivity(ctx, account, count)
}

// Scenario：跳过项（停调/缺凭据）不发请求，但仍计入"本轮考虑过"。
func TestCodeBuddyActivitySchedulerSkipsMarkedCandidates(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))

	env.runner.candidates = []CodeBuddyCheckinCandidate{
		{AccountID: 1, Name: "cb-1"},
		{AccountID: 2, Name: "cb-2", SkipReason: "账号已停调"},
	}

	env.at(t, "2026-09-22", "10:00")

	require.Equal(t, 1, env.runner.reportCount(), "被标记跳过的候选不该发请求")
}

// Scenario：上报条数按 chat_5 门槛传 5（坑 3 的调度侧保障）。
func TestCodeBuddyActivitySchedulerReportsFivePerAccount(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []CodeBuddyCheckinCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.result = CodeBuddyActivityReportResult{Reported: 5, Expected: 5, Verified: true}

	env.at(t, "2026-09-22", "10:00")

	require.Equal(t, int64(5), env.runner.lastCount.Load(),
		"单账号必须报 5 条（chat_5 前置要求同会话 5 次对话）")
}

// Scenario：候选列表查询失败时整轮静默跳过，不 panic、不影响当日去重。
func TestCodeBuddyActivitySchedulerHandlesListFailure(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`)
	env.runner.listErr = errCodeBuddyActivityMissingToken

	env.at(t, "2026-09-22", "10:00")

	require.Equal(t, 0, env.runner.reportCount())
	// 当日已占位（乐观占位语义与签到一致：失败不回滚，避免退化成每分钟重试）。
	require.Equal(t, "2026-09-22", env.scheduler.CodeBuddyActivitySchedulerLastRunDate())
}

// Scenario：自定义时点被调度器读取（不是写死 10 点）。
func TestCodeBuddyActivitySchedulerHonoursConfiguredWindow(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":21,"minute":0},"end":{"hour":22,"minute":0}}}}`)

	// 默认 10 点档不该触发（已被改成 21 点）。
	env.at(t, "2026-09-22", "10:00")
	require.Equal(t, 0, env.runner.rounds(), "默认档位已被配置覆盖，不该在 10 点触发")

	env.at(t, "2026-09-22", "21:00")
	require.Equal(t, 1, env.runner.rounds(), "应按配置的 21 点档触发")
}

// --- 5.3：自检结果必须如实反映，不得谎报成功 ---

// Scenario：账号上报成功但**自检异常**（days=0，坑 1 的静默丢弃信号）时，
// 调度器必须把它计入 selfCheckFailed，而不是并进 reported 当成功。
//
// 这条守的是"不谎报成功"：上报返回 200 可能什么都没发生，只看 reported
// 会把静默丢弃读成正常轮次。
func TestCodeBuddyActivitySchedulerCountsSelfCheckFailure(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []CodeBuddyCheckinCandidate{{AccountID: 1, Name: "cb-1"}}
	// 上报成功（无 Err）但自检可疑。
	env.runner.result = CodeBuddyActivityReportResult{
		Reported: 5, Expected: 5, Verified: true, SelfCheckFailed: true,
	}

	env.at(t, "2026-09-22", "10:00")

	require.Equal(t, 1, env.runner.reportCount(), "请求确实发出去了")
	require.Equal(t, 1, env.scheduler.codeBuddyActivitySelfCheckFailedForTest(),
		"自检可疑必须单独计数，不能并进成功数")
}

// Scenario：自检健康时不计入异常。
func TestCodeBuddyActivitySchedulerHealthySelfCheckNotCountedAsFailure(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t,
		`{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []CodeBuddyCheckinCandidate{{AccountID: 1, Name: "cb-1"}}
	env.runner.result = CodeBuddyActivityReportResult{
		Reported: 5, Expected: 5, StreakDays: 3, Verified: true, SelfCheckFailed: false,
	}

	env.at(t, "2026-09-22", "10:00")

	require.Equal(t, 0, env.scheduler.codeBuddyActivitySelfCheckFailedForTest())
	require.Equal(t, 3, env.runner.result.StreakDays)
}
