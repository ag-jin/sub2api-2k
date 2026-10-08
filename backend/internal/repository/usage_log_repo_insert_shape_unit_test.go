//go:build unit

package repository

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var (
	usageLogStaticInsertShapeRe = regexp.MustCompile(`(?s)INSERT INTO usage_logs \((.*?)\) VALUES \((.*?)\)`)
	usageLogPlaceholderRe       = regexp.MustCompile(`\$(\d+)`)
)

// newSQLCapturingMock 返回把实际下发 SQL 记录到 captured 的 sqlmock；语句一律视为匹配，
// 参数仍由 WithArgs 校验。
func newSQLCapturingMock(t *testing.T, captured *[]string) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	matcher := sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
		*captured = append(*captured, actualSQL)
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// requireStaticInsertMatchesArgTypes 断言手写的 INSERT：列清单长度与 VALUES 占位符数量
// 都等于 usageLogInsertArgTypes，且占位符恰为 $1..$N 各出现一次。
func requireStaticInsertMatchesArgTypes(t *testing.T, query string) {
	t.Helper()
	m := usageLogStaticInsertShapeRe.FindStringSubmatch(query)
	require.Len(t, m, 3, "unrecognised INSERT shape:\n%s", query)

	want := len(usageLogInsertArgTypes)
	columns := 0
	for _, col := range strings.Split(m[1], ",") {
		if strings.TrimSpace(col) != "" {
			columns++
		}
	}
	require.Equal(t, want, columns, "INSERT column list must match usageLogInsertArgTypes")

	seen := make(map[int]struct{}, want)
	for _, ph := range usageLogPlaceholderRe.FindAllStringSubmatch(m[2], -1) {
		n, err := strconv.Atoi(ph[1])
		require.NoError(t, err)
		_, dup := seen[n]
		require.False(t, dup, "duplicate placeholder $%d", n)
		seen[n] = struct{}{}
	}
	require.Len(t, seen, want, "VALUES placeholder count must match usageLogInsertArgTypes")
	for i := 1; i <= want; i++ {
		_, ok := seen[i]
		require.True(t, ok, "missing placeholder $%d", i)
	}
}

// TestUsageLogStaticInsertShape_PlaceholdersMatchArgTypes 覆盖两条不经占位符生成器、
// 直接手写 $1..$N 的 INSERT 路径，防止加列后漏补占位符只在集成测试才暴露。
func TestUsageLogStaticInsertShape_PlaceholdersMatchArgTypes(t *testing.T) {
	upstreamRequestID := "20260902080329-oneapi"
	log := &service.UsageLog{
		UserID:            1,
		APIKeyID:          2,
		AccountID:         3,
		RequestID:         "client:insert-shape",
		UpstreamRequestID: &upstreamRequestID,
		Model:             "claude-3",
		InputTokens:       10,
		CreatedAt:         time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}
	prepared := prepareUsageLogInsert(log)
	args := anySliceToDriverValues(prepared.args)

	t.Run("createSingle", func(t *testing.T) {
		var captured []string
		db, mock := newSQLCapturingMock(t, &captured)
		repo := &usageLogRepository{sql: db}

		mock.ExpectQuery("INSERT INTO usage_logs").
			WithArgs(args...).
			WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), log.CreatedAt))

		inserted, err := repo.Create(context.Background(), log)
		require.NoError(t, err)
		require.True(t, inserted)
		require.NoError(t, mock.ExpectationsWereMet())
		require.Len(t, captured, 1)
		requireStaticInsertMatchesArgTypes(t, captured[0])
	})

	t.Run("execUsageLogInsertNoResult", func(t *testing.T) {
		var captured []string
		db, mock := newSQLCapturingMock(t, &captured)

		mock.ExpectExec("INSERT INTO usage_logs").
			WithArgs(args...).
			WillReturnResult(sqlmock.NewResult(0, 1))

		require.NoError(t, execUsageLogInsertNoResult(context.Background(), db, prepared))
		require.NoError(t, mock.ExpectationsWereMet())
		require.Len(t, captured, 1)
		requireStaticInsertMatchesArgTypes(t, captured[0])
	})
}

