package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T3 保活调度器 tick 测试（A4/A5/A6 同款：默认关闭 / 窗口内一次 / 窗口外补刷）。

const keepaliveEnabledWindow = `{"codebuddy":{"token_keepalive":{"enabled":true,"start":{"hour":22,"minute":0},"end":{"hour":23,"minute":0}}}}`

type keepaliveSchedulerEnv struct {
	scheduler *CodeBuddyTokenKeepaliveScheduler
	refresher *keepaliveRefresherStub
	repo      *keepaliveRepoStub
	zone      *time.Location
	current   time.Time
}

func newKeepaliveSchedulerEnv(t *testing.T, featuresJSON string, accounts ...*Account) *keepaliveSchedulerEnv {
	t.Helper()
	repo := newPlatformFeatureTestRepo()
	if featuresJSON != "" {
		repo.vals[SettingFeatureStorageKeyForTest()] = featuresJSON
	}
	svc := newPlatformFeatureTestService(t, repo)
	refresher := &keepaliveRefresherStub{can: true, creds: map[string]any{"access_token": "n"}}
	kaAccts := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		kaAccts = append(kaAccts, *a)
	}
	kaRepo := &keepaliveRepoStub{accts: kaAccts}
	ka := NewCodeBuddyTokenKeepalive(kaRepo, nil, refresher)
	scheduler := NewCodeBuddyTokenKeepaliveScheduler(ka, svc)
	env := &keepaliveSchedulerEnv{
		scheduler: scheduler, refresher: refresher, repo: kaRepo, zone: codeBuddyTimeZone,
	}
	scheduler.clock = func() time.Time { return env.current }
	return env
}

func (e *keepaliveSchedulerEnv) at(t *testing.T, day, hhmm string) {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, e.zone)
	require.NoError(t, err)
	e.current = parsed
	e.scheduler.tick()
}

func TestKeepaliveScheduler_FeatureOff_Noop(t *testing.T) {
	env := newKeepaliveSchedulerEnv(t, "", mkAcct(500, nil))
	env.at(t, "2026-09-24", "22:30")
	assert.Zero(t, env.refresher.called, "默认关闭时不得对上游发刷新")
}

func TestKeepaliveScheduler_InWindow_FullRunOncePerDay(t *testing.T) {
	env := newKeepaliveSchedulerEnv(t, keepaliveEnabledWindow, mkAcct(500, nil))
	env.at(t, "2026-09-24", "22:10")
	called := env.refresher.called
	assert.Equal(t, 1, called, "窗口内全量刷新")
	env.at(t, "2026-09-24", "22:50")
	assert.Equal(t, called, env.refresher.called, "同日窗口内去重")
	// 次日窗口：重新全量
	env.at(t, "2026-09-25", "22:10")
	assert.Equal(t, called+1, env.refresher.called)
}

func TestKeepaliveScheduler_OutsideWindow_OnlyExpiring(t *testing.T) {
	expiring := mkAcct(504, nil)
	fresh := mkAcct(500, nil)
	env := newKeepaliveSchedulerEnv(t, keepaliveEnabledWindow, expiring, fresh)
	// refresher.needs=true → 两号都算"将过期"；验证窗口外走 expiry 补刷(M3)
	env.refresher.needs = true
	env.at(t, "2026-09-24", "10:00")
	assert.Positive(t, env.refresher.called, "窗口外补刷将过期号(M3)")
	assert.NotContains(t, env.repo.updates, codeBuddyKeepaliveDisabledKey)
}
