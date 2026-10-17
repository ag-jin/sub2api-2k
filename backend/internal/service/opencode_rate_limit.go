package service

import (
	"context"
	"log/slog"
	"time"

	opencodepkg "github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
)

type openCodeRateLimitExtendingRepository interface {
	SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error
}

// openCodeRateLimitResetAt returns the latest valid reset for exhausted
// OpenCode windows. Accounts remain paused until every exhausted window recovers.
func openCodeRateLimitResetAt(account *Account, snapshot *opencodepkg.UsageSnapshot, now time.Time) (time.Time, bool) {
	if account == nil || snapshot == nil {
		return time.Time{}, false
	}

	var resetAt time.Time
	for _, window := range []opencodepkg.UsageWindow{snapshot.Rolling, snapshot.Weekly, snapshot.Monthly} {
		if window.Status != opencodepkg.UsageStatusRateLimited && window.Percent < 100 {
			continue
		}
		if window.ResetsAt == nil || !window.ResetsAt.After(now) {
			continue
		}
		if window.ResetsAt.After(resetAt) {
			resetAt = *window.ResetsAt
		}
	}
	if resetAt.IsZero() {
		return time.Time{}, false
	}
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	return resetAt, true
}

func persistOpenCodeRateLimit(ctx context.Context, repo AccountRepository, blocker AccountRuntimeBlocker, account *Account, snapshot *opencodepkg.UsageSnapshot) bool {
	if repo == nil || account == nil || account.ID <= 0 {
		return false
	}
	resetAt, limited := openCodeRateLimitResetAt(account, snapshot, time.Now())
	if !limited {
		return false
	}
	// 启用额度保护：付费窗口耗尽收敛为模型作用域的付费钱包冷却，不写整账号
	// rate_limit_reset_at、也不写进程内全号 Block——否则替代目标与免费模型会被
	// 连带排除出候选（用户复现的"满 100% 全号拦截"）。未启用账号保持原行为。
	if account.GetOpenCodeQuotaFallback().Enabled {
		// 429 主动探测结果先并入同一份 opencode_go_usage_snapshot（组作用域写入，
		// siblings 共享），判定输入与 OpenCodeGoUsageService 刷新完全一致，不以
		// 另一套钱包状态替代已冻结的快照契约；再写模型作用域的付费钱包冷却。
		persistOpenCodeQuotaFallbackProbeSnapshot(ctx, repo, account, snapshot)
		return persistOpenCodeQuotaFallbackWalletLimit(ctx, repo, account, resetAt, "opencode_usage")
	}
	if blocker != nil {
		blocker.BlockAccountScheduling(account, resetAt, "opencode_usage")
	}

	var err error
	if extendingRepo, ok := repo.(openCodeRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(ctx, account.ID, resetAt)
	} else {
		err = repo.SetRateLimited(ctx, account.ID, resetAt)
	}
	if err != nil {
		slog.Warn("persist_opencode_rate_limit_failed", "account_id", account.ID, "reset_at", resetAt.UTC(), "error", err)
	}
	return true
}

// persistOpenCodeQuotaFallbackProbeSnapshot 把 429 主动探测（pkg/opencode 归一快照）
// 并入同一份 opencode_go_usage_snapshot：复用既有成功快照形态与组作用域写入方
// （siblings 共享同一份证据），使 429 判定与刷新服务共用同一输入。失败只记日志，
// 不影响模型作用域冷却写入。
func persistOpenCodeQuotaFallbackProbeSnapshot(ctx context.Context, repo AccountRepository, account *Account, probe *opencodepkg.UsageSnapshot) bool {
	if repo == nil || account == nil || account.ID <= 0 || probe == nil {
		return false
	}
	writer, ok := repo.(openCodeGoUsageRepository)
	if !ok {
		return false
	}
	data := openCodeGoUsageDataFromProbe(probe)
	if data == nil {
		return false
	}
	now := time.Now().UTC()
	snapshot := &OpenCodeGoUsageSnapshot{
		Status:        OpenCodeGoUsageStatusOK,
		Data:          data,
		FetchedAt:     &now,
		LastAttemptAt: now,
		NextRefreshAt: now.Add(time.Duration(opencodeGoUsageDefaultIntervalMinutes) * time.Minute),
	}
	if err := writer.UpdateOpenCodeGoUsageSnapshot(ctx, account, snapshot); err != nil {
		slog.Warn("opencode_quota_fallback_probe_snapshot_failed", "account_id", account.ID, "error", err)
		return false
	}
	return true
}

// openCodeGoUsageDataFromProbe 把 429 探测快照转换为持久形态用量数据；rate-limited
// 窗口按既有约定归一为已用满（与 openCodeWindowFromSnapshot / openCodeGoUsageWindowFromAPI 一致）。
func openCodeGoUsageDataFromProbe(probe *opencodepkg.UsageSnapshot) *OpenCodeGoUsageData {
	if probe == nil {
		return nil
	}
	return &OpenCodeGoUsageData{
		Rolling: openCodeGoUsageWindowFromProbe(probe.Rolling),
		Weekly:  openCodeGoUsageWindowFromProbe(probe.Weekly),
		Monthly: openCodeGoUsageWindowFromProbe(probe.Monthly),
	}
}

func openCodeGoUsageWindowFromProbe(window opencodepkg.UsageWindow) OpenCodeGoUsageWindow {
	out := OpenCodeGoUsageWindow{Status: string(window.Status), Percent: window.Percent}
	if out.Status == "" {
		out.Status = string(opencodepkg.UsageStatusOK)
	}
	if window.Status == opencodepkg.UsageStatusRateLimited && out.Percent < 100 {
		out.Percent = 100
	}
	if window.ResetsAt != nil {
		out.ResetsAt = window.ResetsAt.UTC()
	}
	return out
}
