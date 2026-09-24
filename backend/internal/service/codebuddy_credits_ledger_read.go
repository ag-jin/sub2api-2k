package service

import (
	"context"
)

// GetCodeBuddyCreditsLedger 暴露积分流水与基线（M9 数据源，只读旁路）。
// 返回原始 extra 片段：ledger（最近一条变动）、baseline、last_checkin_at 等。
func (s *CodeBuddyAdminService) GetCodeBuddyCreditsLedger(ctx context.Context, accountID int64) (map[string]any, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errCodeBuddyManualDisableNeedsAccount
	}
	acct, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || acct == nil || !acct.IsCodeBuddy() {
		return nil, errCodeBuddyManualDisableNeedsAccount
	}
	out := map[string]any{}
	pick := func(key string) {
		if v, ok := acct.Extra[key]; ok {
			out[key] = v
		}
	}
	pick(codeBuddyCreditsLedgerExtraKey)
	pick(codeBuddyCreditsLedgerBalanceKey)
	pick(codeBuddyCreditsLedgerBaselineAtKey)
	pick("last_checkin_at")
	pick("streak_days")
	return out, nil
}
