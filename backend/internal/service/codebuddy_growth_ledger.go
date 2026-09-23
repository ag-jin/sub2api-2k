package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	logredact "github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

// 成长任务链的**台账**（当日去重留痕）与日期口径（批 6，A6）。
//
// ## 为什么台账要落 Account.Extra 而不是进程内 map
//
// A4（签到）与 A5（活跃上报）用的是进程内 `lastRunDate`——那对它们够用，因为
// 它们**每天只跑一轮**，重启后最坏是当天多跑一轮，而多跑的那一轮本身是幂等的
// （签到 10001 / 上报按天去重）。
//
// 成长链不同：**它是逐账号的**，而不同账号的"今天做没做"绑在不同的事实上——
// 有的号昨天补过签、有的号本月已兑 7d 档。用进程内 map 会在一万台实例上各自
// 记一份、重启即忘，然后靠上游幂等兜底去补；而上游对成长链的幂等**不是全覆盖**的
// （抽奖 `draw` 每次新 client_token，就是设计上"每次都真抽"，重复调用会真的
// 消耗次数）。所以这里的去重必须落到账号自身的持久化字段上。
//
// 复用既有通道：`Account.Extra`（`accountRepo.UpdateExtra`，签到先例
// `codebuddy_admin_service.go:463`）。**不写 credentials**——归一化白名单会吞掉
// 未知键，而且会撞 CAS 冲突（签到那处已有注释记着这件事）。
//
// ## 与 A4/A5 的日期口径一致
//
// "当日"一律按 **UTC+8 当地日期**（`PlatformFeatureLocalDate` + `codeBuddyTimeZone`）。
// 用 UTC 日期会在 UTC+8 的早上出现跨日错位：北京时间 08:00 时 UTC 还是前一天，
// 于是"今天已经跑过"被误判成"还没跑"。

// codeBuddyGrowthLedgerPrefix Extra 里所有成长链台账键的统一前缀。
//
// 加前缀而不是裸键（如 "travel_depart_date"）：Extra 是**跨功能共享**的 map
// （OpenAI 长上下文、grok 媒体资格等多处都在写），裸键迟早撞名。前缀让
// "本模块写了哪些键"可以用一次 grep 全部找出来，清理时也不会误删别人的。
const codeBuddyGrowthLedgerPrefix = "codebuddy_growth_"

// codeBuddyGrowthLedgerKey 拼一个台账键。
func codeBuddyGrowthLedgerKey(name string) string {
	return codeBuddyGrowthLedgerPrefix + name
}

// codeBuddyGrowthLocalDay 返回 now 在成长链口径（UTC+8）下的当地日期串。
func codeBuddyGrowthLocalDay(now time.Time) string {
	return PlatformFeatureLocalDate(now, codeBuddyTimeZone).Format(time.DateOnly)
}

// codeBuddyGrowthYesterday 返回 now 在 UTC+8 口径下的**昨日**日期串。
//
// ⚠️ 必须先 In(codeBuddyTimeZone) 再 AddDate：`AddDate` 按**入参 Time 所在时区**
// 做日历日减法。直接把 UTC 的 now 减一天，在 UTC+8 的 00:00–08:00 区间会算错
// （北京时间已是新的一天，而 UTC 还在前一天）。参考实现 `GrowthYesterdayDate`
// 的注释记录了同一件事（那里防的是容器时区含夏令时的情况，这里是防时区错位）。
func codeBuddyGrowthYesterday(now time.Time) string {
	return now.In(codeBuddyTimeZone).AddDate(0, 0, -1).Format(time.DateOnly)
}

