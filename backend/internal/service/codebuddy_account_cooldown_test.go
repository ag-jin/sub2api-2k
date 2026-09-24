package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A7 分级冷却（吸收自 workbuddy2api）：
//   - 402 / 余额耗尽(14018) → 硬冷却至次日 04:00 UTC+8
//   - 404 → 浅冷却 10m
//   - 429/6004 不走本通道（模型级停调已有独立路径）

type cooldownRepoStub struct {
	AccountRepository
	until  time.Time
	reason string
	called bool
}

func (s *cooldownRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	s.called = true
	s.until = until
	s.reason = reason
	return nil
}

func TestCodeBuddyAccountCooldown_402HardUntilNextFourAM(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy}

	// 假设现在 2026-09-24 12:00 +08 → 次日(09-25) 04:00 +08
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, codeBuddyTimeZone)
	until, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusPaymentRequired, nil, now)
	require.True(t, ok)
	want := time.Date(2026, 9, 25, 4, 0, 0, 0, codeBuddyTimeZone)
	assert.Equal(t, want, until)
	assert.True(t, repo.called)
}

func TestCodeBuddyAccountCooldown_402LateNight_SameDayFourAM(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy}

	// 02:00 触发 → 当天 04:00
	now := time.Date(2026, 9, 24, 2, 0, 0, 0, codeBuddyTimeZone)
	until, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusPaymentRequired, nil, now)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 9, 24, 4, 0, 0, 0, codeBuddyTimeZone), until)
}

func TestCodeBuddyAccountCooldown_Body14018CountsAsQuota(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy}

	body := []byte(`{"code":14018,"msg":"账号积分耗尽"}`)
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, codeBuddyTimeZone)
	until, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusForbidden, body, now)
	require.True(t, ok, "业务码 14018 视同余额耗尽（与 HTTP 402 同通道）")
	assert.Equal(t, time.Date(2026, 9, 25, 4, 0, 0, 0, codeBuddyTimeZone), until)
}

func TestCodeBuddyAccountCooldown_404ShallowTenMinutes(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy}

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, codeBuddyTimeZone)
	until, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusNotFound, nil, now)
	require.True(t, ok)
	assert.Equal(t, now.Add(10*time.Minute), until)
}

func TestCodeBuddyAccountCooldown_OtherStatusesNotHandled(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy}

	for _, sc := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusOK} {
		_, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, sc, nil, time.Now())
		assert.False(t, ok, "状态 %d 不应被分级冷却处理", sc)
	}
	assert.False(t, repo.called)
}

func TestCodeBuddyAccountCooldown_NonCodeBuddySkipped(t *testing.T) {
	repo := &cooldownRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{ID: 1, Platform: PlatformOpenAI}

	_, ok := rls.TriggerCodeBuddyAccountCooldownAt(acct, http.StatusPaymentRequired, nil, time.Now())
	assert.False(t, ok)
	assert.False(t, repo.called)
}

func TestCodeBuddyActiveModelRateLimits(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy, Extra: map[string]any{
		"model_rate_limits": map[string]any{
			"deepseek-v4.1-flash": map[string]any{
				"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339),
			},
			"glm-5.2": map[string]any{
				"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
			},
		},
	}}
	got := CodeBuddyActiveModelRateLimits(acct, now)
	assert.Len(t, got, 1, "过期条目不返回")
	assert.Contains(t, got, "deepseek-v4.1-flash")
}

func TestCodeBuddyTokenExpiresAt(t *testing.T) {
	at, ok := CodeBuddyTokenExpiresAt(&Account{Credentials: map[string]any{
		"expires_at": "2026-10-01T00:00:00Z"}})
	assert.True(t, ok)
	assert.Equal(t, "2026-10-01", at.Format("2006-01-02"))
	_, ok = CodeBuddyTokenExpiresAt(&Account{Credentials: map[string]any{"expires_at": 1727000000000.0}})
	assert.True(t, ok, "毫秒数值形态")
	_, ok = CodeBuddyTokenExpiresAt(&Account{Credentials: map[string]any{}})
	assert.False(t, ok)
}
