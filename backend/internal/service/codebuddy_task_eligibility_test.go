package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 自动任务候选资格测试（2026-10-09，spec `.scratch/codebuddy-task-eligibility/spec.md`）。
//
// 根因：签到 / 成长链 / 活跃上报三处候选过滤器复用了**流量调度资格**判据
// （!IsSchedulable() 与 TempUnschedulableUntil 未到期），导致被停调的账号
// 连"账号维护与收益动作"也一并被跳过——0 积分账号因此卡在
// "冻结 → 无法签到回血 → 解冻 → 又冻结"的循环里。
//
// 候选列表级（而非 stub 级）：直接调真实过滤函数，钉住"哪些账号进候选池"。

// Scenario：成长链候选只看凭据类判据——schedulable=false 与临时停调冷却
// （0 积分门 / 402 分级冷却）的账号**都必须在候选内且不带跳过标记**；
// 缺 uid 仍被跳过（上报会被上游静默丢弃）。
func TestListCodeBuddyGrowthCandidatesIgnoresSchedulability(t *testing.T) {
	paused := codebuddyAccount(41, "paused")
	paused.Schedulable = false

	zeroCredit := codebuddyAccount(42, "zero-credit")
	zeroCredit.Schedulable = false
	zeroCredit.TempUnschedulableUntil = ptrTime(time.Now().Add(time.Hour))
	zeroCredit.TempUnschedulableReason = "codebuddy_zero_credit"

	cooldown := codebuddyAccount(43, "cooldown")
	cooldown.TempUnschedulableUntil = ptrTime(time.Now().Add(time.Hour))
	cooldown.TempUnschedulableReason = "codebuddy_account_cooldown:402"

	noUID := codebuddyAccount(44, "no-uid")
	noUID.Credentials = map[string]any{"access_token": "at-44"}

	repo := newCodebuddyAdminTestRepo(paused, zeroCredit, cooldown, noUID)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	candidates, err := svc.ListCodeBuddyGrowthCandidates(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, candidates, 4, "四个候选都要返回（含跳过标记）")

	byID := map[int64]codeBuddyGrowthCandidate{}
	for _, candidate := range candidates {
		byID[candidate.AccountID] = candidate
	}
	for _, id := range []int64{41, 42, 43} {
		require.Empty(t, byID[id].SkipReason,
			"停调/临时停调不得挡成长任务（account %d）", id)
	}
	require.Contains(t, byID[44].SkipReason, "uid", "缺 uid 仍跳过")
}

// Scenario：活跃上报候选同口径——schedulable=false 与临时停调冷却（0 积分门 /
// 402 分级冷却）的账号**都必须在候选内且不带跳过标记**；缺 uid 仍被跳过。
func TestListCodeBuddyActivityCandidatesIgnoresSchedulability(t *testing.T) {
	paused := codebuddyAccount(45, "paused")
	paused.Schedulable = false

	zeroCredit := codebuddyAccount(46, "zero-credit")
	zeroCredit.Schedulable = false
	zeroCredit.TempUnschedulableUntil = ptrTime(time.Now().Add(time.Hour))
	zeroCredit.TempUnschedulableReason = "codebuddy_zero_credit"

	cooldown := codebuddyAccount(47, "cooldown")
	cooldown.TempUnschedulableUntil = ptrTime(time.Now().Add(time.Hour))
	cooldown.TempUnschedulableReason = "codebuddy_account_cooldown:14018"

	noUID := codebuddyAccount(48, "no-uid")
	noUID.Credentials = map[string]any{"access_token": "at-48"}

	repo := newCodebuddyAdminTestRepo(paused, zeroCredit, cooldown, noUID)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	candidates, err := svc.ListCodeBuddyActivityCandidates(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, candidates, 4, "四个候选都要返回（含跳过标记）")

	byID := map[int64]CodeBuddyCheckinCandidate{}
	for _, candidate := range candidates {
		byID[candidate.AccountID] = candidate
	}
	for _, id := range []int64{45, 46, 47} {
		require.Empty(t, byID[id].SkipReason,
			"停调/临时停调不得挡活跃上报（account %d）", id)
	}
	require.Contains(t, byID[48].SkipReason, "uid", "缺 uid 仍跳过")
}

// Scenario：账号过期守卫——admin 设的 expires_at 到期即退役，不该再签到/做任务。
//
// 核实结论（2026-10-09，见 codeBuddyTaskSkipReason 注释）：
// AutoPauseExpiredAccounts（repository/account_repo.go:2708）到期只把
// `schedulable` 置 FALSE，**从不改 status**；而 ListByPlatform 只过滤
// platform + status='active'，所以已到期账号仍会出现在候选里 —— 需要这条守卫。
// 未开启 auto_pause 的到期账号（expires_at 只是提示信息）与未到期账号都不受影响，
// 与 IsSchedulable 的过期判据同口径。
func TestCodeBuddyTaskCandidatesSkipExpiredAccounts(t *testing.T) {
	expired := codebuddyAccount(61, "expired")
	expired.AutoPauseOnExpired = true
	expired.ExpiresAt = ptrTime(time.Now().Add(-time.Minute))
	// 到期后 AccountExpiryService 已把它置为不可调度；这里还原真实形态。
	expired.Schedulable = false

	expiredNoPause := codebuddyAccount(62, "expired-no-pause")
	expiredNoPause.ExpiresAt = ptrTime(time.Now().Add(-time.Minute))

	live := codebuddyAccount(63, "live")
	live.AutoPauseOnExpired = true
	live.ExpiresAt = ptrTime(time.Now().Add(time.Hour))

	repo := newCodebuddyAdminTestRepo(expired, expiredNoPause, live)
	svc := NewCodeBuddyAdminService(nil, repo, nil)
	ctx := context.Background()

	lists := map[string]func(t *testing.T) map[int64]string{
		"checkin": func(t *testing.T) map[int64]string {
			t.Helper()
			candidates, err := svc.ListCodeBuddyCheckinCandidates(ctx, 0)
			require.NoError(t, err)
			out := make(map[int64]string, len(candidates))
			for _, candidate := range candidates {
				out[candidate.AccountID] = candidate.SkipReason
			}
			return out
		},
		"growth": func(t *testing.T) map[int64]string {
			t.Helper()
			candidates, err := svc.ListCodeBuddyGrowthCandidates(ctx, 0)
			require.NoError(t, err)
			out := make(map[int64]string, len(candidates))
			for _, candidate := range candidates {
				out[candidate.AccountID] = candidate.SkipReason
			}
			return out
		},
		"activity": func(t *testing.T) map[int64]string {
			t.Helper()
			candidates, err := svc.ListCodeBuddyActivityCandidates(ctx, 0)
			require.NoError(t, err)
			out := make(map[int64]string, len(candidates))
			for _, candidate := range candidates {
				out[candidate.AccountID] = candidate.SkipReason
			}
			return out
		},
	}

	for name, list := range lists {
		t.Run(name, func(t *testing.T) {
			reasons := list(t)
			require.Len(t, reasons, 3, "三个候选都要返回（含跳过标记）")
			require.Contains(t, reasons[61], "过期",
				"AutoPauseOnExpired 的账号到期即退役，不该再进任务池")
			require.Empty(t, reasons[62],
				"未开启 auto_pause 的过期账号只是到期提示，不该被挡")
			require.Empty(t, reasons[63], "未到期账号正常进池")
		})
	}
}
