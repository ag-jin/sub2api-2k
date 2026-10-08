package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 图床记账表的列必须与 repository/image_bed_repo.go 的语句一一对应：
// 少一列就是运行期 SQL 报错，多一列是死列。这里把形状钉在迁移文件上。
func TestImageBedUploadsMigrationShape(t *testing.T) {
	content, err := FS.ReadFile("240_image_bed_uploads.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS image_bed_uploads")
	require.Contains(t, sql, "id BIGSERIAL PRIMARY KEY")
	require.Contains(t, sql, "key VARCHAR(128) NOT NULL UNIQUE")
	require.Contains(t, sql, "storage_key VARCHAR(512) NOT NULL")
	require.Contains(t, sql, "user_id BIGINT NOT NULL")
	require.Contains(t, sql, "content_type VARCHAR(64) NOT NULL")
	require.Contains(t, sql, "size_bytes BIGINT NOT NULL")
	require.Contains(t, sql, "created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()")
	require.Contains(t, sql, "expires_at TIMESTAMPTZ NOT NULL")

	// 清理扫描靠这个索引（WHERE expires_at <= ... ORDER BY expires_at）。
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS image_bed_uploads_expires_at_idx ON image_bed_uploads (expires_at)")

	// 幂等 + 不碰无关表。
	require.Contains(t, sql, "IF NOT EXISTS")
	require.NotContains(t, strings.ToUpper(sql), "ALTER TABLE")
	require.NotContains(t, strings.ToUpper(sql), "DROP ")
}
