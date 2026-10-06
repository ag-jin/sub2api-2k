package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// ListAccountsByCredentialFlow 的 SQL 条件测试（sqlmock + captureQuerySQL 接缝，
// 与 account_repo_temp_unsched_test.go / account_repo_upstream_billing_probe_due_test.go 同法）。
func TestAccountRepositoryListAccountsByCredentialFlowSQL(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var capturedSQL string
	var capturedArgs []any
	mock.ExpectQuery("SELECT id").
		WithArgs(service.ZhipuLoginAuthFlow).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL, args: &capturedArgs}, nil)

	accounts, err := repo.ListAccountsByCredentialFlow(context.Background(), service.ZhipuLoginAuthFlow)

	require.NoError(t, err)
	require.Empty(t, accounts)

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "FROM accounts")
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.Contains(t, normalized, "credentials->>'auth_flow' = $1")
	require.Contains(t, normalized, "ORDER BY id ASC")
	// 与通用 token 刷新候选集完全分开：不得夹带刷新候选集的任何条件。
	require.NotContains(t, normalized, "schedulable")
	require.NotContains(t, normalized, "refresh_token")
	require.NotContains(t, normalized, "type =")
	require.Equal(t, []any{service.ZhipuLoginAuthFlow}, capturedArgs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepositoryListAccountsByCredentialFlowRejectsEmptyFlow(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db}, nil)

	accounts, err := repo.ListAccountsByCredentialFlow(context.Background(), "   ")

	require.Error(t, err)
	require.Nil(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet(), "空 flow 不得发起查询")
}

func TestAccountRepositoryListAccountsByCredentialFlowRejectsMissingSQLExecutor(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil)

	accounts, err := repo.ListAccountsByCredentialFlow(context.Background(), service.ZhipuLoginAuthFlow)

	require.Error(t, err)
	require.Nil(t, accounts)
}
