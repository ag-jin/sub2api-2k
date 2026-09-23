package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 积分耗尽解冻闭环测试（A4 批 4.5）+ 保活窗口按平台生效（4.4）。
//
// 4.5 的核心风险：解冻必须**只清本模块写入的**硬冷却。
// 余额恢复顺手清掉别家（凭据失效要重录、风控要人工处理）的停调，会让那些
// 保护静默失效——所以正例与反例必须成对断言。

// unfreezeTestRepo 支持停调/模型级限流的账号仓储 stub。
type unfreezeTestRepo struct {
	AccountRepository
	accounts map[int64]*Account
	// clearCalls 记录 ClearTempUnschedulable 的调用次数与账号。
	clearCalls []int64
	clearErr   error
}

func newUnfreezeTestRepo(accounts ...*Account) *unfreezeTestRepo {
	repo := &unfreezeTestRepo{accounts: map[int64]*Account{}}
	for _, account := range accounts {
		repo.accounts[account.ID] = account
	}
	return repo
}

func (r *unfreezeTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if account, ok := r.accounts[id]; ok {
		return account, nil
	}
	return nil, ErrAccountNotFound
}

func (r *unfreezeTestRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	out := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, *account)
		}
	}
	return out, nil
}

func (r *unfreezeTestRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	account, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = reason
	return nil
}

func (r *unfreezeTestRepo) ClearTempUnschedulable(_ context.Context, id int64) error {
	if r.clearErr != nil {
		return r.clearErr
	}
	r.clearCalls = append(r.clearCalls, id)
	account, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	account.TempUnschedulableUntil = nil
	account.TempUnschedulableReason = ""
	return nil
}

func (r *unfreezeTestRepo) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, _ ...string) error {
	account, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	setAccountModelRateLimitSnapshot(account, scope, resetAt, "test", time.Now())
	return nil
}

// newCodeBuddyAccountWithCooldown 造一个"处于本模块积分耗尽硬冷却中"的 codebuddy 账号。
func newCodeBuddyAccountWithCooldown(id int64) *Account {
	until := time.Now().Add(6 * time.Hour)
	return &Account{
		ID:                      id,
		Platform:                PlatformCodeBuddy,
		Type:                    AccountTypeAPIKey,
		Schedulable:             true,
		Status:                  StatusActive,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: codeBuddyCreditsExhaustedReasonPrefix,
		Credentials:             map[string]any{"access_token": "at-x"},
	}
}

// --- 必写测试 4：解冻闭环 ---

// Scenario（正例）：余额恢复到阈值以上 → 硬冷却被清除。
// 阈值口径与参考实现 ReenableIfCredits 一致：remain > 0 即可。
func TestReenableCodeBuddyIfCreditsRecoveredClearsCooldown(t *testing.T) {
	account := newCodeBuddyAccountWithCooldown(11)
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	recovered := svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 1)

	require.True(t, recovered, "余额恢复到阈值以上应解除冷却")
	require.Equal(t, []int64{11}, repo.clearCalls, "应调用一次 ClearTempUnschedulable")
	require.Nil(t, repo.accounts[11].TempUnschedulableUntil, "冷却时间被清除")
	require.Empty(t, repo.accounts[11].TempUnschedulableReason, "停调理由被清除")
}

// Scenario（反例 1）：余额为 0 → **不**清除（还没恢复，清了会立刻再撞 402）。
func TestReenableCodeBuddyIfCreditsZeroKeepsCooldown(t *testing.T) {
	account := newCodeBuddyAccountWithCooldown(12)
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 0),
		"余额为 0 时不应解除冷却")
	require.Empty(t, repo.clearCalls, "不得调用 ClearTempUnschedulable")
	require.NotNil(t, repo.accounts[12].TempUnschedulableUntil, "冷却仍在")
}

