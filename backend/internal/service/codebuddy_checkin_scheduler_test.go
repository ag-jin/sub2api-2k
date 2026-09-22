package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 签到调度器测试（A4 批 4.1/4.2）。
//
// 这两条是任务书点名的核心风险，必须钉死：
//   1. **窗口语义**：窗口内触发一次；当日已触发后窗口内不再触发；窗口外不触发。
//   2. **默认关闭**：配置缺省时调度器不发任何上游请求。
//
// 做法：直接调 tick()（不依赖真实时钟），用注入 clock 把"现在"固定到任意时刻，
// runner 用自己的 stub 记录 CheckinAll 被调用的次数——"没发上游请求"由
// 调用次数为 0 直接证明，而不是靠日志推断。

// codeBuddyCheckinRunnerStub 记录 CheckinAll 调用次数，并可注入错误。
type codeBuddyCheckinRunnerStub struct {
	calls     atomic.Int64
	lastCands int
	resp      *CodeBuddyCheckinBatchResponse
	err       error
}

func (r *codeBuddyCheckinRunnerStub) ListCodeBuddyCheckinCandidates(_ context.Context, _ int) ([]CodeBuddyCheckinCandidate, error) {
	return nil, nil
}

func (r *codeBuddyCheckinRunnerStub) CheckinAll(_ context.Context) (*CodeBuddyCheckinBatchResponse, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	if r.resp != nil {
		return r.resp, nil
	}
	return &CodeBuddyCheckinBatchResponse{}, nil
}

func (r *codeBuddyCheckinRunnerStub) callCount() int { return int(r.calls.Load()) }

// codeBuddyCheckinSchedulerTestEnv 一套可直接推进时钟的调度器测试环境。
type codeBuddyCheckinSchedulerTestEnv struct {
	scheduler *CodeBuddyCheckinScheduler
	runner    *codeBuddyCheckinRunnerStub
	repo      *platformFeatureTestRepo
	svc       *SettingService
	zone      *time.Location
	current   time.Time
}

// newCodeBuddyCheckinSchedulerTestEnv 构造环境。featuresJSON 为空 = 从未配置
// （用于验证"默认关闭"）；非空则作为落库的 platform_features 文档。
func newCodeBuddyCheckinSchedulerTestEnv(t *testing.T, featuresJSON string) *codeBuddyCheckinSchedulerTestEnv {
	t.Helper()
	repo := newPlatformFeatureTestRepo()
	if featuresJSON != "" {
		repo.vals[SettingFeatureStorageKeyForTest()] = featuresJSON
	}
	svc := newPlatformFeatureTestService(t, repo)
	runner := &codeBuddyCheckinRunnerStub{}
	scheduler := NewCodeBuddyCheckinScheduler(runner, svc)
	// 用与 codebuddy 注册时同一个时区（UTC+8），保证窗口口径一致。
	zone := codeBuddyTimeZone

	env := &codeBuddyCheckinSchedulerTestEnv{
		scheduler: scheduler,
		runner:    runner,
		repo:      repo,
		svc:       svc,
		zone:      zone,
	}
	scheduler.clock = func() time.Time { return env.current }
	return env
}

// at 把"现在"设为 UTC+8 当地的某一天某时刻，并立即 tick。
func (e *codeBuddyCheckinSchedulerTestEnv) at(t *testing.T, day, hhmm string) {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, e.zone)
	require.NoError(t, err)
	e.current = parsed
	e.scheduler.tick()
}

// tickAt 只设置时间不触发（供需要连续多次触发的用例复用同一时刻）。
func (e *codeBuddyCheckinSchedulerTestEnv) setTime(t *testing.T, day, hhmm string) {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, e.zone)
	require.NoError(t, err)
	e.current = parsed
}

// SettingFeatureStorageKeyForTest 暴露平台功能落库 key（避免测试里硬编码字符串）。
func SettingFeatureStorageKeyForTest() string { return SettingKeyPlatformFeatures }

// --- 必写测试 2：默认关闭 ---

