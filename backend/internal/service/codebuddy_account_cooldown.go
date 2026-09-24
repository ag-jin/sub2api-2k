package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"log/slog"
)

// A7 分级冷却（吸收自 workbuddy2api）：
//   - HTTP 402 或业务码 14018（积分耗尽）→ 硬冷却至**次日 04:00 UTC+8**
//     （签到恢复积分的时点，与上游「硬冷却至次日 04:00」同语义）；
//   - HTTP 404 → 浅冷却 10 分钟（瞬时路由/模型下线类抖动）；
//   - 429/6004 不走本通道（模型级停调见 TriggerCodeBuddyModelUsageLimit）。
//
// 账号级冷却走既有 SetTempUnschedulable 通道（TempUnschedulableUntil 挡选号），
// 不与 manual_disabled（手动摘出，A9）混淆。

const (
	codeBuddyCooldown404Duration   = 10 * time.Minute
	codeBuddyCooldownHardHour      = 4 // 次日 04:00（UTC+8）
	codeBuddyBizCodeQuotaExhausted = 14018
)

// TriggerCodeBuddyAccountCooldown 对 codebuddy 账号应用分级冷却。
// now 由调用方注入（测试）；返回 ok=false 表示该状态不属于分级冷却范畴。
func (s *RateLimitService) TriggerCodeBuddyAccountCooldownAt(
	acct *Account, statusCode int, body []byte, now time.Time,
) (time.Time, bool) {
	return s.triggerCodeBuddyAccountCooldown(context.Background(), acct, statusCode, body, now)
}

// TriggerCodeBuddyAccountCooldown 生产入口（now=当前时刻）。
func (s *RateLimitService) TriggerCodeBuddyAccountCooldown(
	ctx context.Context, acct *Account, statusCode int, body []byte,
) (time.Time, bool) {
	return s.triggerCodeBuddyAccountCooldown(ctx, acct, statusCode, body, time.Now())
}

func (s *RateLimitService) triggerCodeBuddyAccountCooldown(
	ctx context.Context, acct *Account, statusCode int, body []byte, now time.Time,
) (time.Time, bool) {
	if s == nil || s.accountRepo == nil || acct == nil || !acct.IsCodeBuddy() {
		return time.Time{}, false
	}
	quotaExhausted := statusCode == http.StatusPaymentRequired ||
		(len(body) > 0 && gjson.GetBytes(body, "code").Int() == codeBuddyBizCodeQuotaExhausted)

	var until time.Time
	switch {
	case quotaExhausted:
		until = nextCodeBuddyFourAM(now)
	case statusCode == http.StatusNotFound:
		until = now.Add(codeBuddyCooldown404Duration)
	default:
		return time.Time{}, false
	}
	reason := "codebuddy_account_cooldown:" + hardCooldownReasonTag(statusCode, body)
	if err := s.accountRepo.SetTempUnschedulable(ctx, acct.ID, until, reason); err != nil {
		slog.Warn("codebuddy_account_cooldown_set_failed", "account_id", acct.ID, "error", err)
		return until, true
	}
	slog.Info("codebuddy_account_cooldown", "account_id", acct.ID, "status", statusCode, "until", until.Format(time.RFC3339))
	return until, true
}

func hardCooldownReasonTag(statusCode int, body []byte) string {
	if statusCode == http.StatusPaymentRequired ||
		gjson.GetBytes(body, "code").Int() == codeBuddyBizCodeQuotaExhausted {
		return "quota_exhausted"
	}
	return strings.ToLower(http.StatusText(statusCode))
}

// nextCodeBuddyFourAM 下一个 04:00 UTC+8（含当天：02:59 触发冷却到当天 04:00）。
func nextCodeBuddyFourAM(now time.Time) time.Time {
	local := now.In(codeBuddyTimeZone)
	next := time.Date(local.Year(), local.Month(), local.Day(),
		codeBuddyCooldownHardHour, 0, 0, 0, codeBuddyTimeZone)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
