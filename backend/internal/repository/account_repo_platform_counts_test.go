package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// CountByPlatform 的 SQL 语义测试（sqlmock + captureQuerySQL 接缝，与
// account_repo_credential_flow_test.go 同法）。
//
// 侧边栏「账号管理」的平台子项靠这个计数隐藏"平台下没有账号"的链接，所以：
//   - 必须是单条按 platform 分组的聚合查询（不能拉全表在内存里数）；
//   - 必须显式排除软删除，账号删空后计数立刻归零。
func TestAccountRepositoryCountByPlatformSQL(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var capturedSQL string
	mock.ExpectQuery("SELECT platform").
		WillReturnRows(sqlmock.NewRows([]string{"platform", "count"}).
			AddRow("anthropic", int64(3)).
			AddRow("zhipu", int64(1)))

	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL}, nil)

	counts, err := repo.CountByPlatform(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"anthropic": 3, "zhipu": 1}, counts)

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "FROM accounts")
	require.Contains(t, normalized, "COUNT(*)")
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.Contains(t, normalized, "GROUP BY platform")
	require.NoError(t, mock.ExpectationsWereMet(), "平台计数必须只执行一条聚合查询")
}

func TestAccountRepositoryCountByPlatformReturnsEmptyMapWhenNoRows(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery("SELECT platform").
		WillReturnRows(sqlmock.NewRows([]string{"platform", "count"}))

	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db}, nil)

	counts, err := repo.CountByPlatform(context.Background())
	require.NoError(t, err)
	require.NotNil(t, counts, "无账号时必须返回空 map，JSON 才会序列化成 {} 而不是 null")
	require.Empty(t, counts)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepositoryCountByPlatformRejectsMissingSQLExecutor(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil)

	counts, err := repo.CountByPlatform(context.Background())
	require.Error(t, err)
	require.Nil(t, counts)
}
