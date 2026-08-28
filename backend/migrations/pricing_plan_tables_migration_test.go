package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPricingPlanTablesMigration(t *testing.T) {
	content, err := FS.ReadFile("229_pricing_plan_tables.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 三张新表
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS pricing_plans")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS pricing_plan_models")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS pricing_plan_routes")

	// 套餐表：公开产品标记 + 软删除后允许重名的部分唯一索引
	require.Contains(t, sql, "is_public BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plans_unique_active_name")
	require.Contains(t, sql, "ON pricing_plans (name)")
	require.Contains(t, sql, "WHERE deleted_at IS NULL")

	// 模型协议条目表：协议 CHECK 约束与部分唯一索引
	require.Contains(t, sql, "CONSTRAINT pricing_plan_models_protocol_check CHECK (protocol IN ('chat_completions', 'messages', 'responses'))")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plan_models_unique_active")
	require.Contains(t, sql, "ON pricing_plan_models (plan_id, public_model, protocol)")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_pricing_plan_models_plan_priority")

	// 路由表是私有内部池层：每层只绑定一个 group_id 与优先级。
	require.Contains(t, sql, "group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plan_routes_unique_active_group")
	require.Contains(t, sql, "ON pricing_plan_routes (plan_id, group_id)")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plan_routes_unique_active_priority")
	require.Contains(t, sql, "ON pricing_plan_routes (plan_id, priority)")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_pricing_plan_routes_plan_priority")
	require.Contains(t, sql, "ON pricing_plan_routes (plan_id, priority, id)")

	// api_keys 新增可空 pricing_plan_id，group_id 保持原样（无改列/删列/删约束语句）
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS pricing_plan_id BIGINT REFERENCES pricing_plans(id) ON DELETE SET NULL")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_api_keys_pricing_plan_id")
	require.NotContains(t, sql, "ALTER COLUMN")
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "DROP CONSTRAINT")
}
