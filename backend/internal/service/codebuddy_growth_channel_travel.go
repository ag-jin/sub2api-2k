package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// C 通道：猫猫旅行（status / depart / claim）+ 领养（buddy/first）。
//
// ## 分级按**动作**拆（团队负责人 2026-09-22 裁定）
//
//   - `travel_status`（查状态）      → `preview`，可自动，只读
//   - `travel_run`（派出 + 到站领奖） → `claim`，  可自动，上游幂等
//   - `adopt`（领养 buddy/first）    → `full`，   **仅手动**
//
// 为什么领养单独是 full：它的前置 `chat_5` 只能靠伪造活跃上报满足，且到达门槛后
// **直接送 300 分**——"猫会自己出现"不是真实发生的事。所以它必须走手动入口
// （`RunCodeBuddyGrowthAdoptNow`），**不得**被任何自动排程调用。

// CodeBuddyGrowthTravelResult 旅行一段的结果。
type CodeBuddyGrowthTravelResult struct {
	// State 上游报告的状态：idle / traveling / arrived。
	State string `json:"state"`
	// DailyLimitReached 今日已派出过（上游标记，自然日 00:00 CST 重置）。
	DailyLimitReached bool `json:"daily_limit_reached"`
	// Departed 本次是否派出了。
	Departed bool `json:"departed"`
	// Claimed 本次是否领了到站奖励。
	Claimed bool `json:"claimed"`
	// RewardCredit 本次领取的奖励积分。
	RewardCredit int `json:"reward_credit"`
	// SkipReason 未动作的原因（正常态，非错误）。
	SkipReason string `json:"skip_reason,omitempty"`
	// QuietNotes 静默跳过说明（活动下线 / 已领等）。
	QuietNotes []string `json:"quiet_notes,omitempty"`
	// Error 顶层失败。非空时其余字段无意义。
	Error string `json:"error,omitempty"`
}

// runCodeBuddyGrowthTravel 跑一轮旅行闭环：status → depart → claim。
//
// 闭环形态（参考实现 `travel.go` + `scheduler`）：
//
//	status（拿 state / daily_limit_reached / record_id）
//	  → 若 idle 且未达当日上限 → depart
//	  → 若已 arrived 且有 record_id → claim（**必须带 record_id**）
//
// 三段都是幂等领奖类动作（`travel_run`，claim 级）。派出受上游 `daily_limit_reached`
// 与本地台账双重约束；领奖按 record_id 天然幂等。
func (s *CodeBuddyAdminService) runCodeBuddyGrowthTravel(
	ctx context.Context,
	account *Account,
	localDay string,
) CodeBuddyGrowthTravelResult {
	result := CodeBuddyGrowthTravelResult{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}

	state, err := s.fetchCodeBuddyTravelState(ctx, account)
	if err != nil {
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			result.QuietNotes = append(result.QuietNotes, reason)
			return result
		}
		result.Error = "fetch travel state failed"
		slog.Warn("codebuddy_growth.travel_state_failed", "account_id", account.ID, "error", err)
		return result
	}
	result.State = state.State
	result.DailyLimitReached = state.DailyLimitReached

	// 1) 已到站 → 领奖（须带 record_id）。先领奖再考虑派出：
	//    到站状态说明上一次行程已完成，先把它结算掉，避免 record 挂在那里。
	if strings.EqualFold(state.State, codebuddy.CodeBuddyTravelStateArrived) {
		if state.RecordID <= 0 {
			// 到站但没有 record_id：上游数据不一致，不能瞎领（claim 必带该字段）。
			result.SkipReason = "到站但缺 record_id（上游数据异常）"
		} else if codeBuddyGrowthLedgerToday(account, ledgerTravelClaim, localDay) {
			result.SkipReason = "今日已领过旅行奖励"
		} else {
			reward, err := s.claimCodeBuddyTravel(ctx, account, state.RecordID)
			if err != nil {
				if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
					result.QuietNotes = append(result.QuietNotes, reason)
				} else {
					result.SkipReason = "领奖失败"
					slog.Warn("codebuddy_growth.travel_claim_failed",
						"account_id", account.ID, "record_id", state.RecordID, "error", err)
				}
			} else {
				result.Claimed = true
				result.RewardCredit = reward
				s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerTravelClaim)
			}
		}
	}

	// 2) idle 且当日未派出 → 派出。
	//    上游 daily_limit_reached 是权威判据；本地台账是防重复的第二道闸
	//    （进程重启后台账仍在，比只靠上游标记更早挡住）。
	if strings.EqualFold(state.State, codebuddy.CodeBuddyTravelStateIdle) && !result.Claimed {
		switch {
		case state.DailyLimitReached:
			result.SkipReason = "今日已达派出上限（上游标记）"
		case codeBuddyGrowthLedgerToday(account, ledgerTravelDepart, localDay):
			result.SkipReason = "今日已派出过"
		default:
			if err := s.departCodeBuddyTravel(ctx, account); err != nil {
				if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
					result.QuietNotes = append(result.QuietNotes, reason)
				} else {
					result.SkipReason = "派出失败"
					slog.Warn("codebuddy_growth.travel_depart_failed",
						"account_id", account.ID, "error", err)
				}
			} else {
				result.Departed = true
				s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerTravelDepart)
			}
		}
	}
	return result
}

