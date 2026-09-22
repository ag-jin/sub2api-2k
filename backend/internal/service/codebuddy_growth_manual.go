package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// 成长链的**手动执行入口**（A6 批 P5）。
//
// ## 为什么 full 级必须有手动入口
//
// 用户裁定的三级分级里，`full` = "仅手动，不得进自动排程"。这句话有两层含义，
// 少任何一层都不成立：
//
//  1. **不得自动**：调度器只能经 `CodeBuddyGrowthAutoSchedulableChannelKeys()`
//     取通道（full 在类型层面进不来）；
//  2. **但必须能手动**：授权明确"功能要做"，只是不做自动 tick。
//     没有手动入口 = 功能事实上不存在。
//
// 所以本文件提供 `RunCodeBuddyGrowthChannelNow`：由管理端点调用，**人显式点名**
// 要跑哪个通道。它**不读平台功能开关**——手动触发意味着人明确要求执行，
// 被"自动排程的开关"挡住会让人点了没反应且找不到原因。

// CodeBuddyGrowthChannelNowResult 手动执行单通道的结果。
type CodeBuddyGrowthChannelNowResult struct {
	// Channel 通道键。
	Channel string `json:"channel"`
	// Tier 该通道的合规级别（回执带上，让人一眼确认自己跑的是什么级别）。
	Tier string `json:"tier"`
	// AutoRunnable 该通道是否本可自动跑（false = full 级，仅手动）。
	//
	// 回执里带上这个字段是刻意的：运维手动跑一个 full 级通道时，
	// 应该能在返回值里看到"这个动作平时是不会自动跑的"，而不是靠记忆。
	AutoRunnable bool `json:"auto_runnable"`
	// Detail 通道特定结果。
	Detail any `json:"detail,omitempty"`
	// Error 失败原因（空 = 没失败）。
	Error string `json:"error,omitempty"`
}

// RunCodeBuddyGrowthChannelNow 手动执行**一个**通道（供管理端点调用）。
//
// 分级语义：
//   - 传 `full` 级通道（adopt / night_cat / school）→ 正常执行（这正是它的用途）；
//   - 传 `claim`/`preview` 级通道 → 也照跑（手动跑幂等领奖没有合规问题，
//     只是通常没必要）。
//
// 未注册的通道键 → 返回错误（而不是静默 no-op）：手动端点是人直接调用的，
// 拼错通道名必须立刻可见。
func (s *CodeBuddyAdminService) RunCodeBuddyGrowthChannelNow(
	ctx context.Context,
	account *Account,
	channelKey string,
) CodeBuddyGrowthChannelNowResult {
	result := CodeBuddyGrowthChannelNowResult{Channel: channelKey}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}
	spec, ok := codebuddy.CodeBuddyGrowthChannelSpecByKey(channelKey)
	if !ok {
		result.Error = "unknown growth channel: " + channelKey
		return result
	}
	result.Tier = spec.Tier.String()
	result.AutoRunnable = spec.Tier.AutoSchedulable()

	localDay := codeBuddyGrowthTodayLocalDay()

	switch spec.Key {
	case codebuddy.CodeBuddyGrowthChannelAdopt:
		detail := s.RunCodeBuddyGrowthAdoptNow(ctx, account, localDay)
		result.Detail = detail
		if detail.Error != "" {
			result.Error = detail.Error
		}
	case codebuddy.CodeBuddyGrowthChannelNightCat:
		detail := s.runCodeBuddyGrowthNightCatNow(ctx, account, localDay)
		result.Detail = detail
		if detail.Error != "" {
			result.Error = detail.Error
		}
	case codebuddy.CodeBuddyGrowthChannelSchool:
		// 只读盘点（点亮环节属 full，本实现不做——见 school 通道文件头）。
		detail := s.FetchCodeBuddyGrowthSchoolStatus(ctx, account)
		result.Detail = detail
		if detail.Error != "" {
			result.Error = detail.Error
		}
	case codebuddy.CodeBuddyGrowthChannelStreak,
		codebuddy.CodeBuddyGrowthChannelTravelRun,
		codebuddy.CodeBuddyGrowthChannelTravelStatus,
		codebuddy.CodeBuddyGrowthChannelTrial:
		// 手动跑自动级别的通道：走同一套分发，保证语义与自动路径一致。
		summary := s.RunCodeBuddyGrowthChannels(ctx, account, localDay, []string{spec.Key})
		result.Detail = summary
		if summary.Error != "" {
			result.Error = summary.Error
		}
	case codebuddy.CodeBuddyGrowthChannelTravel:
		// 聚合键：不代表具体动作。手动端点要求点名具体动作，避免歧义。
		result.Error = "travel 是聚合键，请指定 travel_status / travel_run / adopt"
	default:
		result.Error = "channel has no manual entry: " + channelKey
	}
	return result
}