// usageLogSQLList 抽取生成 SQL 中由 start/end 标记界定的逗号分隔清单
// （去空白、去空项），用于逐位核对同一张表的列清单是否同步。
func usageLogSQLList(t *testing.T, query, start, end string) []string {
	t.Helper()
	startIdx := strings.Index(query, start)
	require.GreaterOrEqual(t, startIdx, 0, "start marker %q not found in query:\n%s", start, query)
	rest := query[startIdx+len(start):]
	endIdx := strings.Index(rest, end)
	require.GreaterOrEqual(t, endIdx, 0, "end marker %q not found in query:\n%s", end, query)

	var out []string
	for _, item := range strings.Split(rest[:endIdx], ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// TestUsageLogBestEffortInsertShape_ListsStayAligned 是 best-effort 批量插入的长期守卫：
// WITH input 列清单、INSERT 目标列清单与 SELECT 表达式必须逐位同名且列数等于
// usageLogInsertArgTypes。背景（票 #38 A）：这三个清单在同一函数里各写一遍，
// upstream_credit 只在 WITH/INSERT 补齐、SELECT 漏写，导致 100% 触发
// `INSERT has more target columns than expressions`，请求退化为逐行兜底插入。
// 列数守卫在此挡住"改一处漏一处"。
func TestUsageLogBestEffortInsertShape_ListsStayAligned(t *testing.T) {
	preparedList := []usageLogInsertPrepared{
		prepareUsageLogInsert(&service.UsageLog{
			UserID:      1,
			APIKeyID:    2,
			AccountID:   3,
			RequestID:   "client:best-effort-shape-1",
			Model:       "glm-5",
			InputTokens: 10,
			CreatedAt:   time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
		}),
		prepareUsageLogInsert(&service.UsageLog{
			UserID:       4,
			APIKeyID:     5,
			AccountID:    6,
			RequestID:    "client:best-effort-shape-2",
			Model:        "glm-5",
			OutputTokens: 7,
			CreatedAt:    time.Date(2026, 10, 8, 12, 0, 1, 0, time.UTC),
		}),
	}
	require.Len(t, preparedList, 2, "batch branch must be exercised with >= 2 rows")

	query, args := buildUsageLogBestEffortInsertQuery(preparedList)

	withColumns := usageLogSQLList(t, query, "WITH input (", ") AS (VALUES")
	insertColumns := usageLogSQLList(t, query, "INSERT INTO usage_logs (", "SELECT")
	selectExprs := usageLogSQLList(t, query, "SELECT", "FROM input")

	want := len(usageLogInsertArgTypes)
	require.Equal(t, want, len(withColumns), "WITH input column count must match usageLogInsertArgTypes")
	require.Equal(t, want, len(insertColumns), "INSERT target column count must match usageLogInsertArgTypes")
	require.Equal(t, want, len(selectExprs), "SELECT expression count must match usageLogInsertArgTypes")
	require.Equal(t, withColumns, insertColumns, "INSERT column list must mirror the WITH input list")
	require.Equal(t, withColumns, selectExprs, "SELECT expressions must mirror the WITH input list")
	require.Contains(t, selectExprs, "upstream_credit", "SELECT must carry upstream_credit")
	require.Contains(t, insertColumns, "upstream_credit", "INSERT target list must carry upstream_credit")

	require.Len(t, args, len(preparedList)*want, "each row contributes len(usageLogInsertArgTypes) args")
}

// TestPrepareUsageLogInsert_UpstreamRequestIDArgWiring 把 upstream_request_id 钉在
// session_id 之前，与参数类型表保持同位；缺失时落 NULL 而不是空串。
func TestPrepareUsageLogInsert_UpstreamRequestIDArgWiring(t *testing.T) {
	upstreamRequestID := "req_upstream_123"
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:            1,
		APIKeyID:          2,
		RequestID:         "client:wiring",
		Model:             "gpt-5",
		UpstreamRequestID: &upstreamRequestID,
		CreatedAt:         time.Now().UTC(),
	})
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	idx := len(prepared.args) - 5
	arg, ok := prepared.args[idx].(sql.NullString)
	require.True(t, ok, "upstream_request_id arg should be sql.NullString, got %T", prepared.args[idx])
	require.True(t, arg.Valid)
	require.Equal(t, upstreamRequestID, arg.String)
	require.Equal(t, "text", usageLogInsertArgTypes[idx])

	absent := prepareUsageLogInsert(&service.UsageLog{UserID: 1, APIKeyID: 2, RequestID: "client:absent", Model: "gpt-5", CreatedAt: time.Now().UTC()})
	nullArg, ok := absent.args[idx].(sql.NullString)
	require.True(t, ok)
	require.False(t, nullArg.Valid, "absent upstream request id must be NULL")

	require.Contains(t, usageLogSelectColumns, "upstream_request_id")
}