// Scenario（反例 2）：余额为负（脏数据）→ 不清除。负数不等于"有余额"。
func TestReenableCodeBuddyIfCreditsNegativeKeepsCooldown(t *testing.T) {
	account := newCodeBuddyAccountWithCooldown(13)
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, -5))
	require.Empty(t, repo.clearCalls)
	require.NotNil(t, repo.accounts[13].TempUnschedulableUntil)
}

// Scenario（关键反例）：**别家写入的停调不得被余额恢复清掉**。
// 凭据失效要重录、风控要人工处理——顺手清掉会让这些保护静默失效。
func TestReenableCodeBuddyIfCreditsDoesNotClearForeignCooldown(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{
		ID:                      14,
		Platform:                PlatformCodeBuddy,
		Type:                    AccountTypeAPIKey,
		Schedulable:             true,
		Status:                  StatusActive,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: "session_dead: 12153 需重新登录", // 非本模块前缀
		Credentials:             map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 999),
		"非本模块写入的停调不归余额恢复管")
	require.Empty(t, repo.clearCalls, "不得触碰别家的停调")
	require.NotNil(t, repo.accounts[14].TempUnschedulableUntil, "凭据故障冷却保留")
}

// Scenario（反例 3）：账号压根没有本模块冷却 → 不做任何写操作（幂等空转）。
func TestReenableCodeBuddyIfCreditsNoCooldownIsNoop(t *testing.T) {
	account := &Account{
		ID:          15,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 100))
	require.Empty(t, repo.clearCalls, "无冷却时不该产生写操作")
}

// Scenario（反例 4）：冷却已过期（时间已过）→ 不算"处于冷却中"，不重复清。
func TestReenableCodeBuddyIfCreditsExpiredCooldownIsNoop(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	account := &Account{
		ID:                      16,
		Platform:                PlatformCodeBuddy,
		Type:                    AccountTypeAPIKey,
		Schedulable:             true,
		Status:                  StatusActive,
		TempUnschedulableUntil:  &past,
		TempUnschedulableReason: codeBuddyCreditsExhaustedReasonPrefix,
		Credentials:             map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 100),
		"已过期的冷却无需清除")
	require.Empty(t, repo.clearCalls)
}

// Scenario（反例 5）：非 codebuddy 平台的账号不受影响（闭环只服务本平台）。
func TestReenableCodeBuddyIfCreditsIgnoresOtherPlatforms(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{
		ID:                      17,
		Platform:                PlatformAnthropic,
		Type:                    AccountTypeAPIKey,
		Schedulable:             true,
		Status:                  StatusActive,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: codeBuddyCreditsExhaustedReasonPrefix,
		Credentials:             map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 100))
	require.Empty(t, repo.clearCalls)
}

// Scenario：ClearTempUnschedulable 报错 → 如实返回 false（不谎报已解冻）。
func TestReenableCodeBuddyIfCreditsSurfacesClearError(t *testing.T) {
	account := newCodeBuddyAccountWithCooldown(18)
	repo := newUnfreezeTestRepo(account)
	repo.clearErr = errors.New("db down")
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), account, 100),
		"落库失败必须如实返回 false")
	require.NotNil(t, repo.accounts[18].TempUnschedulableUntil, "冷却未被误标为已清")
}

// Scenario：nil 入参不 panic（防御路径）。
func TestReenableCodeBuddyIfCreditsNilSafety(t *testing.T) {
	var nilSvc *CodeBuddyAdminService
	require.False(t, nilSvc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), nil, 100))

	repo := newUnfreezeTestRepo()
	svc := NewCodeBuddyAdminService(nil, repo, nil)
	require.False(t, svc.ReenableCodeBuddyIfCreditsRecovered(context.Background(), nil, 100))
}

// --- 4.6 业务码冷却规则（6004 只冷模型，不冷账号）---