// --- 上游动作 ---

// codeBuddyTravelState 旅行状态。
type codeBuddyTravelState struct {
	State             string `json:"state"`
	DailyLimitReached bool   `json:"daily_limit_reached"`
	RecordID          int64  `json:"record_id"`
	RewardCredit      int    `json:"reward_credit"`
}

// fetchCodeBuddyTravelState 查旅行状态（只读，preview 级）。
func (s *CodeBuddyAdminService) fetchCodeBuddyTravelState(
	ctx context.Context,
	account *Account,
) (*codeBuddyTravelState, error) {
	result, err := s.callCodeBuddyGrowth(ctx, account, http.MethodGet, codebuddy.CodeBuddyTravelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	state := &codeBuddyTravelState{}
	if err := codeBuddyGrowthData(result, state); err != nil {
		return nil, err
	}
	return state, nil
}

// departCodeBuddyTravel 派出旅行。
//
// `location_id` 实测 1~4 收益/时长区间**相同**（参考实现 `travel.go:76` 原文），
// 所以恒用 `CodeBuddyTravelLocationID`（=4）而不是随机——可复现优于伪随机，
// 出问题时不必先排除"这次是不是随机到了别的地点"。
func (s *CodeBuddyAdminService) departCodeBuddyTravel(ctx context.Context, account *Account) error {
	_, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyTravelDepartPath,
		map[string]any{"location_id": codebuddy.CodeBuddyTravelLocationID},
	)
	return err
}

// claimCodeBuddyTravel 领到站奖励（**必须带 record_id**）。
func (s *CodeBuddyAdminService) claimCodeBuddyTravel(
	ctx context.Context,
	account *Account,
	recordID int64,
) (int, error) {
	result, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyTravelClaimPath,
		map[string]any{"record_id": recordID},
	)
	if err != nil {
		return 0, err
	}
	var payload struct {
		RewardCredit int `json:"reward_credit"`
	}
	// 奖励字段缺失不视为失败（参考实现同口径：调用方按 0 记日志）。
	_ = codeBuddyGrowthData(result, &payload)
	return payload.RewardCredit, nil
}

// --- 领养（full 级，仅手动）---

// CodeBuddyGrowthAdoptResult 领养结果。
type CodeBuddyGrowthAdoptResult struct {
	// Adopted 本次是否成功领养。
	Adopted bool `json:"adopted"`
	// Credit / Energy 领养到账（参考实现：+300 分 +8 能量）。
	Credit int `json:"credit"`
	Energy int `json:"energy"`
	// ThresholdNotMet 门槛未达（HTTP 400 + first_buddy 关键词）——**预期行为**，
	// 不是故障：说明该账号的 chat_5 还没刷满。
	ThresholdNotMet bool `json:"threshold_not_met"`
	// SkipReason 未动作的原因。
	SkipReason string `json:"skip_reason,omitempty"`
	// Error 顶层失败。
	Error string `json:"error,omitempty"`
}

