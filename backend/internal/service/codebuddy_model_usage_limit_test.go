package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 生产实测报文（账号 504，2026-09-24）：6004 模型用量超限，带精确重置时刻。
const codeBuddyUsageLimitBodyFixture = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-24 15:45:02 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"30574444-4ffb-8703-4ab0-a428"}`

func TestParseCodeBuddyModelUsageLimit_ProductionBody(t *testing.T) {
	resetAt, ok := ParseCodeBuddyModelUsageLimit(http.StatusTooManyRequests, []byte(codeBuddyUsageLimitBodyFixture))
	require.True(t, ok)
	assert.Equal(t, "2026-09-24 15:45:02 +0800", resetAt.Format("2006-01-02 15:04:05 -0700"))
}

func TestParseCodeBuddyModelUsageLimit_Only429(t *testing.T) {
	_, ok := ParseCodeBuddyModelUsageLimit(http.StatusOK, []byte(codeBuddyUsageLimitBodyFixture))
	assert.False(t, ok)
}

func TestParseCodeBuddyModelUsageLimit_Non6004Code(t *testing.T) {
	body := `{"code":14018,"msg":"账号积分耗尽"}`
	_, ok := ParseCodeBuddyModelUsageLimit(http.StatusTooManyRequests, []byte(body))
	assert.False(t, ok)
}

func TestParseCodeBuddyModelUsageLimit_MissingResetTime_FallsBack(t *testing.T) {
	body := `{"code":6004,"msg":"您的使用量已超出频率限制"}`
	resetAt, ok := ParseCodeBuddyModelUsageLimit(http.StatusTooManyRequests, []byte(body))
	require.True(t, ok)
	assert.Greater(t, resetAt.Sub(time.Now()), time.Duration(0), "兜底时刻应在未来")
}

func TestParseCodeBuddyModelUsageLimit_EmptyBody(t *testing.T) {
	_, ok := ParseCodeBuddyModelUsageLimit(http.StatusTooManyRequests, nil)
	assert.False(t, ok)
}

func TestClampCodeBuddyUsageReset(t *testing.T) {
	now := time.Now()
	assert.Equal(t, now.Add(codeBuddyUsageLimitMinDuration), clampCodeBuddyUsageReset(now.Add(-time.Hour), now))
	assert.Equal(t, now.Add(codeBuddyUsageLimitMaxDuration), clampCodeBuddyUsageReset(now.Add(48*time.Hour), now))
	want := now.Add(2 * time.Hour)
	assert.Equal(t, want, clampCodeBuddyUsageReset(want, now))
}

type codeBuddyUsageLimitRepoStub struct {
	// 内嵌接口以凑齐 AccountRepository；测试只触达下面两个被覆写的方法。
	AccountRepository
	err              error
	modelRateLimited []struct {
		scope   string
		resetAt time.Time
		reason  string
	}
	tempUnschedCalled bool
}

func (s *codeBuddyUsageLimitRepoStub) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	joined := ""
	if len(reason) > 0 {
		joined = reason[0]
	}
	s.modelRateLimited = append(s.modelRateLimited, struct {
		scope   string
		resetAt time.Time
		reason  string
	}{scope, resetAt, joined})
	return s.err
}

func (s *codeBuddyUsageLimitRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	s.tempUnschedCalled = true
	return nil
}

func TestTriggerCodeBuddyModelUsageLimit_ModelScopedOnly(t *testing.T) {
	repo := &codeBuddyUsageLimitRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{Platform: PlatformCodeBuddy}

	until, ok := rls.TriggerCodeBuddyModelUsageLimit(context.Background(), acct, "deepseek-v4.1-flash", http.StatusTooManyRequests, []byte(codeBuddyUsageLimitBodyFixture))
	require.True(t, ok)
	require.Len(t, repo.modelRateLimited, 1)
	assert.Equal(t, "deepseek-v4.1-flash", repo.modelRateLimited[0].scope)
	assert.Equal(t, "2026-09-24 15:45:02 +0800", repo.modelRateLimited[0].resetAt.Format("2006-01-02 15:04:05 -0700"))
	assert.Equal(t, "2026-09-24 15:45:02 +0800", until.Format("2006-01-02 15:04:05 -0700"))
	assert.False(t, repo.tempUnschedCalled, "绝不停调整个账号")
	assert.Contains(t, repo.modelRateLimited[0].reason, "6004")
}

func TestTriggerCodeBuddyModelUsageLimit_NonCodeBuddySkipped(t *testing.T) {
	repo := &codeBuddyUsageLimitRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{Platform: PlatformOpenAI}

	_, ok := rls.TriggerCodeBuddyModelUsageLimit(context.Background(), acct, "m", http.StatusTooManyRequests, []byte(codeBuddyUsageLimitBodyFixture))
	assert.False(t, ok)
	assert.Empty(t, repo.modelRateLimited)
}

func TestTriggerCodeBuddyModelUsageLimit_Non6004Skipped(t *testing.T) {
	repo := &codeBuddyUsageLimitRepoStub{}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{Platform: PlatformCodeBuddy}

	_, ok := rls.TriggerCodeBuddyModelUsageLimit(context.Background(), acct, "m", http.StatusTooManyRequests, []byte(`{"code":0}`))
	assert.False(t, ok)
	assert.Empty(t, repo.modelRateLimited)
}

func TestTriggerCodeBuddyModelUsageLimit_PersistFailStillFailsOver(t *testing.T) {
	repo := &codeBuddyUsageLimitRepoStub{err: errors.New("db down")}
	rls := &RateLimitService{accountRepo: repo}
	acct := &Account{Platform: PlatformCodeBuddy}

	_, ok := rls.TriggerCodeBuddyModelUsageLimit(context.Background(), acct, "m", http.StatusTooManyRequests, []byte(codeBuddyUsageLimitBodyFixture))
	assert.True(t, ok, "持久化失败也不得把当前请求放回同一账号")
	assert.False(t, repo.tempUnschedCalled, "绝不扩大成账号级停调")
}

func TestFailoverOpenAIUpstreamHTTPError_CodeBuddy6004Wired(t *testing.T) {
	repo := &codeBuddyUsageLimitRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
	acct := &Account{Platform: PlatformCodeBuddy}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}

	foErr := svc.failoverOpenAIUpstreamHTTPError(
		context.Background(), nil, acct, resp,
		[]byte(codeBuddyUsageLimitBodyFixture),
		"您的使用量已超出频率限制",
		"deepseek-v4.1-flash",
	)
	require.NotNil(t, foErr)
	assert.Equal(t, http.StatusTooManyRequests, foErr.StatusCode)
	require.Len(t, repo.modelRateLimited, 1, "应写入 (账号,模型) 级停调")
	assert.False(t, repo.tempUnschedCalled, "不得账号级停调")
}
