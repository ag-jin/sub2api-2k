package service

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// F 通道：开学季（小程序门户域）。
//
// ## 分级：`full`（仅手动）
//
// 依据（实测定论，团队负责人 2026-09-22 采纳）：该活动的**完成判据靠伪造活跃上报**
// ——参考实现 `scripts/school_open_day_2026.py:592` 原文「触发完成判据。share 一次
// 即完成；**report 按 target-当前进度循环触发**」，`:603` 走 `make_report_events` +
// `report_events`，与夜猫子 `black_cat` 是**同一套伪造机制**。
// 且 `share-complete` 本身也是伪造"已分享到微信"这一事实。
// 结论：**要拿奖就得先点亮，点亮就得伪造**——无法只做 claim 那一半。
//
// ## §6.5「活动下线自动跳过」的要保留
//
// 这与"不做点亮"是两件事：识别到活动下线（上游 41000）时**静默跳过**——
// 不重试、不记账号故障、日志说清"活动已下线"，且**按上游错误语义判断，
// 不硬编码活动名**（判据在 `codebuddy.CodeBuddyGrowthOfflineReason`）。
//
// ## 本实现的范围界定
//
// 只做**只读盘点**（tasks + in_period）。**不做点亮**（那属 full 级的伪造动作）。
// 对外暴露的是"活动是否在期、有哪些任务、各自什么状态"，供管理端点人工决策。

// CodeBuddyGrowthSchoolStatus 开学季只读盘点结果。
type CodeBuddyGrowthSchoolStatus struct {
	// InPeriod 活动是否在进行期（上游权威判据 data.in_period）。
	InPeriod bool `json:"in_period"`
	// Offline 是否识别到"活动已下线"（上游错误语义，非硬编码活动名）。
	Offline bool `json:"offline"`
	// OfflineReason 下线原因（可读，供运维）。
	OfflineReason string `json:"offline_reason,omitempty"`
	// Tasks 任务清单（只读快照）。
	Tasks []CodeBuddyGrowthSchoolTask `json:"tasks,omitempty"`
	// Error 顶层失败。
	Error string `json:"error,omitempty"`
}

// CodeBuddyGrowthSchoolTask 单个开学季任务的只读快照。
type CodeBuddyGrowthSchoolTask struct {
	Code     string `json:"task_code"`
	Status   string `json:"status,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Target   int    `json:"target_count,omitempty"`
}

// FetchCodeBuddyGrowthSchoolStatus 只读盘点开学季活动（**不写上游**）。
//
// 分级：`preview` 部分的动作（列表/配置都是 GET）。整个通道的点亮环节才是 full，
// 本函数**不涉及点亮**，所以它本身是可以自动调用的——但当前没有自动排程在调它。
func (s *CodeBuddyAdminService) FetchCodeBuddyGrowthSchoolStatus(
	ctx context.Context,
	account *Account,
) CodeBuddyGrowthSchoolStatus {
	status := CodeBuddyGrowthSchoolStatus{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		status.Error = "not a codebuddy account"
		return status
	}

	result, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodGet, codebuddy.CodeBuddySchoolTasksPath, nil,
	)
	if err != nil {
		// §6.5：活动下线要静默跳过（不重试、不记账号故障），并说清原因。
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			status.Offline = true
			status.OfflineReason = reason
			slog.Info("codebuddy_growth.school_offline",
				"account_id", account.ID, "reason", reason)
			return status
		}
		status.Error = "fetch school tasks failed"
		slog.Warn("codebuddy_growth.school_tasks_failed", "account_id", account.ID, "error", err)
		return status
	}

	var payload struct {
		InPeriod bool `json:"in_period"`
		Tasks    []struct {
			Code     string `json:"task_code"`
			Status   string `json:"status"`
			Progress int    `json:"progress"`
			Target   int    `json:"target_count"`
		} `json:"tasks"`
	}
	if err := codeBuddyGrowthData(result, &payload); err != nil {
		status.Error = "parse school tasks failed"
		return status
	}
	status.InPeriod = payload.InPeriod
	for _, task := range payload.Tasks {
		status.Tasks = append(status.Tasks, CodeBuddyGrowthSchoolTask{
			Code:     task.Code,
			Status:   task.Status,
			Progress: task.Progress,
			Target:   task.Target,
		})
	}
	if !status.InPeriod {
		// 非进行期：正常态（活动未开始或已结束），说清楚但不报错。
		status.Offline = true
		status.OfflineReason = "活动非进行期（in_period=false）"
		slog.Info("codebuddy_growth.school_not_in_period", "account_id", account.ID)
	}
	return status
}

// --- G 通道：国际版 trial 加油包 ---

// 分级：`claim`（幂等领奖，可自动）。
// 依据：一次性领取，重复领取返回幂等码 14051（视为正常，非错误）。
// **仅 global 账号适用**——CN 无该端点（参考实现 `trial.go` 直接对非 global 报错）。

// CodeBuddyGrowthTrialResult trial 领取结果。
type CodeBuddyGrowthTrialResult struct {
	// Claimed 本次是否新领到（false = 已领过，幂等，非失败）。
	Claimed bool `json:"claimed"`
	// AlreadyClaimed 上游回幂等码（已领过）。
	AlreadyClaimed bool `json:"already_claimed"`
	// SkipReason 未动作的原因（非 global / 等）。
	SkipReason string `json:"skip_reason,omitempty"`
	// Error 顶层失败。
	Error string `json:"error,omitempty"`
}

// runCodeBuddyGrowthTrial 领取国际版 trial 加油包。
//
// 仅对 global realm 账号执行：CN 无该端点，发出去只会拿到 404
// （而 404 在本层是"路径不存在"的信号，会被误当成需要回落的编程错误）。
// 所以这里**先按 realm 拦住**，而不是靠上游报错来发现。
func (s *CodeBuddyAdminService) runCodeBuddyGrowthTrial(
	ctx context.Context,
	account *Account,
	localDay string,
) CodeBuddyGrowthTrialResult {
	result := CodeBuddyGrowthTrialResult{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}

	// trial 是一次性的（不是按天），台账记的是"已领"这一事实本身。
	if codeBuddyGrowthLedgerValue(account, ledgerTrialClaimed) != "" {
		result.AlreadyClaimed = true
		result.SkipReason = "已领过（台账记录）"
		return result
	}
	if codeBuddyAccountRealm(account) != codeBuddyRealmKindGlobal {
		result.SkipReason = "仅国际版（global）账号适用"
		return result
	}

	_, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyTrialPath, map[string]any{},
	)
	if err != nil {
		if isCodeBuddyGrowthTrialAlreadyClaimed(err) {
			// 幂等码 14051：已领过，正常态。记台账避免后续每轮都打一次。
			result.AlreadyClaimed = true
			result.SkipReason = "已领过（上游幂等码）"
			s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerTrialClaimed)
			return result
		}
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			result.SkipReason = reason
			return result
		}
		result.Error = "claim trial failed"
		slog.Warn("codebuddy_growth.trial_failed", "account_id", account.ID, "error", err)
		return result
	}

	result.Claimed = true
	s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerTrialClaimed)
	return result
}
