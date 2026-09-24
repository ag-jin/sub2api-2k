package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

// A9/M7 临时停用（吸收自 workbuddy2api / workbuddy-manager）：
// manual_disabled 只摘出对话流量选号；签到/保活/6004 排程照常执行。
// 存储走 extra 的 JSONB 原子合并（UpdateExtra），不与其它 extra 键互踩。

var errCodeBuddyManualDisableNeedsAccount = errors.New("codebuddy manual disable: account not found or not codebuddy")

// ManualDisable 手动停用：后续对话选号不再选中该号；签到/保活照常。
func (s *CodeBuddyAdminService) ManualDisable(ctx context.Context, accountID int64, reason string) error {
	if s == nil || s.accountRepo == nil {
		return errCodeBuddyManualDisableNeedsAccount
	}
	acct, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || acct == nil || !acct.IsCodeBuddy() {
		return errCodeBuddyManualDisableNeedsAccount
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "运营手动停用"
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		"manual_disabled": map[string]any{
			"enabled": true,
			"reason":  reason,
			"at":      time.Now().UTC().Format(time.RFC3339),
		},
	})
}

// ManualEnable 恢复：清除 manual_disabled 位；账号重新回到选号池
// （若系统级 disabled 仍在，则仍不可选——两位独立，都清才回池）。
func (s *CodeBuddyAdminService) ManualEnable(ctx context.Context, accountID int64) error {
	if s == nil || s.accountRepo == nil {
		return errCodeBuddyManualDisableNeedsAccount
	}
	acct, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || acct == nil || !acct.IsCodeBuddy() {
		return errCodeBuddyManualDisableNeedsAccount
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		"manual_disabled": map[string]any{
			"enabled":    false,
			"cleared_at": time.Now().UTC().Format(time.RFC3339),
		},
	})
}
