package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type keepaliveRepoStub struct {
	AccountRepository
	accts   []Account
	updates []map[int64]map[string]any
	tempUnschedCalls []int64
}

func (s *keepaliveRepoStub) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return s.accts, nil
}

func (s *keepaliveRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	s.tempUnschedCalls = append(s.tempUnschedCalls, id)
	return nil
}

func (s *keepaliveRepoStub) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	if s.updates == nil {
		s.updates = []map[int64]map[string]any{}
	}
	s.updates = append(s.updates, map[int64]map[string]any{id: updates})
	// 模拟 DB 持久化：合并回账号，下一轮 ListByPlatform 才能读到上一轮的 streak
	for i := range s.accts {
		if s.accts[i].ID != id {
			continue
		}
		if s.accts[i].Extra == nil {
			s.accts[i].Extra = map[string]any{}
		}
		for k, v := range updates {
			s.accts[i].Extra[k] = v
		}
	}
	return nil
}

type keepaliveRefresherStub struct {
	can    bool
	needs  bool
	creds  map[string]any
	err    error
	called int
}

func (s *keepaliveRefresherStub) CanRefresh(*Account) bool { return s.can }
func (s *keepaliveRefresherStub) NeedsRefresh(*Account, time.Duration) bool {
	return s.needs
}
func (s *keepaliveRefresherStub) Refresh(context.Context, *Account) (map[string]any, error) {
	s.called++
	return s.creds, s.err
}

type keepaliveCredsStub struct {
	applied bool
	updated []int64
}

func (s *keepaliveCredsStub) UpdateCodeBuddyCredentialsIfUnchanged(
	ctx context.Context, id int64, expected map[string]any, proxyID *int64, creds map[string]any,
) (bool, error) {
	s.updated = append(s.updated, id)
	return s.applied, nil
}

func mkAcct(id int64, extra map[string]any) *Account {
	if extra == nil {
		extra = map[string]any{}
	}
	return &Account{ID: id, Platform: PlatformCodeBuddy, Status: StatusActive,
		Schedulable: true, Credentials: map[string]any{"refresh_token": "rt"}, Extra: extra}
}

func newKeepalive(accts []Account, r *keepaliveRefresherStub, c *keepaliveCredsStub) *CodeBuddyTokenKeepalive {
	return &CodeBuddyTokenKeepalive{
		accountRepo: &keepaliveRepoStub{accts: accts},
		credsRepo:   c,
		refresher:   r,
		window:      3 * 24 * time.Hour,
	}
}

func TestKeepAlive_Scheduled_RefreshesAllIncludingManualDisabled(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, creds: map[string]any{"access_token": "new"}}
	c := &keepaliveCredsStub{applied: true}
	manualDisabled := mkAcct(504, map[string]any{"manual_disabled": map[string]any{"enabled": true}})
	svc := newKeepalive([]Account{*mkAcct(500, nil), *manualDisabled}, r, c)

	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Total)
	assert.Equal(t, 2, sum.Refreshed, "scheduled 模式全量刷新，含手动停用号(A9:停用期保活照常)")
	assert.Equal(t, 2, r.called)
	assert.Equal(t, []int64{500, 504}, c.updated)
}

func TestKeepAlive_ExpiryMode_OnlyExpiring(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, needs: true, creds: map[string]any{"access_token": "n"}}
	c := &keepaliveCredsStub{applied: true}
	svc := newKeepalive([]Account{*mkAcct(500, nil)}, r, c)

	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeExpiry)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Refreshed)
}

func TestKeepAlive_ExpiryMode_SkipsNotExpiring(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, needs: false}
	svc := newKeepalive([]Account{*mkAcct(500, nil)}, r, &keepaliveCredsStub{})

	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeExpiry)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Skipped)
	assert.Zero(t, r.called)
}

func TestKeepAlive_AuthRejected_ThreeStrikesDisables(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, err: newCodeBuddyRefreshAuthError(400, "invalid_refresh_token")}
	c := &keepaliveCredsStub{}
	repo := &keepaliveRepoStub{accts: []Account{*mkAcct(504, nil)}}
	svc := &CodeBuddyTokenKeepalive{accountRepo: repo, credsRepo: c, refresher: r, window: 3 * 24 * time.Hour}

	for i := 1; i <= 3; i++ {
		sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
		require.NoError(t, err)
		assert.Equal(t, 1, sum.AuthFailed)
		if i < 3 {
			assert.Empty(t, sum.Disabled, "第%d次失败不应停用", i)
		} else {
			assert.Equal(t, []int64{504}, sum.Disabled, "连续3次失效才停用")
		assert.Contains(t, repo.tempUnschedCalls, int64(504), "三振同时长冷却摘出对话池(审查修正)")
		}
	}
	// 第4轮：已标记 keepalive_disabled → 跳过不再撞上游
	r.err = nil
	calledBefore := r.called
	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Skipped)
	assert.Equal(t, calledBefore, r.called, "已停用的号不再发起刷新调用")
}

func TestKeepAlive_TransientError_DoesNotCountStrike(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, err: errors.New("network timeout")}
	svc := newKeepalive([]Account{*mkAcct(500, nil)}, r, &keepaliveCredsStub{})

	for i := 0; i < 3; i++ {
		sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
		require.NoError(t, err)
		assert.Equal(t, 1, sum.Failed)
		assert.Empty(t, sum.Disabled, "非凭据失效(网络错误)不计连击")
	}
}

func TestKeepAlive_CredentialsChangedDuringRefresh_Skips(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, creds: map[string]any{"access_token": "n"}}
	c := &keepaliveCredsStub{applied: false} // 条件更新未命中=刷新期间凭据已变
	svc := newKeepalive([]Account{*mkAcct(500, nil)}, r, c)

	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Skipped)
	assert.Equal(t, "credentials_changed_during_refresh", sum.Results[0].Detail)
}

func TestKeepAlive_InactiveAccountExcluded(t *testing.T) {
	r := &keepaliveRefresherStub{can: true, creds: map[string]any{}}
	dead := mkAcct(600, nil)
	dead.Status = "disabled"
	svc := newKeepalive([]Account{*dead, *mkAcct(500, nil)}, r, &keepaliveCredsStub{applied: true})

	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Total, "非 active 账号不进保活")
}