// Scenario：**从未配置**平台功能时，调度器一个上游请求都不发。
//
// 这是「默认关闭」的**行为证据**：不是断言"配置里写着 false"，而是断言
// runner 的 CheckinAll 调用次数为 0——签到是对上游的写操作，没被显式开启
// 就不该产生任何出站流量。
func TestCodeBuddyCheckinSchedulerDefaultOffSendsNoUpstreamRequest(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t, "")

	// 在整个默认窗口内逐分钟走一遍，一次都不该触发。
	for _, hhmm := range []string{"08:59", "09:00", "09:30", "10:00", "10:59", "11:00", "12:00"} {
		env.at(t, "2026-09-22", hhmm)
	}
	require.Equal(t, 0, env.runner.callCount(),
		"未配置平台功能时不得发出任何签到请求（默认关闭）")
}

// Scenario：显式写入 enabled=false 时同样不发（"关"是显式状态，不只是缺省）。
func TestCodeBuddyCheckinSchedulerExplicitlyDisabledSendsNoUpstreamRequest(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":false,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	for _, hhmm := range []string{"09:00", "09:30", "10:59"} {
		env.at(t, "2026-09-22", hhmm)
	}
	require.Equal(t, 0, env.runner.callCount(), "显式关闭时不得发出签到请求")
}

// Scenario：开启后窗口外不发（窗口语义的"不触发"半边）。
func TestCodeBuddyCheckinSchedulerOutsideWindowSendsNoRequest(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	for _, hhmm := range []string{"00:00", "08:59", "11:00", "11:01", "23:59"} {
		env.at(t, "2026-09-22", hhmm)
	}
	require.Equal(t, 0, env.runner.callCount(), "窗口外不得触发")
}

// --- 必写测试 1：窗口语义 ---

// Scenario：窗口内触发**恰好一次**，之后同一窗口内其余 tick 全部静默。
//
// 这是用户裁定②的直接断言：签到是"窗口内执行一次"，**不是**"窗口内每分钟
// 都试一遍"（后者会持续打上游）。
func TestCodeBuddyCheckinSchedulerTriggersExactlyOnceInsideWindow(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	// 窗口内逐分钟 tick：09:00 … 10:59 共 120 次。
	for hour := 9; hour < 11; hour++ {
		for minute := 0; minute < 60; minute++ {
			env.at(t, "2026-09-22", time.Date(2026, 9, 22, hour, minute, 0, 0, env.zone).Format("15:04"))
		}
	}
	require.Equal(t, 1, env.runner.callCount(),
		"窗口内必须恰好触发一次（不是每分钟一次）")
}

// Scenario：窗口起点之前 tick 过，进入窗口后仍会触发（tick 形态能覆盖迟到启动）。
func TestCodeBuddyCheckinSchedulerTriggersWhenWindowStartsAfterEarlierTicks(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	env.at(t, "2026-09-22", "08:00")
	require.Equal(t, 0, env.runner.callCount(), "窗口前不触发")

	env.at(t, "2026-09-22", "09:00")
	require.Equal(t, 1, env.runner.callCount(), "进入窗口后触发一次")
}

// Scenario：**当日已触发后，同日窗口内不再触发**；跨到次日窗口重新触发。
//
// "当日"按功能声明的时区（UTC+8）判定，不是 UTC——否则 UTC+8 的早上会被
// 算成前一天的重复触发。
func TestCodeBuddyCheckinSchedulerDeduplicatesPerLocalDay(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)

	env.at(t, "2026-09-22", "09:30")
	require.Equal(t, 1, env.runner.callCount(), "首次进入窗口触发")

	// 同一天窗口内后续 tick 全部静默。
	env.at(t, "2026-09-22", "09:40")
	env.at(t, "2026-09-22", "10:30")
	require.Equal(t, 1, env.runner.callCount(), "当日已执行，窗口内不再触发")

	// 次日窗口内重新触发。
	env.at(t, "2026-09-23", "09:05")
	require.Equal(t, 2, env.runner.callCount(), "跨到次日应重新触发一次")

	// 再次静默。
	env.at(t, "2026-09-23", "10:00")
	require.Equal(t, 2, env.runner.callCount(), "次日也已去重")
}