// codeBuddyGrowthLedgerValue 读一个台账日期值；不存在/空返回空串。
func codeBuddyGrowthLedgerValue(account *Account, name string) string {
	if account == nil || account.Extra == nil {
		return ""
	}
	raw, ok := account.Extra[codeBuddyGrowthLedgerKey(name)]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		// Extra 经 JSON 往返后非字符串类型不可预期（bool/数字/嵌套）。
		// 台账只认字符串日期，其余一律当"没有记录"——**不能**用 fmt.Sprint
		// 兜底：那会把 `nil` 变成字符串 "nil"，进而不等于任何日期，
		// 表面上"没记录"，但也会把真脏数据伪装成合法值。
		return ""
	}
}

// codeBuddyGrowthLedgerToday 报告某台账项是否已记在当天（localDay 为 UTC+8 日期串）。
func codeBuddyGrowthLedgerToday(account *Account, name, localDay string) bool {
	return codeBuddyGrowthLedgerValue(account, name) == localDay
}

// markCodeBuddyGrowthLedger 把若干台账项记为当天，并**就地更新内存里的 Extra**。
//
// 内存更新是必须的：同一轮排程里同一个账号可能被多个步骤读同一份 `*Account`，
// 只写库不更新内存，后续步骤读到的还是旧值（"刚补完签又去补一次"）。
//
// 写库失败**只记 WARN 不回滚**——乐观占位语义与 A4/A5 一致：当轮已经真的
// 对上游做了动作，把内存标记撤掉会让下一轮 tick 再打一次上游，反而更糟。
// 代价是"进程重启 + 落库失败"同时发生时会重复一次动作，由上游幂等兜底。
func (s *CodeBuddyAdminService) markCodeBuddyGrowthLedger(
	ctx context.Context,
	account *Account,
	localDay string,
	names ...string,
) {
	if account == nil || len(names) == 0 {
		return
	}
	updates := make(map[string]any, len(names))
	for _, name := range names {
		key := codeBuddyGrowthLedgerKey(name)
		if account.Extra == nil {
			account.Extra = map[string]any{}
		}
		account.Extra[key] = localDay
		updates[key] = localDay
	}
	if s == nil || s.accountRepo == nil {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("codebuddy_growth.ledger_write_failed",
			"account_id", account.ID,
			"keys", strings.Join(names, ","),
			"error", logredact.RedactText(err.Error()))
	}
}

// --- 台账键名（集中定义，避免各处手写字符串漂移）---

const (
	// ledgerTravelDepart 当日已派出旅行（每日 1 次，上游也有 daily_limit_reached 兜底）。
	ledgerTravelDepart = "travel_depart_date"
	// ledgerTravelClaim 当日已领旅行奖励。
	ledgerTravelClaim = "travel_claim_date"
	// ledgerAdoptTried 当日已尝试领养（门槛未达时记，避免当日反复重试）。
	ledgerAdoptTried = "adopt_tried_date"
	// ledgerStreakClaimed 当日已跑过连登奖励链（补签→兑换→抽奖→礼包/补偿一轮）。
	ledgerStreakClaimed = "streak_claimed_date"
	// ledgerMakeupUse 当日已用过补签卡。
	ledgerMakeupUse = "makeup_use_date"
	// ledgerNightCat 当日已跑过夜猫子（full 级，仅手动）。
	ledgerNightCat = "night_cat_date"
	// ledgerSchool 当日已跑过开学季（full 级，仅手动）。
	ledgerSchool = "school_date"
	// ledgerSchoolLastRun 开学季最近一次运行结果（"offline:<date>" 表示当天识别到活动下线）。
	//
	// 为什么单独一项：识别到下线的意义是"**今天别再试了**"，而不是"跑过了"。
	// 混用同一个键会让日志分不清"真跑完"与"因为下线跳过"。
	ledgerSchoolOffline = "school_offline_date"
	// ledgerTrialClaimed 国际版 trial 已领（**一次性**，不是按天——所以记的是
	// "已领"这一事实本身，值取当天日期仅用于诊断"哪天领的"）。
	ledgerTrialClaimed = "trial_claimed_date"
)

// --- 补签判据的纯逻辑（可单测，不依赖 HTTP）---

