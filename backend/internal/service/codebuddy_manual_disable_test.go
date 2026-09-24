package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A9/M7 临时停用双状态位：manual_disabled 只摘对话流量选号，
// 绝不影响 IsSchedulable（签到/保活/6004 排程照常）。

func TestIsManuallyDisabled(t *testing.T) {
	assert.False(t, (*Account)(nil).IsManuallyDisabled())
	assert.False(t, (&Account{}).IsManuallyDisabled(), "无 extra 视为未停用")
	assert.False(t, (&Account{Extra: map[string]any{}}).IsManuallyDisabled())
	assert.True(t, (&Account{Extra: map[string]any{"manual_disabled": true}}).IsManuallyDisabled(),
		"布尔简写形式")
	assert.True(t, (&Account{Extra: map[string]any{"manual_disabled": map[string]any{
		"enabled": true, "reason": "拖后腿", "at": "2026-09-24T00:00:00Z",
	}}}).IsManuallyDisabled(), "对象形式 {enabled,reason,at}")
	assert.False(t, (&Account{Extra: map[string]any{"manual_disabled": map[string]any{
		"enabled": false,
	}}}).IsManuallyDisabled(), "对象 enabled=false 视为恢复")
}

func TestManualDisable_DoesNotTouchIsSchedulable(t *testing.T) {
	// 关键语义：手动停用绝不改变 IsSchedulable——否则签到/保活/6004 排程会被误停
	acct := &Account{
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{"manual_disabled": map[string]any{"enabled": true}},
	}
	assert.True(t, acct.IsManuallyDisabled())
	assert.True(t, acct.IsSchedulable(), "手动停用不得改变 IsSchedulable")
}

func TestIsAccountSchedulableForSelection_ExcludesManualDisabled(t *testing.T) {
	svc := &GatewayService{}
	acct := &Account{
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{"manual_disabled": map[string]any{"enabled": true}},
	}
	assert.False(t, svc.isAccountSchedulableForSelection(acct), "手动停用号不得被选中")
	acct.Extra = map[string]any{"manual_disabled": map[string]any{"enabled": false}}
	assert.True(t, svc.isAccountSchedulableForSelection(acct), "恢复后应可选")
}

func TestOpenAIEligibility_ManualDisabledReason(t *testing.T) {
	acct := &Account{
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{"manual_disabled": map[string]any{"enabled": true}},
	}
	reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		context.Background(), acct, PlatformCodeBuddy, "deepseek-v4.1-flash", false, "")
	assert.Equal(t, "manual_disabled", reason)
}

func TestIsCodeBuddyModelUsageLimited_6004UnaffectedByManualDisable(t *testing.T) {
	// 6004 模型级停调与手动停用相互独立：手动停用不要求 6004 状态，反之亦然
	repo := &codeBuddyUsageLimitRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{
		Platform: PlatformCodeBuddy,
		Extra:    map[string]any{"manual_disabled": map[string]any{"enabled": true}},
	}
	_, ok := rls.TriggerCodeBuddyModelUsageLimit(context.Background(), acct, "m", 429, []byte(`{"code":6004,"msg":"x 将在 2026-09-24 15:45:02 UTC+8 重置"}`))
	require.True(t, ok, "手动停用号的 6004 处置照常执行（停用期排程任务照常的镜像语义）")
}
