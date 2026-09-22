package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// 成长链的**具体上游动作**（A6 批 P4）。
//
// 这一层是 P3（`callCodeBuddyGrowth`）之上的薄封装：每个函数对应一个上游端点，
// 负责请求体形状、响应字段解析、以及"哪些业务码算正常态"。
// 业务级的"要不要发"留在各通道文件（streak / travel / school / trial）。
//
// 所有函数返回的 error 若为 `*codeBuddyGrowthUpstreamError`，调用方可用
// `codeBuddyGrowthQuietReason` / 各 is* 判定是否属正常态。

// --- 连登状态 ---

// fetchCodeBuddyGrowthStreakState 一次 GET 连登端点，取三段切片。
//
// 为什么一次取完：`/activity/growth/streak` 的响应体同时含
// `streak.days` / `makeup_cards` / `redemption_status`（参考实现
// `GrowthStreakWithCards` 与 `GrowthRewardState` 注释都写了"同一响应体"）。
// 分三次请求既浪费又可能读到不一致的快照。
func (s *CodeBuddyAdminService) fetchCodeBuddyGrowthStreakState(
	ctx context.Context,
	account *Account,
) (*codeBuddyGrowthStreakState, error) {
	result, err := s.callCodeBuddyGrowth(ctx, account, http.MethodGet, codebuddy.CodeBuddyGrowthStreakPath, nil)
	if err != nil {
		return nil, err
	}
	state := &codeBuddyGrowthStreakState{}
	var payload struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
		MakeupCards struct {
			Balance int `json:"balance"`
		} `json:"makeup_cards"`
		RedemptionStatus struct {
			Tier7d  string `json:"tier_7d_status"`
			Tier14d string `json:"tier_14d_status"`
			Tier28d string `json:"tier_28d_status"`
		} `json:"redemption_status"`
	}
	if err := codeBuddyGrowthData(result, &payload); err != nil {
		return nil, err
	}
	state.streakDays = payload.Streak.Days
	state.makeupCardBalance = payload.MakeupCards.Balance
	state.tier7d = payload.RedemptionStatus.Tier7d
	state.tier14d = payload.RedemptionStatus.Tier14d
	state.tier28d = payload.RedemptionStatus.Tier28d
	// 有 redemption_status 段才算"拿到档位信息"；缺失时**不猜**（见
	// codeBuddyGrowthChooseRedeemTier：没有信息就宁可少领一次，也别发可能重复的 redeem）。
	state.hasRedemption = payload.RedemptionStatus.Tier7d != "" ||
		payload.RedemptionStatus.Tier14d != "" ||
		payload.RedemptionStatus.Tier28d != ""
	return state, nil
}

// codeBuddyHeatmapDayScore 取某日分数（在 cells 里找）。
func codeBuddyHeatmapDayScore(cells []codebuddy.HeatmapCell, date string) (int, bool) {
	return codebuddy.HeatmapDayScore(cells, date)
}

// fetchCodeBuddyGrowthHeatmap 拉活跃地图热力格。
func (s *CodeBuddyAdminService) fetchCodeBuddyGrowthHeatmap(
	ctx context.Context,
	account *Account,
) ([]codebuddy.HeatmapCell, error) {
	result, err := s.callCodeBuddyGrowth(ctx, account, http.MethodGet, codebuddy.CodeBuddyGrowthHeatmapPath, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Cells []codebuddy.HeatmapCell `json:"cells"`
	}
	if err := codeBuddyGrowthData(result, &payload); err != nil {
		return nil, err
	}
	return payload.Cells, nil
}

// useCodeBuddyMakeupCard 对指定日期补签。
func (s *CodeBuddyAdminService) useCodeBuddyMakeupCard(
	ctx context.Context,
	account *Account,
	targetDate string,
) error {
	_, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyGrowthMakeupCardUsePath,
		map[string]any{"target_date": targetDate},
	)
	return err
}

// redeemCodeBuddyGrowthTier 兑换指定连登档位。
//
// `client_token` **每次新生成**（参考实现 `growth_reward.go` 原文：
// "每次调用必须新键，不应复用，否则上游可能按幂等键去重吞掉本次领取"）。
func (s *CodeBuddyAdminService) redeemCodeBuddyGrowthTier(
	ctx context.Context,
	account *Account,
	tier string,
) (*codeBuddyGrowthRedeemResult, error) {
	result, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyGrowthRedeemPath,
		map[string]any{"tier": tier, "client_token": codeBuddyGrowthClientToken("redeem-" + tier)},
	)
	if err != nil {
		return nil, err
	}
	redeem := &codeBuddyGrowthRedeemResult{}
	if err := codeBuddyGrowthData(result, redeem); err != nil {
		// 回执字段解析失败不视为失败：领取可能已经成功，按 0 记日志即可
		// （参考实现同口径："回执字段缺失不视为失败"）。
		return &codeBuddyGrowthRedeemResult{}, nil
	}
	return redeem, nil
}