// Scenario：跨日判定用**当地**日期——UTC+8 的 09:00 在 UTC 下仍是前一日，
// 若按 UTC 记日期，同一当地日的两次 tick 会被当成不同日重复触发。
func TestCodeBuddyCheckinSchedulerDedupUsesLocalDate(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":0,"minute":30},"end":{"hour":4,"minute":0}}}}`)

	// UTC+8 的 00:30 与 03:00 同属当地 2026-09-22，但 UTC 下分别是
	// 2026-09-21T16:30 与 2026-09-21T19:00（都还是 21 日）。
	env.at(t, "2026-09-22", "00:30")
	require.Equal(t, 1, env.runner.callCount())

	env.at(t, "2026-09-22", "03:00")
	require.Equal(t, 1, env.runner.callCount(),
		"同一当地日的第二次 tick 必须被去重（若按 UTC 记日期会重复触发）")

	// 当地次日 00:30 → UTC 2026-09-22T16:30，与上一条的 UTC 日不同。
	env.at(t, "2026-09-23", "00:30")
	require.Equal(t, 2, env.runner.callCount(), "当地次日应重新触发")
}

// Scenario：跨零点窗口内只触发一次（窗口落库合法，读取侧也要正确）。
func TestCodeBuddyCheckinSchedulerCrossMidnightWindowTriggersOnce(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":23,"minute":0},"end":{"hour":1,"minute":0}}}}`)

	env.at(t, "2026-09-22", "22:59")
	require.Equal(t, 0, env.runner.callCount(), "窗口前不触发")

	env.at(t, "2026-09-22", "23:00")
	require.Equal(t, 1, env.runner.callCount(), "跨零点窗口起点触发")

	env.at(t, "2026-09-22", "23:59")
	require.Equal(t, 1, env.runner.callCount(), "同窗口内不重复")
}

// Scenario：窗口内的执行失败**不回滚**当日去重——否则"持续失败"会退化成
// "每分钟都打上游"，正是用户裁定②明确反对的形态。
func TestCodeBuddyCheckinSchedulerDoesNotRetryAfterFailureSameDay(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`)
	env.runner.err = context.DeadlineExceeded

	env.at(t, "2026-09-22", "09:10")
	require.Equal(t, 1, env.runner.callCount(), "首次触发（失败）")

	env.at(t, "2026-09-22", "09:20")
	env.at(t, "2026-09-22", "10:00")
	require.Equal(t, 1, env.runner.callCount(),
		"失败当日不重试：否则会变成窗口内每分钟打上游")
}

// Scenario：配置了自定义窗口时按自定义值生效（09:00–11:00 只是默认值）。
func TestCodeBuddyCheckinSchedulerHonoursCustomWindow(t *testing.T) {
	env := newCodeBuddyCheckinSchedulerTestEnv(t,
		`{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":14,"minute":30},"end":{"hour":15,"minute":30}}}}`)

	env.at(t, "2026-09-22", "09:30")
	require.Equal(t, 0, env.runner.callCount(), "默认窗口时段不该触发（已改成 14:30）")

	env.at(t, "2026-09-22", "14:30")
	require.Equal(t, 1, env.runner.callCount(), "自定义窗口起点触发")
}

// Scenario：Start/Stop 幂等；未配置 runner 时不 panic。
func TestCodeBuddyCheckinSchedulerStartStopIdempotent(t *testing.T) {
	runner := &codeBuddyCheckinRunnerStub{}
	scheduler := NewCodeBuddyCheckinScheduler(runner, newPlatformFeatureTestService(t, newPlatformFeatureTestRepo()))

	scheduler.Start()
	scheduler.Start() // 第二次应为 no-op
	scheduler.Stop()
	scheduler.Stop() // 第二次为 no-op

	// nil / 缺依赖不得 panic。
	var nilScheduler *CodeBuddyCheckinScheduler
	nilScheduler.Start()
	nilScheduler.Stop()
	NewCodeBuddyCheckinScheduler(nil, nil).tick()
}