// RunCodeBuddyGrowthAdoptNow 手动执行领养（buddy/first）。
//
// ⚠️ **分级 `full`（仅手动）**：领养的前置 `chat_5` 只能靠伪造对话活跃满足，
// 且达标后直接送 300 分。**不得**被任何自动排程调用——
// 唯一合法入口是管理端点（人手触发）。
//
// 流程：查猫档案（有猫则跳过）→ 同意协议（幂等）→ 领养。
// 门槛未达时上游回 HTTP 400 + `first_buddy task not completed yet`，
// 属**预期行为**（返回 ThresholdNotMet=true，不算错误、不重试）。
func (s *CodeBuddyAdminService) RunCodeBuddyGrowthAdoptNow(
	ctx context.Context,
	account *Account,
	localDay string,
) CodeBuddyGrowthAdoptResult {
	result := CodeBuddyGrowthAdoptResult{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Error = "not a codebuddy account"
		return result
	}

	// 已有猫 → 无需领养（info 只读）。
	buddy, err := s.fetchCodeBuddyBuddyInfo(ctx, account)
	if err != nil {
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			result.SkipReason = reason
			return result
		}
		result.Error = "fetch buddy info failed"
		slog.Warn("codebuddy_growth.buddy_info_failed", "account_id", account.ID, "error", err)
		return result
	}
	if buddy != nil {
		result.SkipReason = "该账号已有猫"
		return result
	}

	// 同意协议（幂等，重复调用无副作用）。
	if err := s.agreeCodeBuddyBuddyAgreement(ctx, account); err != nil {
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			result.SkipReason = reason
			return result
		}
		result.Error = "agree buddy agreement failed"
		slog.Warn("codebuddy_growth.buddy_agreement_failed", "account_id", account.ID, "error", err)
		return result
	}

	adopt, err := s.adoptCodeBuddyBuddyFirst(ctx, account)
	if err != nil {
		if isCodeBuddyGrowthBuddyThresholdNotMet(err) {
			// 门槛未达：预期行为，当日不再重试（台账挡住）。
			result.ThresholdNotMet = true
			result.SkipReason = "未达 chat_5 门槛（需先用活跃上报刷满 5 轮对话）"
			s.markCodeBuddyGrowthLedger(ctx, account, localDay, ledgerAdoptTried)
			return result
		}
		if reason, quiet := codeBuddyGrowthQuietReason(err); quiet {
			result.SkipReason = reason
			return result
		}
		result.Error = "adopt failed"
		slog.Warn("codebuddy_growth.adopt_failed", "account_id", account.ID, "error", err)
		return result
	}
	result.Adopted = true
	result.Credit = adopt.Credit
	result.Energy = adopt.Energy
	return result
}

// codeBuddyBuddy 猫档案（nil = 无猫）。
type codeBuddyBuddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// fetchCodeBuddyBuddyInfo 查当前猫档案；**返回 (nil, nil) 表示无猫**。
//
// `data.buddy` 为 null / 缺字段 / 空对象都按无猫处理（参考实现
// `BuddyInfo` 的口径）——把"没有猫"当错误会让首次领养永远走不到。
func (s *CodeBuddyAdminService) fetchCodeBuddyBuddyInfo(
	ctx context.Context,
	account *Account,
) (*codeBuddyBuddy, error) {
	result, err := s.callCodeBuddyGrowth(ctx, account, http.MethodGet, codebuddy.CodeBuddyBuddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Buddy *codeBuddyBuddy `json:"buddy"`
	}
	if err := codeBuddyGrowthData(result, &payload); err != nil {
		return nil, err
	}
	if payload.Buddy == nil || payload.Buddy.ID == 0 {
		return nil, nil
	}
	return payload.Buddy, nil
}

// agreeCodeBuddyBuddyAgreement 同意领养协议（幂等）。
func (s *CodeBuddyAdminService) agreeCodeBuddyBuddyAgreement(ctx context.Context, account *Account) error {
	_, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyBuddyAgreementPath,
		map[string]any{"agree": true},
	)
	return err
}

// adoptCodeBuddyBuddyFirst 领养第一只猫。
func (s *CodeBuddyAdminService) adoptCodeBuddyBuddyFirst(
	ctx context.Context,
	account *Account,
) (*codeBuddyGrowthAdoptPayload, error) {
	result, err := s.callCodeBuddyGrowth(
		ctx, account, http.MethodPost, codebuddy.CodeBuddyBuddyFirstPath, map[string]any{},
	)
	if err != nil {
		return nil, err
	}
	payload := &codeBuddyGrowthAdoptPayload{}
	// 字段缺失不视为失败：领养可能已成功，按 0 记日志。
	_ = codeBuddyGrowthData(result, payload)
	return payload, nil
}

type codeBuddyGrowthAdoptPayload struct {
	Credit int `json:"credit"`
	Energy int `json:"energy"`
}

// isCodeBuddyGrowthBuddyThresholdNotMet 判定"领养门槛未达标"。
//
// 判据：HTTP 400 + 文案含 `first_buddy task not completed yet`
// （参考实现 `travel.go:28` 的 marker 常量原文）。
// 该错误**当日不应重试**——门槛要靠刷对话量满足，重试一万次也不会变。
//
// 大小写不敏感匹配：上游文案首字母大小写在不同接口上不一致过。
func isCodeBuddyGrowthBuddyThresholdNotMet(err error) bool {
	var upstreamErr *codeBuddyGrowthUpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if upstreamErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(upstreamErr.Message),
		codebuddy.CodeBuddyBuddyThresholdMarker)
}
