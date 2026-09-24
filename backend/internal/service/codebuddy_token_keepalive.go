package service

import (
	"context"
	"errors"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// T3（吸收自 workbuddy2api D4 / workbuddy-manager M3）：CodeBuddy token 保活。
// - scheduled 模式（每日 22:00）：全量可刷新账号刷新一轮；
// - expiry 模式（M3）：仅刷窗口内（默认 3 天，见 CodeBuddyAccessTokenRefreshWindow）将过期的号；
// - manual_disabled 账号**照常保活**（A9 语义：停用是摘对话流量，不是冻结）；
// - session 失效（refresh 被上游拒绝）连续 3 次 → extra.keepalive_disabled=true，
//   不再对该号尝试保活（对话侧仍由既有 401 failover 兜底）。

const (
	codeBuddyKeepaliveModeScheduled = "scheduled"
	codeBuddyKeepaliveModeExpiry    = "expiry"

	codeBuddyKeepaliveFailStreakKey = "keepalive_fail_streak"
	codeBuddyKeepaliveDisabledKey   = "keepalive_disabled"

	codeBuddyKeepaliveMaxFailStreak = 3
)

// codeBuddyKeepaliveRefresher 令 CodeBuddyTokenRefresher 隐式满足，测试可注入替身。
type codeBuddyKeepaliveRefresher interface {
	CanRefresh(account *Account) bool
	NeedsRefresh(account *Account, refreshWindow time.Duration) bool
	Refresh(ctx context.Context, account *Account) (map[string]any, error)
}

type CodeBuddyTokenKeepalive struct {
	accountRepo AccountRepository
	credsRepo   CodeBuddyOAuthRefreshSuccessRepository // 可空：为空则只刷不持久化
	refresher   codeBuddyKeepaliveRefresher
	window      time.Duration
}

func NewCodeBuddyTokenKeepalive(
	accountRepo AccountRepository,
	credsRepo CodeBuddyOAuthRefreshSuccessRepository,
	refresher codeBuddyKeepaliveRefresher,
) *CodeBuddyTokenKeepalive {
	return &CodeBuddyTokenKeepalive{
		accountRepo: accountRepo,
		credsRepo:   credsRepo,
		refresher:   refresher,
		window:      CodeBuddyAccessTokenRefreshWindow,
	}
}

type CodeBuddyKeepaliveAccountResult struct {
	AccountID int64  `json:"account_id"`
	Action    string `json:"action"` // refreshed|skipped|auth_failed|error
	Detail    string `json:"detail,omitempty"`
}

type CodeBuddyKeepaliveSummary struct {
	Mode       string                            `json:"mode"`
	Total      int                               `json:"total"`
	Refreshed  int                               `json:"refreshed"`
	Skipped    int                               `json:"skipped"`
	AuthFailed int                               `json:"auth_failed"`
	Failed     int                               `json:"failed"`
	Disabled   []int64                           `json:"disabled"`
	Results    []CodeBuddyKeepaliveAccountResult `json:"results"`
}

// RunKeepAlive 跑一轮保活。mode=scheduled 全量；mode=expiry 仅窗口内。
func (s *CodeBuddyTokenKeepalive) RunKeepAlive(ctx context.Context, mode string) (*CodeBuddyKeepaliveSummary, error) {
	if s == nil || s.accountRepo == nil || s.refresher == nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_KEEPALIVE_UNAVAILABLE", "keepalive dependencies not configured")
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformCodeBuddy)
	if err != nil {
		return nil, err
	}
	summary := &CodeBuddyKeepaliveSummary{Mode: mode, Disabled: []int64{}}
	for i := range accounts {
		acct := &accounts[i]
		if !acct.IsCodeBuddy() || acct.Status != StatusActive {
			continue
		}
		summary.Total++
		res := s.refreshOne(ctx, acct, mode)
		summary.Results = append(summary.Results, res)
		switch res.Action {
		case "refreshed":
			summary.Refreshed++
		case "skipped":
			summary.Skipped++
		case "auth_failed":
			summary.AuthFailed++
		case "error":
			summary.Failed++
		}
		if res.Action == "auth_failed" && s.disableIfBroken(ctx, acct) {
			summary.Disabled = append(summary.Disabled, acct.ID)
		}
	}
	return summary, nil
}

func (s *CodeBuddyTokenKeepalive) refreshOne(ctx context.Context, acct *Account, mode string) CodeBuddyKeepaliveAccountResult {
	if acct.ExtraBool(codeBuddyKeepaliveDisabledKey) {
		return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "skipped", Detail: "keepalive_disabled"}
	}
	if mode == codeBuddyKeepaliveModeExpiry && !s.refresher.NeedsRefresh(acct, s.window) {
		return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "skipped", Detail: "not_expiring"}
	}
	if !s.refresher.CanRefresh(acct) {
		return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "skipped", Detail: "cannot_refresh"}
	}
	creds, err := s.refresher.Refresh(ctx, acct)
	if err != nil {
		if errors.Is(err, errCodeBuddyRefreshRejected) {
			return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "auth_failed", Detail: err.Error()}
		}
		return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "error", Detail: err.Error()}
	}
	if s.credsRepo != nil {
		applied, perr := s.credsRepo.UpdateCodeBuddyCredentialsIfUnchanged(
			ctx, acct.ID, acct.Credentials, acct.ProxyID, creds)
		if perr != nil {
			return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "error", Detail: perr.Error()}
		}
		if !applied {
			return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "skipped", Detail: "credentials_changed_during_refresh"}
		}
	}
	_ = s.accountRepo.UpdateExtra(ctx, acct.ID, map[string]any{codeBuddyKeepaliveFailStreakKey: 0})
	return CodeBuddyKeepaliveAccountResult{AccountID: acct.ID, Action: "refreshed"}
}

// disableIfBroken 失效连击达到阈值后停掉该号的保活（extra 位，幂等）。
// 返回 true 表示本次触发了停用。
func (s *CodeBuddyTokenKeepalive) disableIfBroken(ctx context.Context, acct *Account) bool {
	streak := 0
	if v, ok := acct.Extra[codeBuddyKeepaliveFailStreakKey].(float64); ok {
		streak = int(v)
	} else if v, ok := acct.Extra[codeBuddyKeepaliveFailStreakKey].(int); ok {
		streak = v
	}
	streak++
	updates := map[string]any{codeBuddyKeepaliveFailStreakKey: streak}
	if streak >= codeBuddyKeepaliveMaxFailStreak {
		updates[codeBuddyKeepaliveDisabledKey] = true
	}
	_ = s.accountRepo.UpdateExtra(ctx, acct.ID, updates)
	return streak >= codeBuddyKeepaliveMaxFailStreak
}
