package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestAccountRepository_UpdateCodeBuddyCredentialsIfUnchanged_UsesExactAttemptStateAndAtomicOutbox
// 对齐 UpdateGrokOAuthCredentialsIfUnchanged 先例：成功持久化必须以完整凭据文档
// + proxy 的 CAS 条件为前提，并原子推送 scheduler outbox。
func TestAccountRepository_UpdateCodeBuddyCredentialsIfUnchanged_UsesExactAttemptStateAndAtomicOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	proxyID := int64(29)

	applied, err := repo.UpdateCodeBuddyCredentialsIfUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "attempted"},
		&proxyID,
		map[string]any{"refresh_token": "rotated"},
	)

	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "WITH updated AS")
	require.Contains(t, normalized, "credentials = $1::jsonb")
	require.Contains(t, normalized, "credentials = $5::jsonb")
	require.Contains(t, normalized, "proxy_id IS NOT DISTINCT FROM $6")
	require.Contains(t, normalized, "platform = $3")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Len(t, exec.execArgs[0], 7)
	require.Equal(t, &proxyID, exec.execArgs[0][5])
	// platform/type 以参数绑定，不是 SQL 字面量。
	require.Equal(t, service.PlatformCodeBuddy, exec.execArgs[0][2])
	require.Equal(t, service.AccountTypeAPIKey, exec.execArgs[0][3])
}

func TestAccountRepository_SetCodeBuddyRefreshErrorIfCredentialsUnchanged_SQLShape(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	proxyID := int64(29)

	applied, err := repo.SetCodeBuddyRefreshErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "attempted"},
		&proxyID,
		"refresh rejected",
	)

	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "status = $1")
	require.Contains(t, normalized, "error_message = $2")
	require.Contains(t, normalized, "schedulable = FALSE")
	require.Contains(t, normalized, "credentials = $7::jsonb")
	require.Contains(t, normalized, "status = $6")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Len(t, exec.execArgs[0], 9)
	require.Equal(t, &proxyID, exec.execArgs[0][7])
	require.Equal(t, service.StatusError, exec.execArgs[0][0], "成功/Active 账号应写 StatusError")
	require.Equal(t, service.StatusActive, exec.execArgs[0][5], "CAS 仅命中处于 Active 态的账号")
}

func TestAccountRepository_SetCodeBuddyRefreshTempUnschedulableIfCredentialsUnchanged_SQLShape(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	until := time.Now().Add(10 * time.Minute)

	applied, err := repo.SetCodeBuddyRefreshTempUnschedulableIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "attempted"},
		nil,
		until,
		"retry",
	)

	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "temp_unschedulable_until = $1")
	require.Contains(t, normalized, "temp_unschedulable_reason = $2")
	require.Contains(t, normalized, "credentials = $7::jsonb")
	require.Contains(t, normalized, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until < $1")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Len(t, exec.execArgs[0], 9)
	require.Nil(t, exec.execArgs[0][7], "expectedProxyID 为 nil 时 CAS 条件不变")
}

func TestAccountRepository_CodeBuddyConditionalMutations_ZeroRowsSkipOutbox(t *testing.T) {
	mutations := map[string]func(r *accountRepository, exec *recordingSQLExecutor) (bool, error){
		"success_cas": func(r *accountRepository, exec *recordingSQLExecutor) (bool, error) {
			return r.UpdateCodeBuddyCredentialsIfUnchanged(
				context.Background(), 42, map[string]any{}, nil, map[string]any{})
		},
		"error_cas": func(r *accountRepository, exec *recordingSQLExecutor) (bool, error) {
			return r.SetCodeBuddyRefreshErrorIfCredentialsUnchanged(
				context.Background(), 42, map[string]any{}, nil, "err")
		},
		"temp_unsched_cas": func(r *accountRepository, exec *recordingSQLExecutor) (bool, error) {
			return r.SetCodeBuddyRefreshTempUnschedulableIfCredentialsUnchanged(
				context.Background(), 42, map[string]any{}, nil, time.Now(), "reason")
		},
	}
	for name, fn := range mutations {
		t.Run(name, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			applied, err := fn(repo, exec)
			require.NoError(t, err)
			require.False(t, applied, "0 行受影响 → 视为并发覆盖，跳过改写")
			require.Len(t, exec.execQueries, 1)
		})
	}
}

var _ = sql.ErrNoRows
var _ = regexp.MustCompile("")
