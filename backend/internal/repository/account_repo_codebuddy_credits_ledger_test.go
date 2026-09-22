package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Scenario: codebuddy 积分留痕是调度中性键——出站余额轮询每 3 分钟写一次基线/
// 流水，若不当成中性键，每次轮询都会入队一条调度变更事件。
func TestCodeBuddyCreditsLedgerExtraIsSchedulerNeutral(t *testing.T) {
	require.True(t, isSchedulerNeutralExtraKey("codebuddy_credits_ledger"))
	require.True(t, isSchedulerNeutralExtraKey("codebuddy_credits_ledger_balance"))
	require.True(t, isSchedulerNeutralExtraKey("codebuddy_credits_ledger_baseline_at"))
	require.True(t, isSchedulerNeutralExtraKey("codebuddy_credits_ledger_dedup"))
	require.False(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{
		"codebuddy_credits_ledger":             map[string]any{"delta": 80.0},
		"codebuddy_credits_ledger_balance":     180.0,
		"codebuddy_credits_ledger_baseline_at": "2026-09-22T08:00:00Z",
		"codebuddy_credits_ledger_dedup":       "codebuddy-credits|180",
	}))
}