// CodeBuddyGrowthNightCatResult 夜猫子（E 通道，full 级）结果。
type CodeBuddyGrowthNightCatResult struct {
	// Lighted 本次是否点亮（发了活跃上报）。
	Lighted bool `json:"lighted"`
	// Reports 发出的上报条数。
	Reports int `json:"reports"`
	// SkipReason 未执行的原因（非夜猫窗口 / 已有猫 等）。
	SkipReason string `json:"skip_reason,omitempty"`
	// Error 失败原因。
	Error string `json:"error,omitempty"`
}

// runCodeBuddyGrowthNightCatNow 手动执行夜猫子任务（`black_cat`）。
//
// ⚠️ **分级 `full`（仅手动）**：它没有任何"领奖"端点，唯一的动作就是发
// `chat_request_send`（`mode=night`）去**点亮任务**——纯伪造活跃上报语义
// （参考实现 `task_runner.py:1121-1127`：非夜猫窗口 skip；窗口内最多补 1 次）。
//
// **两道门都与上游一致**：
//  1. 时段：23:00–08:00 CST 跨零点窗口内才执行（非窗口期上游也只记 skip）；
//  2. 当日去重：台账挡住当天重复点亮（上游口径是"窗口内最多补 1 次"）。
//
// 窗口判定直接复用 A4 的 `WithinTimeRange`（它已处理跨零点），不重写。
func (s *CodeBuddyAdminService) runCodeBuddyGrowthNightCatNow(
	ctx context.Context,
	account *Account,
	localDay string,
) CodeBuddyGrowthNightCatResult {
	result := CodeBuddyGrowthNightCatResult{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}

	// 门 1：夜猫窗口（23:00–08:00，跨零点）。
	if !codeBuddyGrowthInNightCatWindow(time.Now()) {
		result.SkipReason = "非夜猫窗口（23:00–08:00 CST）"
		return result
	}

	// 门 2：当日已点亮（上游口径"窗口内最多补 1 次"）。
	if codeBuddyGrowthLedgerToday(account, ledgerNightCat, localDay) {
		result.SkipReason = "今日已点亮过夜猫任务"
		return result
	}

	// 夜猫子靠**活跃上报**点亮——复用 A5 的上报通道，不另写一套事件。
	// 用 night 事件形状（mode=night + GLM-5.2），它与普通 chat 事件同结构、
	// 只差两个字段，见 platform 包 NewCodeBuddyNightCatEvent。
	reports, err := s.reportCodeBuddyNightCatEvents(ctx, account)
	if err != nil {
		result.Error = "night cat report failed"
		result.Reports = reports
		return result
	}
	result.Lighted = reports > 0
	result.Reports = reports
	if result.Lighted {
		s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerNightCat)
	}
	return result
}

// codeBuddyNightCatStartHour / EndHour 夜猫窗口边界（CST）。
//
// 依据参考实现 `scripts/task_runner.py:129-134` 原文「夜猫时段（CST）23:00 - 次日 08:00」
// 与判定 `now.hour >= 23 or now.hour < 8` ——**跨零点窗口**，
// A4 的 `WithinTimeRange` 已处理该情形，直接复用不重写。
const (
	codeBuddyNightCatStartHour = 23
	codeBuddyNightCatEndHour   = 8
)

// codeBuddyGrowthInNightCatWindow 报告 now 是否落在夜猫窗口内。
func codeBuddyGrowthInNightCatWindow(now time.Time) bool {
	return WithinTimeRange(
		now,
		TimeOfDay{Hour: codeBuddyNightCatStartHour, Minute: 0},
		TimeOfDay{Hour: codeBuddyNightCatEndHour, Minute: 0},
		codeBuddyTimeZone,
	)
}

