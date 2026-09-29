package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// F 通道：开学季（小程序门户域）的**执行链路**。
//
// ## 性质：`full`（含伪造活跃上报 + 伪造分享事实）
//
// 依据（实测定论）：该活动的**完成判据靠伪造活跃上报**——参考实现
// `scripts/school_open_day_2026.py:592` 原文「触发完成判据。share 一次即完成；
// **report 按 target-当前进度循环触发**」，`:603` 走 `make_report_events` +
// `report_events`。且 `share-complete` 本身也是伪造"已分享到微信"这一事实。
//
// **用户 2026-09-29 授权本通道进自动排程**（见 channel spec 的 AutoAuthorization）。
// 授权不改变性质，只改变政策——所以本文件顶部如实写明它在伪造什么。
//
// ## 链路（逐字对齐参考实现 `run_account`）
//
//	拉任务列表（GET /tasks）
//	  → 非进行期：静默跳过（本次不动作，不重试）
//	  → 逐个任务：
//	      manual 类（学生认证）→ 跳过（需真人，重试无意义）
//	      share 类（share_invite）→ viewed 激活 → share-complete → 回读 → claim
//	      report 类（chat_3_times / desktop_chat_1_time / expert_use）
//	          → viewed 激活 → 按 target 差额发上报 → 回读 → claim
//
// ## 与"只读盘点"的关系
//
// 原实现只有只读盘点（`FetchCodeBuddyGrowthSchoolStatus`），点亮与领奖**没做**。
// 该函数保留：它仍是"活动在不在期、有哪些任务"的权威快照，用于手动排查，
// 也被本文件的执行链路复用来拉任务列表。

// CodeBuddyGrowthSchoolRunResult 开学季一轮的结果。
type CodeBuddyGrowthSchoolRunResult struct {
	// Offline 活动不在进行期（正常态，非错误）。
	Offline bool `json:"offline"`
	// OfflineReason 不在期的可读原因。
	OfflineReason string `json:"offline_reason,omitempty"`
	// InPeriod 上游报告活动是否在进行期。
	InPeriod bool `json:"in_period"`
	// Tasks 各任务的处理结果。
	Tasks []CodeBuddyGrowthSchoolTaskRun `json:"tasks,omitempty"`
	// Claimed 本轮成功领取的任务数。
	Claimed int `json:"claimed"`
	// SkipReason 整体未动作的原因。
	SkipReason string `json:"skip_reason,omitempty"`
	// Error 顶层失败。
	Error string `json:"error,omitempty"`
}

// CodeBuddyGrowthSchoolTaskRun 单个任务的处理结果。
type CodeBuddyGrowthSchoolTaskRun struct {
	Code string `json:"task_code"`
	// Mode 动作类别：manual / share / report / unknown。
	Mode string `json:"mode"`
	// Before / After 处理前后的状态（便于运维核对"到底动没动"）。
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Reported 本条任务发出的上报条数（report 类）。
	Reported int `json:"reported,omitempty"`
	// Claimed 本条任务是否已领奖。
	Claimed bool `json:"claimed,omitempty"`
	// SkipReason 未动作的原因（正常态，非错误）。
	SkipReason string `json:"skip_reason,omitempty"`
	// Error 失败原因。
	Error string `json:"error,omitempty"`
}

// codeBuddySchoolTaskSpec 单个任务的执行规格（对齐参考实现 `KNOWN_TASKS`）。
type codeBuddySchoolTaskSpec struct {
	// Mode 动作类别。
	Mode string
	// ReportKind report 类的上报形状：mini_chat / expert / desktop_seq。
	ReportKind string
}

// codeBuddySchoolKnownTasks 任务规格表（参考实现 `KNOWN_TASKS` 的逐条移植）。
//
// ⚠️ 表里**没有**的 task_code 一律按 `unknown` 处理并跳过：上游随时可能加新任务，
// 而新任务的点亮方式未知——猜一个形状发出去只会浪费请求并可能触发风控。
var codeBuddySchoolKnownTasks = map[string]codeBuddySchoolTaskSpec{
	// 微信学生认证：需真人，不碰。
	"task_student_verify": {Mode: "manual"},
	// 分享活动给好友：share-complete 一次即完成。
	"share_invite": {Mode: "share"},
	// 与 AI 对话 3 次：小程序域 chat_request_send（带 activityId）。
	"chat_3_times": {Mode: "report", ReportKind: "mini_chat"},
	// 桌面端对话 1 次：桌面指纹 6 连事件链。
	"desktop_chat_1_time": {Mode: "report", ReportKind: "desktop_seq"},
	// 召唤开学季专家并对话：BackToSchool 分类专家 + expert_actual_use。
	"expert_use": {Mode: "report", ReportKind: "expert"},
}