// Scenario：6004 是**模型级**限流——落在模型维度，账号继续服务其他模型。
// 整号冷却会把上游明确限定为"仅该模型"的故障放大成全账号不可用。
func TestCodeBuddyBizCodeCooldown6004IsModelScoped(t *testing.T) {
	decision := CodeBuddyBizCodeCooldown(CodeBuddyModelRateLimitBizCode)

	require.True(t, decision.Handled)
	require.Equal(t, "model", decision.Scope, "6004 必须只冷模型")
	require.Greater(t, decision.Duration, time.Duration(0))
}

// Scenario：14018 积分耗尽 → 账号级冷却，且时长要能跨过签到窗口。
func TestCodeBuddyBizCodeCooldown14018IsAccountScoped(t *testing.T) {
	decision := CodeBuddyBizCodeCooldown(CodeBuddyCreditsExhaustedBizCode)

	require.True(t, decision.Handled)
	require.Equal(t, "account", decision.Scope)
	require.GreaterOrEqual(t, decision.Duration, time.Hour,
		"积分耗尽冷却应足够长，避免耗尽期间被反复调度空转")
}

// Scenario：未被本表覆盖的码交回既有规则引擎（不抢管）。
func TestCodeBuddyBizCodeCooldownUnknownDelegatesToRuleEngine(t *testing.T) {
	decision := CodeBuddyBizCodeCooldown(999999)
	require.False(t, decision.Handled, "未覆盖的码应交给 TempUnschedulableRule 引擎")
}

// Scenario：6004 施加冷却时**只写模型级**，不得写账号级停调。
// 这条是"只冷模型不冷账号"的行为证据（不是只看决策结构）。
func TestApplyCodeBuddyBizCodeCooldown6004TouchesModelNotAccount(t *testing.T) {
	account := &Account{
		ID:          21,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	applied := svc.ApplyCodeBuddyBizCodeCooldown(context.Background(), account, CodeBuddyModelRateLimitBizCode, "hy3-preview")

	require.True(t, applied)
	require.Nil(t, repo.accounts[21].TempUnschedulableUntil,
		"6004 不得把整个账号摘出去（其他模型仍可用）")
	require.True(t, account.isRateLimitActiveForKey("hy3-preview"),
		"该模型进入限流")
	require.False(t, account.isRateLimitActiveForKey("other-model"),
		"同账号其他模型不受影响")
}

// Scenario：6004 但**拿不到模型名**时，放弃模型级冷却而不是降级为账号级。
// 降级会把上游限定的模型故障扩大成整号故障，代价更大。
func TestApplyCodeBuddyBizCodeCooldown6004WithoutModelDoesNothing(t *testing.T) {
	account := &Account{
		ID:          22,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.False(t, svc.ApplyCodeBuddyBizCodeCooldown(context.Background(), account, CodeBuddyModelRateLimitBizCode, ""),
		"无模型名时放弃冷却")
	require.Nil(t, repo.accounts[22].TempUnschedulableUntil, "不得降级为账号级冷却")
}

// Scenario：14018 施加**账号级**冷却（整号摘出，等签到/充值恢复）。
func TestApplyCodeBuddyBizCodeCooldown14018BlocksAccount(t *testing.T) {
	account := &Account{
		ID:          23,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "at-x"},
	}
	repo := newUnfreezeTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	require.True(t, svc.ApplyCodeBuddyBizCodeCooldown(context.Background(), account, CodeBuddyCreditsExhaustedBizCode, "hy3"))
	require.NotNil(t, repo.accounts[23].TempUnschedulableUntil, "14018 应整号停调")
	require.True(t, account.HasCodeBuddyCreditsExhaustedCooldown(),
		"写入的停调必须能被解冻闭环识别（reason 前缀一致）")
}

// --- 必写测试 3：保活窗口按平台生效（回归保护）---

// Scenario：CodeBuddy 用 3 天窗口；其他平台沿用传入的全局窗口。
// 这条是回归保护：全局窗口是按 Google 的 1 小时 token 选的（0.5h），
// 套到签发 60 天的 CodeBuddy accessToken 上会让刷新窗口过窄。
func TestCodeBuddyRefreshWindowIsPlatformScoped(t *testing.T) {
	t.Parallel()

	refresher := NewCodeBuddyTokenRefresher()
	globalWindow := 30 * time.Minute

	t.Run("codebuddy 用 3 天窗口", func(t *testing.T) {
		// 距到期还有 2 天：全局 0.5h 窗口下"不需要刷新"，codebuddy 专属
		// 3 天窗口下必须需要刷新。
		account := &Account{
			ID:       31,
			Platform: PlatformCodeBuddy,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_token":  "at-x",
				"refresh_token": "rt-x",
				"expires_at":    time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
			},
		}
		require.True(t, refresher.NeedsRefresh(account, globalWindow),
			"剩 2 天 < 3 天窗口 → 应判需要刷新")
	})

	t.Run("距到期超过 3 天则不需刷新", func(t *testing.T) {
		account := &Account{
			ID:       32,
			Platform: PlatformCodeBuddy,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_token":  "at-x",
				"refresh_token": "rt-x",
				"expires_at":    time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339),
			},
		}
		require.False(t, refresher.NeedsRefresh(account, globalWindow),
			"剩 10 天 > 3 天窗口 → 不需刷新")
	})

	t.Run("全局窗口更宽时以全局为准（运维手调不被缩回）", func(t *testing.T) {
		account := &Account{
			ID:       33,
			Platform: PlatformCodeBuddy,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"access_token":  "at-x",
				"refresh_token": "rt-x",
				"expires_at":    time.Now().Add(8 * 24 * time.Hour).UTC().Format(time.RFC3339),
			},
		}
		// 全局 10 天 > codebuddy 专属 3 天 → 剩 8 天应判需要刷新。
		require.True(t, refresher.NeedsRefresh(account, 10*24*time.Hour),
			"全局窗口更宽时应生效，不能把运维的窗口缩回 3 天")
		// 同一账号在默认 0.5h 全局窗口下不需刷新（证明上面的 true 来自窗口差异）。
		require.False(t, refresher.NeedsRefresh(account, globalWindow))
	})
}

