package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// 2026-09-29 授权自动执行后的**新行为**测试。
//
// 三条授权（adopt / night_cat / school）落地后，"自动跑"不再等于"安全"——
// 恰恰因为无人看管，这些路径的**前置、去重、窗口**判据更要经得起重启与并发。
// 本文件钉住那几处。

// --- adopt 的 chat_5 自查前置 ---

// Scenario：门槛未达时，领养**自己补一轮活跃上报**再重试（不依赖外部顺序）。
//
// 这是授权自动后的必要条件：adopt 与活跃上报是两条独立链路，各有窗口与
// 当日去重，谁先跑没有保证。若领养只依赖"上报已经跑过"，多数账号会永远撞门槛。
func TestAdoptSelfHealsChat5Precondition(t *testing.T) {
	var (
		buddyInfoCalls  int
		agreementCalls  int
		firstBuddyCalls int
		reportCalls     int
	)
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codebuddy.CodeBuddyBuddyInfoPath:
			buddyInfoCalls++
			// 无猫 → 需要领养。
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case codebuddy.CodeBuddyBuddyAgreementPath:
			agreementCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case codebuddy.CodeBuddyBuddyFirstPath:
			firstBuddyCalls++
			// 第一次撞门槛，第二次（补报之后）成功。
			if firstBuddyCalls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"message":"first_buddy task not completed yet"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":300,"energy":10}}`))
		case codebuddy.CodeBuddyActivityReportPath:
			reportCalls++
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthAdoptNow(context.Background(), account, "2026-09-29")

	require.True(t, result.Adopted, "补报前置后应领养成功（走通了自查前置）")
	require.Equal(t, 300, result.Credit)
	require.Greater(t, reportCalls, 0, "应发出活跃上报以补足 chat_5 前置")
	require.Equal(t, 2, firstBuddyCalls, "应在补报后重试一次领养")
	require.Equal(t, 1, buddyInfoCalls)
	require.Equal(t, 1, agreementCalls)
}

// Scenario：门槛未达**且当天已报过** → 不再重复补报（风控口径：每号每天 1 次）。
//
// 这条是"共享台账键"的守门：若领养这里不看台账，调度器已报过之后它还会再报一轮，
// 两条链路都开着就变成每号每天 2 次上报。
func TestAdoptDoesNotReReportWhenAlreadyReportedToday(t *testing.T) {
	var reportCalls int
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codebuddy.CodeBuddyBuddyInfoPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case codebuddy.CodeBuddyBuddyAgreementPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case codebuddy.CodeBuddyBuddyFirstPath:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"message":"first_buddy task not completed yet"}`))
		case codebuddy.CodeBuddyActivityReportPath:
			reportCalls++
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	account := newGrowthTestAccount(1)
	// 预置：今天已由（调度器或别人）报过。
	account.Extra = map[string]any{
		codeBuddyGrowthLedgerKey(ledgerActivity): "2026-09-29",
	}

	result := svc.RunCodeBuddyGrowthAdoptNow(context.Background(), account, "2026-09-29")

	require.True(t, result.ThresholdNotMet, "门槛未达应如实报出")
	require.Equal(t, 0, reportCalls,
		"当天已报过就不该再补报（风控口径：每号每天 1 次）")
	// 当日不再重试的台账应被记下。
	require.Equal(t, "2026-09-29", codeBuddyGrowthLedgerValue(account, ledgerAdoptTried))
}

// Scenario：补报成功 → 记**共享的**活跃台账（两条链路不会各报一轮）。
func TestAdoptMarksSharedActivityLedgerAfterReporting(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codebuddy.CodeBuddyBuddyInfoPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case codebuddy.CodeBuddyBuddyAgreementPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case codebuddy.CodeBuddyBuddyFirstPath:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"message":"first_buddy task not completed yet"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	})
	account := newGrowthTestAccount(1)

	_ = svc.RunCodeBuddyGrowthAdoptNow(context.Background(), account, "2026-09-29")

	require.Equal(t, "2026-09-29",
		codeBuddyGrowthLedgerValue(account, ledgerActivity),
		"补报后应记共享的活跃台账键（与活跃上报调度器同键）")
}

// Scenario：已有猫 → 跳过，不发任何写请求（幂等自愈）。
func TestAdoptSkipsWhenBuddyExists(t *testing.T) {
	var writes int
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyBuddyInfoPath {
			// 有猫（上游从不发 id，只有别的字段）。
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"name":"小橘","rarity":"SSR"}}}`))
			return
		}
		writes++
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthAdoptNow(context.Background(), account, "2026-09-29")

	require.False(t, result.Adopted)
	require.Contains(t, result.SkipReason, "已有猫")
	require.Equal(t, 0, writes, "已有猫不该再发协议/领养请求")
}

// --- 活跃上报的账号级当日去重 ---