// codeBuddySchoolTerminalStatuses 终态（已完成/已领）——不必再动。
var codeBuddySchoolTerminalStatuses = map[string]bool{
	"completed": true,
	"claimed":   true,
}

// RunCodeBuddyGrowthSchoolNow 执行开学季一轮：盘点 → 点亮 → 领奖。
//
// 手动端点与自动排程共用。活动下线/非进行期时**静默跳过**（不重试、不记账号故障）——
// 判据按上游错误语义（`CodeBuddyGrowthOfflineReason`），不硬编码活动名。
func (s *CodeBuddyAdminService) RunCodeBuddyGrowthSchoolNow(
	ctx context.Context,
	account *Account,
	localDay string,
) CodeBuddyGrowthSchoolRunResult {
	result := CodeBuddyGrowthSchoolRunResult{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}

	// 当日已跑过：不再重复（活动任务是幂等的，但重复点亮没有意义且增加风控暴露）。
	if codeBuddyGrowthLedgerToday(account, ledgerSchool, localDay) {
		result.SkipReason = "今日已跑过开学季"
		return result
	}

	tasks, inPeriod, offlineReason, err := s.fetchCodeBuddySchoolTasks(ctx, account)
	if err != nil {
		result.Error = "fetch school tasks failed"
		return result
	}
	if !inPeriod {
		result.Offline = true
		result.OfflineReason = offlineReason
		// 活动不在期：记台账（当天不再试），但**不算跑过**——用单独的键，
		// 这样日志能区分"真跑完"与"因为下线跳过"。
		s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerSchoolOffline)
		return result
	}
	result.InPeriod = true

	for _, task := range tasks {
		if ctx.Err() != nil {
			result.Error = "cancelled"
			break
		}
		run := s.runCodeBuddySchoolTask(ctx, account, task)
		result.Tasks = append(result.Tasks, run)
		if run.Claimed {
			result.Claimed++
		}
	}

	s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerSchool)
	return result
}

