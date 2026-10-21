//go:build unit

package repository

// M3 — design §9.2-13：更新 openai_stream_503_auto_continue 后，
// UpdateExtra → enqueueSchedulerOutbox → syncSchedulerAccountSnapshot → Redis
// 的快照必须能读到新值。这里用 sqlmock 模拟“更新后读回”的数据库状态，
// 用真实 schedulerCache + miniredis 断言生产热路径实际消费的 payload。

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpdateAccountExtraRefreshesSchedulerSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	cache, mr := newSchedulerCacheUnitWithRedis(t)
	repo := newAccountRepositoryWithSQL(client, db, cache)

	ctx := context.Background()
	const accountID int64 = 503
	updates := map[string]any{service.OpenAIStream503AutoContinueExtraKey: true}
	payload, err := json.Marshal(updates)
	require.NoError(t, err)

	// 调度相关 extra 变更：事务内 UPDATE + outbox 入队，提交后主动刷新单账号快照。
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL")).
		WithArgs(string(payload), accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, accountID, nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	// syncSchedulerAccountSnapshot 会重新读取账号；返回更新后的数据库状态。
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "accounts"\."id" = \$1`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows(dbaccount.Columns).AddRow(
			accountID,
			now,
			now,
			nil, // deleted_at
			"cont-snapshot",
			nil, // notes
			service.PlatformOpenAI,
			service.AccountTypeOAuth,
			[]byte(`{"access_token":"snapshot-token"}`),
			[]byte(`{"openai_stream_503_auto_continue":true}`),
			nil,        // proxy_id
			nil,        // proxy_fallback_origin_id
			int64(1),   // concurrency
			nil,        // load_factor
			int64(0),   // priority
			float64(1), // rate_multiplier
			service.StatusActive,
			nil,   // error_message
			nil,   // last_used_at
			nil,   // expires_at
			false, // auto_pause_on_expired
			true,  // schedulable
			nil,   // rate_limited_at
			nil,   // rate_limit_reset_at
			nil,   // overload_until
			nil,   // temp_unschedulable_until
			nil,   // temp_unschedulable_reason
			nil,   // session_window_start
			nil,   // session_window_end
			nil,   // session_window_status
			nil,   // parent_account_id
			string(service.QuotaDimensionGlobal),
		))
	mock.ExpectQuery(`(?s)SELECT .* FROM "account_groups" WHERE .*`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows(dbaccountgroup.Columns))

	require.NoError(t, repo.UpdateExtra(ctx, accountID, updates))
	require.NoError(t, mock.ExpectationsWereMet())

	// GetAccount 消费的是同步写入的全量 payload。
	cached, err := cache.GetAccount(ctx, accountID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.True(t, cached.IsOpenAIStream503AutoContinueEnabled())

	// GetSnapshot 消费的是 meta payload（白名单投影）；只断言该键仍在，
	// 证明开关既触发了快照刷新，也没有被调度快照白名单裁掉。
	metaRaw, err := mr.Get(schedulerAccountMetaKey("503"))
	require.NoError(t, err)
	var meta service.Account
	require.NoError(t, json.Unmarshal([]byte(metaRaw), &meta))
	require.True(t, meta.IsOpenAIStream503AutoContinueEnabled())
}
