package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// platformCountRepoStub 同时满足 AccountRepository 与新增的窄能力接口
// AccountPlatformCounter：只实现计数方法，其余方法靠嵌入接口兜底。
type platformCountRepoStub struct {
	AccountRepository
	counts map[string]int64
	err    error
	calls  int
}

func (s *platformCountRepoStub) CountByPlatform(context.Context) (map[string]int64, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.counts, nil
}

// repoWithoutPlatformCounts 只有 AccountRepository 能力：仓储不支持计数能力时必须显式报错，
// 不能静默返回空计数——空计数会被侧边栏读成"所有平台都没有账号"。
type repoWithoutPlatformCounts struct{ AccountRepository }

func TestAdminServiceGetAccountPlatformCountsDelegatesToRepository(t *testing.T) {
	repo := &platformCountRepoStub{counts: map[string]int64{"anthropic": 3, "zhipu": 1}}
	svc := &adminServiceImpl{accountRepo: repo}

	counts, err := svc.GetAccountPlatformCounts(context.Background())

	require.NoError(t, err)
	require.Equal(t, map[string]int64{"anthropic": 3, "zhipu": 1}, counts)
	require.Equal(t, 1, repo.calls)
}

func TestAdminServiceGetAccountPlatformCountsNormalizesEmptyResult(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &platformCountRepoStub{}}

	counts, err := svc.GetAccountPlatformCounts(context.Background())

	require.NoError(t, err)
	require.NotNil(t, counts, "没有账号时必须是空 map，保证 handler 的 JSON 是 {} 而不是 null")
	require.Empty(t, counts)
}

func TestAdminServiceGetAccountPlatformCountsFailsWhenRepositoryLacksCapability(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &repoWithoutPlatformCounts{}}

	counts, err := svc.GetAccountPlatformCounts(context.Background())

	require.Error(t, err)
	require.Nil(t, counts)
}

func TestAdminServiceGetAccountPlatformCountsPropagatesRepositoryError(t *testing.T) {
	repoErr := errors.New("platform counts unavailable")
	svc := &adminServiceImpl{accountRepo: &platformCountRepoStub{err: repoErr}}

	counts, err := svc.GetAccountPlatformCounts(context.Background())

	require.ErrorIs(t, err, repoErr)
	require.Nil(t, counts)
}

func TestAdminServiceGetAccountPlatformCountsFailsWithoutRepository(t *testing.T) {
	svc := &adminServiceImpl{}

	counts, err := svc.GetAccountPlatformCounts(context.Background())

	require.Error(t, err)
	require.Nil(t, counts)
}
