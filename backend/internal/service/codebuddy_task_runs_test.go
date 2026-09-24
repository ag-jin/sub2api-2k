package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type taskRunRepoStub struct {
	AccountRepository
	acct   *Account
	merges []map[string]any
}

func (s *taskRunRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	return s.acct, nil
}

func (s *taskRunRepoStub) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	s.merges = append(s.merges, updates)
	// 模拟持久化
	for k, v := range updates {
		if s.acct.Extra == nil {
			s.acct.Extra = map[string]any{}
		}
		s.acct.Extra[k] = v
	}
	return nil
}

func TestAppendCodeBuddyTaskRun_NewestFirstAndCap(t *testing.T) {
	acct := &Account{ID: 500, Platform: PlatformCodeBuddy}
	repo := &taskRunRepoStub{acct: acct}

	for i := 0; i < 13; i++ {
		AppendCodeBuddyTaskRun(context.Background(), repo, 500,
			"token_keepalive", "refreshed", "")
	}

	require.Len(t, repo.merges, 13)
	raw := acct.Extra[codeBuddyTaskRunsKey].([]any)
	assert.Len(t, raw, codeBuddyTaskRunsMaxEntries, "环形上限 10 条")
	first := raw[0].(map[string]any)
	assert.Equal(t, "refreshed", first["status"])
}

func TestAppendCodeBuddyTaskRun_ExistingRingPreserved(t *testing.T) {
	old := map[string]any{"at": "old", "task": "checkin", "status": "ok"}
	acct := &Account{ID: 500, Extra: map[string]any{
		codeBuddyTaskRunsKey: []any{old},
	}}
	repo := &taskRunRepoStub{acct: acct}

	AppendCodeBuddyTaskRun(context.Background(), repo, 500, "token_keepalive", "auth_failed", "invalid_refresh_token")

	raw := acct.Extra[codeBuddyTaskRunsKey].([]any)
	require.Len(t, raw, 2)
	newest := raw[0].(map[string]any)
	assert.Equal(t, "auth_failed", newest["status"])
	assert.Equal(t, "old", raw[1]["at"], "旧记录保留在后")
	assert.Equal(t, "invalid_refresh_token", newest["detail"])
}

func TestAppendCodeBuddyTaskRun_NilSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		AppendCodeBuddyTaskRun(context.Background(), nil, 500, "t", "ok", "")
	})
}

// 保活执行后应留痕（M10 验收：执行结果一目了然）
func TestKeepAlive_WritesTaskRunRecords(t *testing.T) {
	acct := mkAcct(500, nil)
	repo := &taskRunRepoStub{acct: acct}
	r := &keepaliveRefresherStub{can: true, creds: map[string]any{"access_token": "n"}}
	svc := &CodeBuddyTokenKeepalive{accountRepo: repo, credsRepo: nil, refresher: r, window: 3 * 24 * time.Hour}

	sum, err := svc.RunKeepAlive(context.Background(), codeBuddyKeepaliveModeScheduled)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Refreshed)

	// repo.merges: [0]=任务留痕? 顺序上留痕在 refresh 后、重置连击前——至少出现一次
	found := false
	for _, m := range repo.merges {
		if _, ok := m[codeBuddyTaskRunsKey]; ok {
			found = true
		}
	}
	assert.True(t, found, "保活执行应写入任务留痕")
}
