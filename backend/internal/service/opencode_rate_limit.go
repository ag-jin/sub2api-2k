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
