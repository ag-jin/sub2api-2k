package service

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/tidwall/gjson"
	"log/slog"
)

// CodeBuddy 6004「模型用量超限」识别与 (账号,模型) 级停调。
//
// 平台按「账号 × 模型」控流：超额后返回 429 + 业务码 6004，消息里给出精确
// 重置时刻（"将在 2026-09-24 15:45:02 UTC+8 重置"），并明示"切换其他模型可
// 立即使用"。因此处置必须是：
//   - 只停调 (该账号, 该模型) 这一对，绝不停调整个账号；
//   - 停调终点 = 消息里的重置时刻（解析失败退化为固定兜底时长，而非丢弃）；
//   - 当前请求照常 failover，持久化失败也不得把请求放回同一账号。
//
// 复用既有 SetModelRateLimit 通道（与 temp_unschedulable_rules 的模型级路径
// 同一把 (账号,模型) 键），调度器侧无需新增读路径。不走 IsTempUnschedulableEnabled
// 开关：平台控流是该平台固有行为，不依赖运营者逐账号配置。

const (
	codeBuddyBizCodeModelUsageLimit = 6004
	// 解析失败或重置时刻已过时的兜底：只覆盖很短窗口，宁可早恢复不可误停。
	codeBuddyUsageLimitFallbackDuration = 30 * time.Minute
	codeBuddyUsageLimitMinDuration      = time.Minute
	codeBuddyUsageLimitMaxDuration      = 24 * time.Hour
)

var codeBuddyUsageLimitResetPattern = regexp.MustCompile(`将在\s*(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})\s*UTC\+8\s*重置`)

// ParseCodeBuddyModelUsageLimit 识别 6004 用量超限响应并解析重置时刻。
// 返回 ok=false 表示不是该类响应（调用方继续走通用错误链）。
func ParseCodeBuddyModelUsageLimit(statusCode int, body []byte) (time.Time, bool) {
	if statusCode != http.StatusTooManyRequests || len(body) == 0 {
		return time.Time{}, false
	}
	if gjson.GetBytes(body, "code").Int() != codeBuddyBizCodeModelUsageLimit {
		return time.Time{}, false
	}
	if m := codeBuddyUsageLimitResetPattern.FindStringSubmatch(gjson.GetBytes(body, "msg").String()); m != nil {
		loc := time.FixedZone("UTC+8", 8*60*60)
		if resetAt, err := time.ParseInLocation("2006-01-02 15:04:05", m[1]+" "+m[2], loc); err == nil {
			return resetAt, true
		}
	}
	// 6004 但报文形态变化：按平台控流对待，用兜底时长，避免整段缺失处置。
	return time.Now().Add(codeBuddyUsageLimitFallbackDuration), true
}

// clampCodeBuddyUsageReset 把重置时刻约束在 [now+1min, now+24h]。
func clampCodeBuddyUsageReset(resetAt, now time.Time) time.Time {
	if !resetAt.After(now) {
		return now.Add(codeBuddyUsageLimitMinDuration)
	}
	if maxAt := now.Add(codeBuddyUsageLimitMaxDuration); resetAt.After(maxAt) {
		return maxAt
	}
	return resetAt
}

// TriggerCodeBuddyModelUsageLimit 把 (账号, 该模型) 停调到 6004 重置时刻。
// 返回 ok=false 表示未识别（调用方继续通用错误链）；ok=true 表示调用方应
// 立即 failover 当前请求。持久化失败仍返回 true：绝不把请求放回同一账号。
func (s *RateLimitService) TriggerCodeBuddyModelUsageLimit(ctx context.Context, account *Account, model string, statusCode int, responseBody []byte) (time.Time, bool) {
	if s == nil || s.accountRepo == nil || account == nil || !account.IsCodeBuddy() || model == "" {
		return time.Time{}, false
	}
	parsedAt, ok := ParseCodeBuddyModelUsageLimit(statusCode, responseBody)
	if !ok {
		return time.Time{}, false
	}
	resetAt := clampCodeBuddyUsageReset(parsedAt, time.Now())
	state := TempUnschedState{
		UntilUnix:       resetAt.Unix(),
		TriggeredAtUnix: time.Now().Unix(),
		StatusCode:      statusCode,
		MatchedKeyword:  "codebuddy_6004_model_usage_limit",
		ErrorMessage:    truncateTempUnschedMessage(responseBody, tempUnschedMessageMaxBytes),
	}
	reason := ""
	if raw, err := json.Marshal(state); err == nil {
		reason = string(raw)
	}
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, model, resetAt, reason); err != nil {
		slog.Warn("codebuddy_usage_limit_model_unsched_set_failed", "account_id", account.ID, "model", model, "error", err)
		return resetAt, true
	}
	slog.Info("codebuddy_model_usage_limit", "account_id", account.ID, "model", model, "until", resetAt.Format(time.RFC3339), "biz_code", codeBuddyBizCodeModelUsageLimit)
	return resetAt, true
}