// codeBuddyGrowthRedeemResult 兑换回执。
type codeBuddyGrowthRedeemResult struct {
	CreditGranted  int `json:"credit_granted"`
	EnergyGranted  int `json:"energy_granted"`
	CardsGranted   int `json:"cards_granted"`
	ChancesGranted int `json:"chances_granted"`
}

// fetchCodeBuddyLotteryChances 查抽奖次数余额。
func (s *CodeBuddyAdminService) fetchCodeBuddyLotteryChances(
	ctx context.Context,
	account *Account,
) (int, error) {
	result, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodGet, codebuddy.CodeBuddyGrowthLotteryChancesPath, nil,
	)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Balance int `json:"balance"`
	}
	if err := codeBuddyGrowthData(result, &payload); err != nil {
		return 0, err
	}
	return payload.Balance, nil
}

// drawCodeBuddyLotteryOnce 抽一次奖。
//
// ⚠️ `client_token` **每次都是新键**（参考实现 `scheduler.go:637` 原文
// 「client_token 每次 draw 必须新键（security-relevant）」）——该键的目的是
// **确保每次都真抽**，复用它会被上游按幂等吞掉，表现为"发出去了但没抽到"。
func (s *CodeBuddyAdminService) drawCodeBuddyLotteryOnce(
	ctx context.Context,
	account *Account,
) (*codeBuddyGrowthLotteryResult, error) {
	result, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyGrowthLotteryDrawPath,
		map[string]any{"client_token": codeBuddyGrowthClientToken("draw")},
	)
	if err != nil {
		return nil, err
	}
	draw := &codeBuddyGrowthLotteryResult{}
	// 奖品字段缺失不视为失败（参考实现同口径）。
	_ = codeBuddyGrowthData(result, draw)
	return draw, nil
}

// codeBuddyGrowthLotteryResult 单次抽奖结果。
type codeBuddyGrowthLotteryResult struct {
	PrizeCode    string `json:"prize_code"`
	PrizeName    string `json:"prize_name"`
	PrizeType    string `json:"prize_type"`
	CreditAmount int    `json:"credit_amount"`
}

// claimCodeBuddyGrowthGift 领新手礼包（每号一次）。
func (s *CodeBuddyAdminService) claimCodeBuddyGrowthGift(
	ctx context.Context,
	account *Account,
) (int, error) {
	return s.claimCodeBuddyGrowthBillingCredit(ctx, account, codebuddy.CodeBuddyClaimGiftPath)
}

// claimCodeBuddyGrowthCompensation 领活动补偿（有则领）。
func (s *CodeBuddyAdminService) claimCodeBuddyGrowthCompensation(
	ctx context.Context,
	account *Account,
) (int, error) {
	return s.claimCodeBuddyGrowthBillingCredit(ctx, account, codebuddy.CodeBuddyClaimCompensationPath)
}

// claimCodeBuddyGrowthBillingCredit billing 域领取类的公共实现：POST → data.credit。
func (s *CodeBuddyAdminService) claimCodeBuddyGrowthBillingCredit(
	ctx context.Context,
	account *Account,
	path string,
) (int, error) {
	result, err := s.callCodeBuddyGrowth(ctx, account, http.MethodPost, path, map[string]any{})
	if err != nil {
		return 0, err
	}
	var payload struct {
		Credit int `json:"credit"`
	}
	// 回执字段缺失不视为失败（调用方按 0 记日志）。
	_ = codeBuddyGrowthData(result, &payload)
	return payload.Credit, nil
}

// codeBuddyGrowthClientToken 生成幂等键：`<prefix>-<nanos>-<n>`。
//
// 形态不必与官方的 uuid 一致——上游只把它当"每次独立的幂等键"，不校验格式
// （参考实现注释：SPA 的 fallback 分支也不是严格 uuid）。
// 用时间戳 + 进程内自增保证同一纳秒内多次调用也不撞。
var codeBuddyGrowthTokenCounter uint64

func codeBuddyGrowthClientToken(prefix string) string {
	codeBuddyGrowthTokenCounter++
	return prefix + "-" + time.Now().UTC().Format("20060102T150405.000000000") +
		"-" + codeBuddyUintToString(codeBuddyGrowthTokenCounter)
}

// codeBuddyUintToString 十进制渲染（避免为一个 token 引入 strconv 之外的格式差异）。
func codeBuddyUintToString(value uint64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// isCodeBuddyGrowthRedeemAlreadyClaimed 判定"本月已领取该档位"（上游 409 幂等态）。
//
// 双判据：数值码 409 + 文案含 duplicate/已领取。为什么同时看文案：
// 上游对这类幂等态有时用 HTTP 409 承载、有时塞业务码，两处都出现过。
// 匹配的是我们**自己也认识的语义词**，不是靠文案推断类别（L2 警告的是后者）。
func isCodeBuddyGrowthRedeemAlreadyClaimed(err error) bool {
	var upstreamErr *codeBuddyGrowthUpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if upstreamErr.StatusCode == http.StatusConflict {
		return true
	}
	lower := strings.ToLower(upstreamErr.Message)
	return strings.Contains(lower, "duplicate") || strings.Contains(upstreamErr.Message, "已领取")
}