// reportCodeBuddyNightCatEvents 发夜猫子（`black_cat`）点亮事件，返回成功条数。
//
// 复用 A5 的上报通道（`sendCodeBuddyActivity`）：夜猫事件与普通 chat 事件
// **同结构、只差两个字段**（mode=night、模型换 GLM-5.2，见 platform 包
// `NewCodeBuddyNightCatEvent` 的注释），所以不另写一份 HTTP 逻辑——
// 两份必然漂移，而漂移后表现为"其中一条链路莫名 401/404"。
//
// 条数取 `CodeBuddyNightCatTargetCount`（参考实现 target=3）；
// **条间留 A5 同款间隔**（1.5s，避免秒发触发风控）；失败即停止后续条数。
func (s *CodeBuddyAdminService) reportCodeBuddyNightCatEvents(
	ctx context.Context,
	account *Account,
) (int, error) {
	userID := strings.TrimSpace(account.GetCredential("uid"))
	if userID == "" {
		// 缺 uid 的事件必被上游静默丢弃（A5 的坑 1），不发。
		return 0, nil
	}
	accessToken := strings.TrimSpace(account.GetCodeBuddyAccessToken())
	if accessToken == "" {
		return 0, nil
	}

	count := codebuddy.CodeBuddyNightCatTargetCount
	now := time.Now()
	conversationID := codebuddy.CodeBuddyActivityConversationID(now)

	sent := 0
	for i := 1; i <= count; i++ {
		if err := ctx.Err(); err != nil {
			return sent, err
		}
		requestID := codebuddy.CodeBuddyActivityRequestID(conversationID, i)
		event := codebuddy.NewCodeBuddyNightCatEvent(userID, conversationID, requestID, now)
		if !codebuddy.ValidateCodeBuddyActivityEvent(event) {
			break
		}
		if err := s.sendCodeBuddyActivity(ctx, account, event, accessToken); err != nil {
			// 失败即停止该号后续条数（与 A5 同口径）。
			return sent, err
		}
		sent++
		if i < count {
			if !codeBuddyGrowthSleep(ctx, codeBuddyActivityReportGap) {
				break
			}
		}
	}
	return sent, nil
}

// codeBuddyGrowthSleep 可取消等待（与 A5 的 codeBuddyActivitySleep 同语义，包一层便于替换）。
func codeBuddyGrowthSleep(ctx context.Context, d time.Duration) bool {
	return codeBuddyActivitySleep(ctx, d) == nil
}

// GetAccount 取账号（手动端点的入口校验用）。
//
// 手动端点与自动排程不同：排程的候选列表已带账号 ID，而端点只拿到路径参数，
// 所以需要自己取一次。**取不到就报错**（不像排程那样静默跳过）——
// 人点了这个账号却被告知"跳过"，会让人以为执行了。
func (s *CodeBuddyAdminService) GetAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errCodeBuddyGrowthRepoUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.NotFound("CODEBUDDY_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account == nil {
		return nil, infraerrors.NotFound("CODEBUDDY_ACCOUNT_NOT_FOUND", "account not found")
	}
	return account, nil
}

// RunCodeBuddyGrowthAllNow 对全部候选账号执行**可自动级别**的成长通道。
//
// 等价于"手动触发一轮排程"：通道集合走 `AutoSchedulableChannelKeys()`，
// **full 级不会带上**——哪怕调用方什么都没传。这是刻意的：
// 手动触发一轮自动排程 ≠ 跑所有东西。
func (s *CodeBuddyAdminService) RunCodeBuddyGrowthAllNow(ctx context.Context) CodeBuddyGrowthRunSummary {
	summary := CodeBuddyGrowthRunSummary{}
	if s == nil {
		summary.Error = "growth service is not configured"
		return summary
	}
	channelKeys := codebuddy.CodeBuddyGrowthAutoSchedulableChannelKeys()
	summary.Channels = channelKeys

	candidates, err := s.ListCodeBuddyGrowthCandidates(ctx, codeBuddyGrowthBatchSize)
	if err != nil {
		summary.Error = "list growth candidates failed"
		return summary
	}
	summary.Attempted = len(candidates)

	localDay := codeBuddyGrowthTodayLocalDay()
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		if candidate.SkipReason != "" {
			summary.Skipped++
			continue
		}
		account, loadErr := s.accountRepo.GetByID(ctx, candidate.AccountID)
		if loadErr != nil || account == nil {
			summary.Failed++
			continue
		}
		perAccount := s.RunCodeBuddyGrowthChannels(ctx, account, localDay, channelKeys)
		if perAccount.Error != "" {
			summary.Failed++
			continue
		}
		summary.Succeeded++
	}
	return summary
}