// Scenario：**进程重启后**不会对同一账号重复上报（账号级台账挡住）。
//
// 这是授权自动后新增的去重层：进程内 lastRunDate 重启即忘，
// 而风控口径要求"每号每天 1 次"。
func TestActivitySchedulerSkipsAccountAlreadyReportedToday(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t, activityEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []CodeBuddyCheckinCandidate{{AccountID: 1, Name: "cb-1"}}

	// 预置：今天已报过（模拟"重启前那一轮报的"）。
	env.account(1).Extra = map[string]any{
		codeBuddyGrowthLedgerKey(ledgerActivity): "2026-09-29",
	}

	env.at(t, "2026-09-29", "10:30")

	require.Equal(t, 0, env.runner.reportCount(),
		"当天已报过的账号不该再报（进程重启也不重复）")
	require.Equal(t, 1, env.runner.rounds(),
		"窗口内仍应执行一轮（候选列表照查，只是账号被跳过）")
}

// Scenario：报成功 → 写账号级当日台账（下次 tick / 重启都挡得住）。
func TestActivitySchedulerMarksLedgerAfterReporting(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t, activityEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []CodeBuddyCheckinCandidate{{AccountID: 1, Name: "cb-1"}}

	env.at(t, "2026-09-29", "10:30")

	require.Equal(t, 1, env.runner.reportCount(), "应真的报了")
	require.Equal(t, "2026-09-29",
		codeBuddyGrowthLedgerValue(env.account(1), ledgerActivity),
		"报成功后应写当日台账")
	// 台账确实经 repo 落库（不只改内存）。
	require.NotEmpty(t, env.extraUpdates(), "应经仓储落库")
}

// Scenario：手动触发（RunActivityNow）**绕开**账号级去重——人点了就该真发。
func TestActivityRunNowBypassesAccountLedger(t *testing.T) {
	env := newCodeBuddyActivitySchedulerTestEnv(t, activityEnabledWindow,
		newCodeBuddyActivityAccount(1, "u-1", "t-1"))
	env.runner.candidates = []CodeBuddyCheckinCandidate{{AccountID: 1, Name: "cb-1"}}
	env.account(1).Extra = map[string]any{
		codeBuddyGrowthLedgerKey(ledgerActivity): "2026-09-29",
	}

	env.scheduler.RunActivityNow(context.Background())

	require.Equal(t, 1, env.runner.reportCount(),
		"手动触发应真的发出去（不被当日台账挡住，否则人点了没反应）")
}

// --- 夜猫子的夜间窗口（通道自身判据）---

// Scenario：非夜猫窗口 → 通道自身返回 skip，且不发上报。
func TestNightCatSkipsOutsideNightWindow(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)

	// 显式构造两个时刻，钉住窗口判据本身。
	day := parseCST(t, "2026-09-29", "12:00")
	night := parseCST(t, "2026-09-29", "23:30")
	require.False(t, codeBuddyGrowthInNightCatWindow(day), "日间应在窗口外")
	require.True(t, codeBuddyGrowthInNightCatWindow(night), "23:30 应在窗口内")

	// 通道侧行为：非窗口时返回 skip 且不发请求。
	// 用"当前真实时刻"调用——若恰在窗口内则跳过该断言（不误报）。
	if !codeBuddyGrowthInNightCatWindow(time.Now()) {
		result := svc.runCodeBuddyGrowthNightCatNow(context.Background(), account, "2026-09-29")
		require.Contains(t, result.SkipReason, "非夜猫窗口")
		require.Empty(t, recorder.snapshot(), "非窗口不该发上报")
	}
}

// --- school 任务的分类与离线守卫 ---

// Scenario：活动非进行期 → 静默跳过（不点亮、不领奖、不记账号故障）。
func TestSchoolSkipsWhenNotInPeriod(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddySchoolTasksPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"in_period":false,"tasks":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthSchoolNow(context.Background(), account, "2026-09-29")

	require.True(t, result.Offline, "非进行期应识别为离线")
	require.Empty(t, result.Error, "活动不在期不是错误")
	require.Equal(t, 0, result.Claimed)
	// 只应有那一次任务列表请求（没有 viewed/claim/report）。
	require.Len(t, recorder.snapshot(), 1, "非进行期应只拉一次任务列表就停手")
}