// codeBuddyMakeupDecision 补签判据链的结论。
type codeBuddyMakeupDecision struct {
	// ShouldUse 是否应当发起补签。
	ShouldUse bool
	// SkipReason 不补的原因（ShouldUse=false 时非空；用于日志，非错误）。
	SkipReason string
	// TargetDate 补签目标日期（UTC+8 昨日）。
	TargetDate string
}

// decideCodeBuddyMakeup 按参考实现的判据链判定是否补签。
//
// 判据链（参考实现 `internal/scheduler/scheduler.go:makeupYesterday`）：
//
//	昨日格 score==0（漏签）→ 有补签卡（balance>0）→ POST makeup-cards/use
//
// 四个分支都返回**可解释的 SkipReason**，且**任何一个都不算失败**：
// 无判据 / 未漏签 / 无卡 / 只是不满足条件，都不该记账号故障、不该重试。
// 理由（参考实现原文）："连续天数一断就要重攒 7 天，一张卡代价远小"——
// 所以条件是"有卡且有漏签"就补，不设其他门槛。
//
// 入参：
//   - hasYesterdayCell：heatmap 里有没有昨日那一格（没有 = 上游未覆盖该日，
//     无从判断漏没漏签 → 不动）。
//   - yesterdayScore：昨日格的分值（0 = 漏签）。
//   - makeupCardBalance：补签卡余额。
func decideCodeBuddyMakeup(
	hasYesterdayCell bool,
	yesterdayScore int,
	makeupCardBalance int,
	yesterday string,
) codeBuddyMakeupDecision {
	decision := codeBuddyMakeupDecision{TargetDate: yesterday}
	if !hasYesterdayCell {
		decision.SkipReason = "活跃地图无昨日格（无漏签判据）"
		return decision
	}
	if yesterdayScore != 0 {
		decision.SkipReason = "昨日已活跃，无需补签"
		return decision
	}
	if makeupCardBalance <= 0 {
		decision.SkipReason = "昨日漏签但无补签卡"
		return decision
	}
	decision.ShouldUse = true
	return decision
}

// codeBuddyGrowthChooseRedeemTier 按连登天数挑选一个可领档位；""=无可领。
//
// 规则（参考实现 `scheduler.go:growthEligibleTier`）：
//   - 只挑"已达标（days >= 档位天数）且本月未领（status != claimed）"的档；
//   - 从**高到低**挑最高的那一个——同一天若同时满足 7d 与 28d，先领价值高的；
//   - 一天最多领一档（避免同日多写）。
//
// claimedTiers 是"本月已领"的档位集合（来自 redemption_status）。
func codeBuddyGrowthChooseRedeemTier(
	days int,
	claimedTiers map[string]bool,
	hasClaimedInfo bool,
) string {
	// 没有档位状态信息时不猜：宁可少领一次（下次排程还会来），也不要对上游
	// 发一个可能重复的 redeem（上游虽 409 幂等兜底，但白白多一次写请求）。
	if !hasClaimedInfo {
		return ""
	}
	for _, tier := range codebuddy.CodeBuddyGrowthRedeemTiers {
		required, ok := codeBuddyGrowthTierDays(tier)
		if !ok {
			continue
		}
		if days >= required && !claimedTiers[tier] {
			return tier
		}
	}
	return ""
}

// codeBuddyGrowthTierDays 档位 → 达标天数。
//
// 硬编码而不是从响应体读：档位天数（7/14/28）是**服务端常量**，响应里给的
// `tiers[]` 只保证包含"当前可见的档位"，缺档时用它算门槛会把 28d 当成"未知"。
// 这里用常量兜底，响应里的 tiers 仅用于判断 claimed 状态。
func codeBuddyGrowthTierDays(tier string) (int, bool) {
	switch tier {
	case "7d":
		return 7, true
	case "14d":
		return 14, true
	case "28d":
		return 28, true
	default:
		return 0, false
	}
}