// codeBuddySchoolTask 上游任务快照。
type codeBuddySchoolTask struct {
	Code     string `json:"task_code"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Target   int    `json:"target_count"`
}

// fetchCodeBuddySchoolTasks 拉取任务列表；返回 (任务, 是否在期, 不在期原因, 错误)。
//
// 活动下线（上游 41000 等）不返回错误：它是"今天别试了"的正常态。
func (s *CodeBuddyAdminService) fetchCodeBuddySchoolTasks(
	ctx context.Context,
	account *Account,
) ([]codeBuddySchoolTask, bool, string, error) {
	call, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodGet, codebuddy.CodeBuddySchoolTasksPath, nil,
	)
	if err != nil {
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			slog.Info("codebuddy_growth.school_offline", "account_id", account.ID, "reason", reason)
			return nil, false, reason, nil
		}
		slog.Warn("codebuddy_growth.school_tasks_failed", "account_id", account.ID, "error", err)
		return nil, false, "", err
	}

	var payload struct {
		InPeriod bool                  `json:"in_period"`
		Tasks    []codeBuddySchoolTask `json:"tasks"`
	}
	if err := codeBuddyGrowthData(call, &payload); err != nil {
		return nil, false, "", err
	}
	if !payload.InPeriod {
		return payload.Tasks, false, "活动非进行期（in_period=false）", nil
	}
	return payload.Tasks, true, "", nil
}

// runCodeBuddySchoolTask 处理单个任务：按类别分派。
func (s *CodeBuddyAdminService) runCodeBuddySchoolTask(
	ctx context.Context,
	account *Account,
	task codeBuddySchoolTask,
) CodeBuddyGrowthSchoolTaskRun {
	run := CodeBuddyGrowthSchoolTaskRun{Code: task.Code, Before: strings.ToLower(task.Status)}

	if strings.TrimSpace(task.Code) == "" {
		run.Mode = "unknown"
		run.SkipReason = "任务缺少 task_code"
		return run
	}
	status := strings.ToLower(strings.TrimSpace(task.Status))
	if codeBuddySchoolTerminalStatuses[status] {
		run.Mode = "done"
		run.After = status
		run.SkipReason = "已完成/已领"
		return run
	}

	spec, known := codeBuddySchoolKnownTasks[task.Code]
	if !known {
		// 未知任务：不猜点亮方式（见 codeBuddySchoolKnownTasks 的注释）。
		run.Mode = "unknown"
		run.After = status
		run.SkipReason = "未知任务类型（未在规格表里，不猜点亮方式）"
		slog.Info("codebuddy_growth.school_unknown_task",
			"account_id", account.ID, "task_code", task.Code)
		return run
	}
	run.Mode = spec.Mode

	if spec.Mode == "manual" {
		// 学生认证等需真人环节：重试一万次也不会变成做过。
		run.After = status
		run.SkipReason = "人工环节（需真人完成，不重试）"
		return run
	}

	// 1) pending → viewed 激活（接单，H5 行为）。已在 in_progress 的跳过这步。
	if status == "pending" {
		if err := s.postCodeBuddySchoolViewed(ctx, account, task.Code); err != nil {
			if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
				run.SkipReason = reason
				run.After = status
				return run
			}
			run.Error = "viewed failed"
			slog.Warn("codebuddy_growth.school_viewed_failed",
				"account_id", account.ID, "task_code", task.Code, "error", err)
			return run
		}
		run.After = "in_progress"
	}

	// 2) 触发完成判据。
	reported, err := s.triggerCodeBuddySchoolCompletion(ctx, account, task, spec)
	run.Reported = reported
	if err != nil {
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			run.SkipReason = reason
			return run
		}
		run.Error = "trigger completion failed"
		slog.Warn("codebuddy_growth.school_trigger_failed",
			"account_id", account.ID, "task_code", task.Code, "error", err)
		return run
	}

	// 3) 回读确认（"发出去了"≠"点亮了"——与 A5 的坑 1 同一考量）。
	after, progress, err := s.refetchCodeBuddySchoolTask(ctx, account, task.Code)
	if err != nil {
		run.Error = "refetch task failed"
		return run
	}
	run.After = after
	if !codeBuddySchoolTerminalStatuses[after] && progress <= task.Progress {
		// 进度没动：可能上游还没结算，或事件被静默丢弃。不报成功，也不重试。
		run.SkipReason = "已触发但进度未变化（可能上游未结算或被静默丢弃）"
		return run
	}

	// 4) completed → claim 领奖（发抽奖机会）。
	if after == "completed" {
		if err := s.postCodeBuddySchoolClaim(ctx, account, task.Code); err != nil {
			if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
				run.SkipReason = reason
				return run
			}
			run.Error = "claim failed"
			slog.Warn("codebuddy_growth.school_claim_failed",
				"account_id", account.ID, "task_code", task.Code, "error", err)
			return run
		}
		run.Claimed = true
		run.After = "claimed"
	}
	return run
}

// triggerCodeBuddySchoolCompletion 按任务类别触发完成判据，返回发出的上报条数。
//
// 条数口径（参考实现 `run_account`）：
//   - share 类：1 次 share-complete 即完成；
//   - report 类：按 `target - 当前进度` 的差额补，且**每轮之间回读**提前终止。
//     差额口径很重要——照抄 target 全额发送会在进度已有部分时多发无用事件。
func (s *CodeBuddyAdminService) triggerCodeBuddySchoolCompletion(
	ctx context.Context,
	account *Account,
	task codeBuddySchoolTask,
	spec codeBuddySchoolTaskSpec,
) (int, error) {
	switch spec.Mode {
	case "share":
		return 0, s.postCodeBuddySchoolShareComplete(ctx, account)
	case "report":
		// desktop_seq 整链算 1 次桌面对话（参考实现原样：rounds = 1）。
		if spec.ReportKind == "desktop_seq" {
			if err := s.reportCodeBuddySchoolDesktopSeq(ctx, account); err != nil {
				return 0, err
			}
			return 1, nil
		}
		target := task.Target
		if target <= 0 {
			target = 1
		}
		rounds := target - task.Progress
		if rounds < 1 {
			rounds = 1
		}
		sent := 0
		for i := 0; i < rounds; i++ {
			if ctx.Err() != nil {
				return sent, ctx.Err()
			}
			var err error
			if spec.ReportKind == "expert" {
				err = s.reportCodeBuddySchoolExpert(ctx, account)
			} else {
				err = s.reportCodeBuddySchoolMiniChat(ctx, account)
			}
			if err != nil {
				return sent, err
			}
			sent++
			// 每轮回读：进度已达标就提前停（免得白打上游）。
			if i+1 < rounds {
				if after, progress, err := s.refetchCodeBuddySchoolTask(ctx, account, task.Code); err == nil {
					if codeBuddySchoolTerminalStatuses[after] || progress >= target {
						break
					}
				}
				if codeBuddyActivitySleep(ctx, codeBuddyActivityReportGap) != nil {
					break
				}
			}
		}
		return sent, nil
	default:
		return 0, nil
	}
}

// --- 上游动作 ---

// postCodeBuddySchoolViewed 「接任务」：POST /tasks/{code}/viewed（pending → in_progress）。
func (s *CodeBuddyAdminService) postCodeBuddySchoolViewed(
	ctx context.Context,
	account *Account,
	taskCode string,
) error {
	return s.callCodeBuddySchoolAction(
		ctx, account, http.MethodPost, codebuddy.CodeBuddySchoolTaskViewedPath(taskCode), nil,
	)
}

// postCodeBuddySchoolShareComplete 分享完成判据：POST /tasks/share-complete {channel:"wechat"}。
func (s *CodeBuddyAdminService) postCodeBuddySchoolShareComplete(
	ctx context.Context,
	account *Account,
) error {
	return s.callCodeBuddySchoolAction(
		ctx, account, http.MethodPost, codebuddy.CodeBuddySchoolShareCompletePath,
		map[string]any{"channel": "wechat"},
	)
}

// postCodeBuddySchoolClaim 领奖：POST /tasks/{code}/claim（completed → claimed）。
func (s *CodeBuddyAdminService) postCodeBuddySchoolClaim(
	ctx context.Context,
	account *Account,
	taskCode string,
) error {
	return s.callCodeBuddySchoolAction(
		ctx, account, http.MethodPost, codebuddy.CodeBuddySchoolTaskClaimPath(taskCode), nil,
	)
}

// callCodeBuddySchoolAction 发一个 school 门户域的动作请求（无响应体需求）。
//
// 用既有 `callCodeBuddyGrowth` 走端点白名单（school 域已登记），不另开 HTTP 路径——
// 那样会绕过"端点必须显式声明"的约束。
func (s *CodeBuddyAdminService) callCodeBuddySchoolAction(
	ctx context.Context,
	account *Account,
	method, path string,
	body any,
) error {
	_, err := s.callCodeBuddyGrowth(ctx, account, method, path, body)
	return err
}

// refetchCodeBuddySchoolTask 回读单个任务的状态与进度。
func (s *CodeBuddyAdminService) refetchCodeBuddySchoolTask(
	ctx context.Context,
	account *Account,
	taskCode string,
) (string, int, error) {
	tasks, _, _, err := s.fetchCodeBuddySchoolTasks(ctx, account)
	if err != nil {
		return "", 0, err
	}
	for _, task := range tasks {
		if task.Code == taskCode {
			return strings.ToLower(strings.TrimSpace(task.Status)), task.Progress, nil
		}
	}
	return "", 0, nil
}

// --- 上报（三个形状）---

// codeBuddySchoolReportEvent 上报体元素：school 事件结构 + 桌面链里的额外键。
//
// 用 map 承载而不是拼结构体：桌面 6 连的字段集与 mini 事件差异很大
// （含 `codebuddy.session_id` 这类带点的键，Go 结构体标签表达不了）。
// 类型安全由**构造侧**保证——三个构造器是唯一产出点，且都有单测钉住字段集。
type codeBuddySchoolReportEvent = map[string]any

// reportCodeBuddySchoolMiniChat 发一条小程序域对话事件（点亮 chat_3_times）。
func (s *CodeBuddyAdminService) reportCodeBuddySchoolMiniChat(
	ctx context.Context,
	account *Account,
) error {
	userID, token, err := s.codeBuddySchoolReportPrecondition(account)
	if err != nil {
		return err
	}
	now := time.Now()
	event := codebuddy.NewCodeBuddySchoolMiniChatEvent(
		userID, codebuddy.CodeBuddySchoolConversationID("wbmp", now), now,
	)
	return s.sendCodeBuddySchoolReport(ctx, account, token, []any{codeBuddySchoolEventToMap(event)})
}

// reportCodeBuddySchoolExpert 发一条专家使用事件（点亮 expert_use）。
func (s *CodeBuddyAdminService) reportCodeBuddySchoolExpert(
	ctx context.Context,
	account *Account,
) error {
	userID, token, err := s.codeBuddySchoolReportPrecondition(account)
	if err != nil {
		return err
	}
	expert := s.pickCodeBuddySchoolExpert(ctx, account)
	now := time.Now()
	event := codebuddy.NewCodeBuddySchoolExpertEvent(
		userID, "", expert.ID, expert.Title,
		codebuddy.CodeBuddySchoolConversationID("wbexp", now),
		codebuddy.CodeBuddySchoolDerivedMachineID(userID), now,
	)
	return s.sendCodeBuddySchoolReport(ctx, account, token, []any{codeBuddySchoolEventToMap(event)})
}

// reportCodeBuddySchoolDesktopSeq 发桌面端 6 连事件链（点亮 desktop_chat_1_time）。
//
// ⚠️ **必须整体上报**（一次 6 连 = 一次桌面对话），拆开发送不点亮。
// 走 copilot 域（参考实现原文：「桌面上报走 copilot 域；发 codebuddy.cn 域
// 不点亮桌面任务」），并带桌面 UA 与 X-Product 头。
func (s *CodeBuddyAdminService) reportCodeBuddySchoolDesktopSeq(
	ctx context.Context,
	account *Account,
) error {
	userID, token, err := s.codeBuddySchoolReportPrecondition(account)
	if err != nil {
		return err
	}
	now := time.Now()
	conv := codebuddy.CodeBuddySchoolConversationID("wbdesk", now)
	events := codebuddy.NewCodeBuddySchoolDesktopSequence(userID, conv, now)
	payload := make([]any, 0, len(events))
	for _, event := range events {
		// 桌面链的事件已经是 map（含带点的键，结构体表达不了），直接用。
		payload = append(payload, event)
	}
	return s.sendCodeBuddySchoolReportToHost(
		ctx, account, token, payload,
		// 桌面上报走 **chat 域**（copilot）：发 billing 域不点亮桌面任务。
		// 经统一收口取 base——测试注入缝才会生效（见 codeBuddyReportEndpointBase）。
		strings.TrimRight(codeBuddyReportEndpointBase(codeBuddyDomainChat, account), "/"),
		map[string]string{
			"X-Product":  "SaaS",
			"User-Agent": codebuddy.CodeBuddySchoolDesktopUserAgent,
		},
	)
}

// pickCodeBuddySchoolExpert 取一个开学季专家；列表拉取失败时回落已知专家。
func (s *CodeBuddyAdminService) pickCodeBuddySchoolExpert(
	ctx context.Context,
	account *Account,
) codebuddy.CodeBuddySchoolExpert {
	experts, err := s.fetchCodeBuddySchoolExperts(ctx, account)
	if err == nil && len(experts) > 0 {
		return experts[0]
	}
	if err != nil {
		slog.Info("codebuddy_growth.school_expert_list_fallback",
			"account_id", account.ID, "error", err)
	}
	// 回落：用已知专家，事件照样能点亮（参考实现同款回落）。
	return codebuddy.CodeBuddySchoolExpertFallback[0]
}

// fetchCodeBuddySchoolExperts 拉 BackToSchool 分类的专家列表。
//
// 该端点**不在** school 门户域，而在 cloud agent 域（`/v2/operation-platform/...`）。
// 用 `callCodeBuddySchoolPortalAction` 直接拼 base——因为端点白名单是为
// growth/school 门户前缀设计的，这个 market 端点是本文件独有的调用。
func (s *CodeBuddyAdminService) fetchCodeBuddySchoolExperts(
	ctx context.Context,
	account *Account,
) ([]codebuddy.CodeBuddySchoolExpert, error) {
	body := map[string]any{
		"edition_mode": "all,domestic",
		"page":         1,
		"page_size":    20,
		"sort_by":      "use_count",
		"sort_order":   "desc",
		"categories":   []string{codebuddy.CodeBuddySchoolExpertCategory},
		"expert_type":  "agent",
	}
	raw, err := s.callCodeBuddySchoolMarket(ctx, account, codebuddy.CodeBuddySchoolExpertListPath, body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Experts []struct {
			ExpertID string `json:"expert_id"`
			Name     string `json:"name"`
			Title    string `json:"title"`
		} `json:"experts"`
	}
	if err := codeBuddyGrowthData(raw, &envelope); err != nil {
		return nil, err
	}
	out := make([]codebuddy.CodeBuddySchoolExpert, 0, len(envelope.Experts))
	for _, expert := range envelope.Experts {
		if strings.TrimSpace(expert.ExpertID) == "" {
			continue
		}
		title := strings.TrimSpace(expert.Title)
		if title == "" {
			title = strings.TrimSpace(expert.Name)
		}
		out = append(out, codebuddy.CodeBuddySchoolExpert{ID: expert.ExpertID, Title: title})
	}
	return out, nil
}

// codeBuddySchoolReportPrecondition 上报前置：取 uid 与 access token。
//
// 缺任一即返回错误（而不是发一个注定被静默丢弃的请求——A5 坑 1 的直接对策）。
func (s *CodeBuddyAdminService) codeBuddySchoolReportPrecondition(
	account *Account,
) (string, string, error) {
	userID := strings.TrimSpace(account.GetCredential("uid"))
	if userID == "" {
		return "", "", errCodeBuddyActivityMissingUID
	}
	token := strings.TrimSpace(account.GetCodeBuddyAccessToken())
	if token == "" {
		return "", "", errCodeBuddyActivityMissingToken
	}
	return userID, token, nil
}

// sendCodeBuddySchoolReport 向 cloud agent 域发上报（参考实现默认 host）。
func (s *CodeBuddyAdminService) sendCodeBuddySchoolReport(
	ctx context.Context,
	account *Account,
	token string,
	events []any,
) error {
	return s.sendCodeBuddySchoolReportToHost(
		ctx, account, token, events,
		strings.TrimRight(codeBuddyReportEndpointBase(codeBuddyDomainBilling, account), "/"),
		nil,
	)
}

// sendCodeBuddySchoolReportToHost 发上报到指定 host（desktop 链走 chat 域，其余走 billing 域）。
//
// 走 `POST {host}/v2/report`，body 是事件数组。成功判据是**信封 code == 0**
// （用 `CodeBuddyNumericBizCode` 严格解析，不用 gjson .Int()——字符串形态的码
// 会被吞成 0 与真实成功无法区分）。
func (s *CodeBuddyAdminService) sendCodeBuddySchoolReportToHost(
	ctx context.Context,
	account *Account,
	token string,
	events []any,
	host string,
	extraHeaders map[string]string,
) error {
	body, err := json.Marshal(events)
	if err != nil {
		return err
	}
	endpoint := host + codebuddy.CodeBuddyActivityReportPath

	reqCtx, cancel := context.WithTimeout(ctx, codeBuddyActivityRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header[k] = []string{v}
	}
	// school 门户与小程序相关请求要求该头（参考实现 task_runner.py 任务表注明）。
	req.Header.Set("X-Client-Platform", codebuddy.CodeBuddySchoolClientPlatform)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, doErr := s.client.Do(req)
	if doErr != nil {
		return doErr
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
	}
	if code, ok := codebuddy.CodeBuddyNumericBizCode(raw); !ok || code != 0 {
		return codebuddyUpstreamBizError(raw)
	}
	return nil
}

// callCodeBuddySchoolMarket 调 cloud agent 域的 market 端点（专家列表）。
//
// 直接拼 base 而不走 `codeBuddyGrowthEndpoints` 白名单：那个白名单是为 growth /
// school 门户前缀的端点设计的，而 market 端点在**另一个前缀**上且只此处使用。
// 加进白名单会让它出现在"成长链端点"清单里，反而模糊了它的用途。
func (s *CodeBuddyAdminService) callCodeBuddySchoolMarket(
	ctx context.Context,
	account *Account,
	path string,
	body any,
) (*codeBuddyGrowthCallResult, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// 域用 school 门户域：它的 base 就是 cloud agent（`www.codebuddy.cn`），
	// 与 market 端点同主机。`callCodeBuddyGrowthOnce` 会自己拼 base。
	//
	// 测试注入 `testBaseURL` 时也走同一条路（`codeBuddyGrowthDomainBase` 会优先
	// 返回注入值），所以测试里无需为此端点特殊处理。
	return s.callCodeBuddyGrowthOnce(ctx, account, http.MethodPost, codeBuddyEndpointCandidate{
		Domain: codeBuddyDomainSchoolMiniApp,
		Path:   path,
	}, encoded)
}

// codeBuddySchoolEventToMap 把 school 事件结构体转成上报用的 map。
//
// 结构体负责**字段名与类型**的正确性（编译期可查），map 负责承载上报体。
// 转换点只有这一处。
func codeBuddySchoolEventToMap(event codebuddy.CodeBuddySchoolReportEvent) map[string]any {
	encoded, err := json.Marshal(event)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(encoded, &out)
	return out
}