// Scenario：人工环节（学生认证）→ 跳过，不尝试点亮。
func TestSchoolSkipsManualOnlyTask(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddySchoolTasksPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"in_period":true,"tasks":[` +
				`{"task_code":"task_student_verify","status":"pending","progress":0,"target_count":1}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthSchoolNow(context.Background(), account, "2026-09-29")

	require.True(t, result.InPeriod)
	require.Len(t, result.Tasks, 1)
	require.Equal(t, "manual", result.Tasks[0].Mode)
	require.Contains(t, result.Tasks[0].SkipReason, "人工环节")
	require.Equal(t, 0, result.Claimed)
}

// Scenario：share 类任务走 viewed → share-complete → 回读 → claim 全链路。
func TestSchoolShareTaskCompletesAndClaims(t *testing.T) {
	var (
		viewedCalls, shareCalls, claimCalls int
		refetched                           bool
	)
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codebuddy.CodeBuddySchoolTasksPath:
			// 首次列出 pending；回读时变 completed（模拟上游结算）。
			if refetched {
				_, _ = w.Write([]byte(`{"code":0,"data":{"in_period":true,"tasks":[` +
					`{"task_code":"share_invite","status":"completed","progress":1,"target_count":1}]}}`))
				return
			}
			refetched = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"in_period":true,"tasks":[` +
				`{"task_code":"share_invite","status":"pending","progress":0,"target_count":1}]}}`))
		case codebuddy.CodeBuddySchoolTaskViewedPath("share_invite"):
			viewedCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case codebuddy.CodeBuddySchoolShareCompletePath:
			shareCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case codebuddy.CodeBuddySchoolTaskClaimPath("share_invite"):
			claimCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"chance_granted":1}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthSchoolNow(context.Background(), account, "2026-09-29")

	require.Equal(t, 1, viewedCalls, "pending 应先 viewed 激活")
	require.Equal(t, 1, shareCalls, "share 类应发 share-complete")
	require.Equal(t, 1, claimCalls, "completed 后应 claim 领奖")
	require.Equal(t, 1, result.Claimed)
	require.Equal(t, "claimed", result.Tasks[0].After)
	// 当日台账应记下（当天不再跑）。
	require.Equal(t, "2026-09-29", codeBuddyGrowthLedgerValue(account, ledgerSchool))
}

// Scenario：report 类任务按 target-进度差额发上报（不是每次全额）。
func TestSchoolReportTaskSendsOnlyTheDeficit(t *testing.T) {
	var reportCalls, refetchCount int
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codebuddy.CodeBuddySchoolTasksPath:
			refetchCount++
			// 首次：进度 1/3 → 差额 2。回读时直接终态，让循环提前结束。
			status, progress := "in_progress", 1
			if refetchCount > 1 {
				status, progress = "completed", 3
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"in_period":true,"tasks":[` +
				`{"task_code":"chat_3_times","status":"` + status + `","progress":` +
				itoa(progress) + `,"target_count":3}]}}`))
		case codebuddy.CodeBuddyActivityReportPath:
			reportCalls++
			_, _ = w.Write([]byte(`{"code":0}`))
		case codebuddy.CodeBuddySchoolTaskClaimPath("chat_3_times"):
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthSchoolNow(context.Background(), account, "2026-09-29")

	// 差额 = 3 - 1 = 2；但第一轮后回读已 completed → 提前终止，故 ≤2 且 ≥1。
	require.GreaterOrEqual(t, reportCalls, 1, "应发上报")
	require.LessOrEqual(t, reportCalls, 2, "只应发差额条数（不是 target 全额 3）")
	require.Equal(t, 1, result.Claimed)
}

// Scenario：未知任务类型 → 不猜点亮方式，跳过（上游随时可能加新任务）。
func TestSchoolSkipsUnknownTaskType(t *testing.T) {
	var reportCalls int
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case codebuddy.CodeBuddySchoolTasksPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"in_period":true,"tasks":[` +
				`{"task_code":"brand_new_task_2027","status":"pending","progress":0,"target_count":1}]}}`))
		case codebuddy.CodeBuddyActivityReportPath:
			reportCalls++
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthSchoolNow(context.Background(), account, "2026-09-29")

	require.Len(t, result.Tasks, 1)
	require.Equal(t, "unknown", result.Tasks[0].Mode)
	require.Contains(t, result.Tasks[0].SkipReason, "未知任务类型")
	require.Equal(t, 0, reportCalls, "未知任务不该猜形状发上报")
}

// Scenario：当日已跑过 → 直接跳过，不发任何请求。
func TestSchoolSkipsWhenAlreadyRunToday(t *testing.T) {
	var calls int
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	account := newGrowthTestAccount(1)
	account.Extra = map[string]any{
		codeBuddyGrowthLedgerKey(ledgerSchool): "2026-09-29",
	}

	result := svc.RunCodeBuddyGrowthSchoolNow(context.Background(), account, "2026-09-29")

	require.Contains(t, result.SkipReason, "今日已跑过")
	require.Equal(t, 0, calls, "当日已跑过不该再发任何请求")
}

// itoa 极简整数转字符串（避免为测试引入 strconv）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// activityEnabledWindow 活跃上报开启且指定窗口的 settings JSON（10:00–11:00）。
const activityEnabledWindow = `{"codebuddy":{"activity":{"enabled":true,"start":{"hour":10,"minute":0},"end":{"hour":11,"minute":0}}}}`