// Scenario：其他平台的刷新器**不受** CodeBuddy 的 3 天窗口影响（回归保护）。
// 断言的是"全局窗口原样生效"，不是某个具体平台实现。
func TestRefreshWindowPlatformScopingDoesNotLeakToOtherPlatforms(t *testing.T) {
	t.Parallel()

	globalWindow := 30 * time.Minute
	refresher := NewCodeBuddyTokenRefresher()

	// 非 codebuddy 账号：CanRefresh 为 false，刷新器本就不接管。
	anthropicAccount := &Account{
		ID:       41,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "at-x",
			"refresh_token": "rt-x",
			"expires_at":    time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	require.False(t, refresher.CanRefresh(anthropicAccount),
		"codebuddy 刷新器不得接管其他平台账号")

	// Claude 刷新器沿用传入的全局窗口（0.5h）——剩 2 天不该判需要刷新。
	claudeRefresher := &ClaudeTokenRefresher{}
	require.False(t, claudeRefresher.NeedsRefresh(anthropicAccount, globalWindow),
		"非 codebuddy 平台必须沿用全局窗口（未被 3 天窗口污染）")

	// OpenAI 刷新器同理。
	openAIRefresher := &OpenAITokenRefresher{}
	openAIAccount := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "at-x",
			"refresh_token": "rt-x",
			"expires_at":    time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	require.False(t, openAIRefresher.NeedsRefresh(openAIAccount, globalWindow),
		"OpenAI 必须沿用全局窗口")

	// 同一窗口下 codebuddy 判定为需要刷新——三者并列，证明窗口确实按平台分流。
	codeBuddyAccount := &Account{
		ID:       43,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token":  "at-x",
			"refresh_token": "rt-x",
			"expires_at":    time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	require.True(t, refresher.NeedsRefresh(codeBuddyAccount, globalWindow),
		"同一时刻 codebuddy 按 3 天窗口判需要刷新")
}
