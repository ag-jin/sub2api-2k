package service

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// D 通道：连登奖励链（补签 → 兑换 → 抽奖 → 礼包/补偿）。
//
// 分级：**`claim`**（幂等领奖，可自动）——**但抽奖是例外，见下方 ⚠️**。
// 依据：补签有卡才补（上游对 target_date 幂等）、兑换 409 幂等、
// 礼包/补偿"有则领"（无则业务错误，属正常态）。重复调用均无副作用。
//
// ⚠️ **抽奖 `lottery/draw` 不幂等**：参考实现 `scheduler.go:637` 原文
// 「client_token 每次 draw 必须新键（security-relevant）」——该键的用途是
// **确保每次都真抽**；抽一次消耗一次次数且**不可恢复**。它既非严格 claim，
// 也不含伪造上报，归属待团队负责人裁定（已上报）。
// 在裁定前：本实现把它放在同一轮里执行，**依赖调度侧的当日去重台账**
// （`ledgerStreakClaimed`）保证每号每天只跑一轮，不额外放大消耗。
//
// 执行顺序刻意固定：**补签 → 兑换 → 抽奖 → 礼包/补偿**。
// 理由：兑换会**送抽奖次数**（`chances_granted`），所以兑换必须在抽奖之前，
// 否则当天新得的次数要等到次日才用得上。

// CodeBuddyGrowthStreakResult 连登链一轮的结果（供日志与端点回执）。
type CodeBuddyGrowthStreakResult struct {
	// StreakDays 当前连登天数。
	StreakDays int `json:"streak_days"`
	// MakeupUsed 是否使用了补签卡。
	MakeupUsed bool `json:"makeup_used"`
	// MakeupSkipReason 未补签的原因（正常态，非错误）。
	MakeupSkipReason string `json:"makeup_skip_reason,omitempty"`
	// RedeemTier 已兑换的档位（""=无可领）。
	RedeemTier string `json:"redeem_tier,omitempty"`
	// RedeemCredit / RedeemChances 兑换到账。
	RedeemCredit  int `json:"redeem_credit"`
	RedeemChances int `json:"redeem_chances"`
	// LotteryDrawn 抽奖次数（成功抽出的次数）。
	LotteryDrawn int `json:"lottery_drawn"`
	// GiftCredit / CompensationCredit 礼包与补偿到账。
	GiftCredit         int `json:"gift_credit"`
	CompensationCredit int `json:"compensation_credit"`
	// QuietNotes 静默跳过的说明（活动下线 / 需人工 / 幂等态），供运维看。
	QuietNotes []string `json:"quiet_notes,omitempty"`
	// Error 顶层失败（拉取连登状态失败等）。非空时其余字段无意义。
	Error string `json:"error,omitempty"`
}

// runCodeBuddyGrowthStreak 执行一轮连登链。
//
// 每一步失败**都不中断后续步骤**：它们是彼此独立的领奖动作，一个失败
// （比如兑换页上游抽风）不该让礼包也领不到。只有"拉连登状态失败"才整体放弃——
// 因为补签与兑换的判据都来自它。
func (s *CodeBuddyAdminService) runCodeBuddyGrowthStreak(
	ctx context.Context,
	account *Account,
	localDay string,
) CodeBuddyGrowthStreakResult {
	result := CodeBuddyGrowthStreakResult{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}

	state, err := s.fetchCodeBuddyGrowthStreakState(ctx, account)
	if err != nil {
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			// 连登端点本身不会"活动下线"，但需人工的环节会——静默记一笔。
			result.QuietNotes = append(result.QuietNotes, reason)
			return result
		}
		result.Error = "fetch streak state failed"
		slog.Warn("codebuddy_growth.streak_state_failed",
			"account_id", account.ID, "error", err)
		return result
	}
	result.StreakDays = state.Days()

	// 1) 补签（必须在兑换之前：连登天数可能因此达标某个档位）。
	//    上游对同一 target_date 幂等，但补签卡本身是消耗品，所以先用台账挡住当日重复。
	if !codeBuddyGrowthLedgerToday(account, ledgerMakeupUse, localDay) {
		decision := s.decideStreakMakeup(ctx, account, state)
		result.MakeupSkipReason = decision.SkipReason
		if decision.ShouldUse {
			if err := s.useCodeBuddyMakeupCard(ctx, account, decision.TargetDate); err != nil {
				if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
					result.QuietNotes = append(result.QuietNotes, reason)
				} else {
					result.MakeupSkipReason = "补签请求失败"
					slog.Warn("codebuddy_growth.makeup_failed",
						"account_id", account.ID, "target_date", decision.TargetDate, "error", err)
				}
			} else {
				result.MakeupUsed = true
				// 补签成功 → 连登天数 +1（本地推算，用于选档；不必再打一次上游）。
				result.StreakDays = state.Days() + 1
			}
		}
	}

	// 2) 兑换（按连登天数挑最高可领档位）。
	tier := codeBuddyGrowthChooseRedeemTier(
		result.StreakDays, state.ClaimedTiers(), state.HasRedemptionStatus(),
	)
	if tier != "" {
		redeem, err := s.redeemCodeBuddyGrowthTier(ctx, account, tier)
		if err != nil {
			if already := isCodeBuddyGrowthRedeemAlreadyClaimed(err); already {
				// 本月已领：正常态（上游 409 幂等），不刷 WARN。
				result.QuietNotes = append(result.QuietNotes, "档位 "+tier+" 本月已领取")
			} else if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
				result.QuietNotes = append(result.QuietNotes, reason)
			} else {
				slog.Warn("codebuddy_growth.redeem_failed",
					"account_id", account.ID, "tier", tier, "error", err)
			}
		} else if redeem != nil {
			result.RedeemTier = tier
			result.RedeemCredit = redeem.CreditGranted
			result.RedeemChances = redeem.ChancesGranted
		}
	}

	// 3) 抽奖（兑换送的次数在这一轮就用掉；⚠️ 不幂等，见文件头）。
	//    只在**有次数**时才发 draw：无次数是 400 正常态，但少一次无谓请求。
	if chances, err := s.fetchCodeBuddyLotteryChances(ctx, account); err == nil && chances > 0 {
		drawn, err := s.drawCodeBuddyLotteryTimes(ctx, account, chances)
		result.LotteryDrawn = drawn
		if err != nil {
			if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
				result.QuietNotes = append(result.QuietNotes, reason)
			} else {
				slog.Warn("codebuddy_growth.lottery_failed",
					"account_id", account.ID, "drawn", drawn, "error", err)
			}
		}
	}

	// 4) 礼包 / 补偿（有则领，无则业务错误 = 正常态）。
	if credit, err := s.claimCodeBuddyGrowthGift(ctx, account); err == nil {
		result.GiftCredit = credit
	} else if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
		result.QuietNotes = append(result.QuietNotes, reason)
	}
	if credit, err := s.claimCodeBuddyGrowthCompensation(ctx, account); err == nil {
		result.CompensationCredit = credit
	} else if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
		result.QuietNotes = append(result.QuietNotes, reason)
	}

	// 跑完一轮即记台账（当日不再重复；补签另有自己的键，避免"补过了又去补"）。
	s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerStreakClaimed)
	if result.MakeupUsed {
		s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerMakeupUse)
	}
	return result
}

// decideStreakMakeup 走补签判据链（heatmap 昨日格 → 补签卡余额）。
//
// 四个分支都返回可解释的 SkipReason，且**都不是失败**：无判据 / 未漏签 /
// 无卡 / 只是不满足条件，都不该记账号故障、不该重试。
func (s *CodeBuddyAdminService) decideStreakMakeup(
	ctx context.Context,
	account *Account,
	state *codeBuddyGrowthStreakState,
) codeBuddyMakeupDecision {
	return s.decideStreakMakeupWithYesterday(ctx, account, state, codeBuddyGrowthYesterday(time.Now()))
}

// decideStreakMakeupWithYesterday 同 decideStreakMakeup，但"昨日"由调用方给定。
//
// 抽出来是为了可测：判据链的四个分支（无判据 / 未漏签 / 无卡 / 应补）都依赖
// "昨日是哪天"，而昨天随真实时钟变化——不注入日期就只能写出随日期漂移的测试。
func (s *CodeBuddyAdminService) decideStreakMakeupWithYesterday(
	ctx context.Context,
	account *Account,
	state *codeBuddyGrowthStreakState,
	yesterday string,
) codeBuddyMakeupDecision {
	cells, err := s.fetchCodeBuddyGrowthHeatmap(ctx, account)
	if err != nil {
		// 只读判据拿不到 → 静默（次日再判，无写风险）。
		return codeBuddyMakeupDecision{
			SkipReason: "活跃地图查询失败（次日再判）",
			TargetDate: yesterday,
		}
	}
	score, ok := codeBuddyHeatmapDayScore(cells, yesterday)
	if !ok {
		return codeBuddyMakeupDecision{
			SkipReason: "活跃地图无昨日格（无漏签判据）",
			TargetDate: yesterday,
		}
	}
	return decideCodeBuddyMakeup(ok, score, state.MakeupCardBalance(), yesterday)
}

// drawCodeBuddyLotteryTimes 抽 n 次奖，返回成功次数。
//
// **每次一个新 client_token**（参考实现 `scheduler.go:637` 原文要求）——
// 复用旧键可能被上游按幂等吞掉，表现为"发出去了但没抽到"。
// 中途失败即停止（不再续抽），返回已成功次数。
func (s *CodeBuddyAdminService) drawCodeBuddyLotteryTimes(
	ctx context.Context,
	account *Account,
	times int,
) (int, error) {
	drawn := 0
	for i := 0; i < times; i++ {
		if err := ctx.Err(); err != nil {
			return drawn, err
		}
		if _, err := s.drawCodeBuddyLotteryOnce(ctx, account); err != nil {
			if drawn > 0 {
				// 已抽到一部分：返回已成功次数 + 错误，让调用方如实记录。
				return drawn, err
			}
			return 0, err
		}
		drawn++
	}
	return drawn, nil
}

// codeBuddyGrowthStreakState 连登端点一次 GET 的三段切片（避免三次请求）。
type codeBuddyGrowthStreakState struct {
	streakDays        int
	makeupCardBalance int
	tier7d            string
	tier14d           string
	tier28d           string
	hasRedemption     bool
}

func (s *codeBuddyGrowthStreakState) Days() int                 { return s.streakDays }
func (s *codeBuddyGrowthStreakState) MakeupCardBalance() int    { return s.makeupCardBalance }
func (s *codeBuddyGrowthStreakState) HasRedemptionStatus() bool { return s.hasRedemption }

// ClaimedTiers 返回本月已领的档位集合。
func (s *codeBuddyGrowthStreakState) ClaimedTiers() map[string]bool {
	claimed := map[string]bool{}
	for tier, status := range map[string]string{
		"7d": s.tier7d, "14d": s.tier14d, "28d": s.tier28d,
	} {
		if strings.EqualFold(strings.TrimSpace(status), "claimed") {
			claimed[tier] = true
		}
	}
	return claimed
}
