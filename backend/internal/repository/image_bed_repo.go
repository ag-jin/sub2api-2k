package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// defaultImageBedCleanupBatchSize 是清理扫描的兜底批量（service 侧同值）。
const defaultImageBedCleanupBatchSize = 100

// imageBedRepository 是站点图床上传记账的仓储（纯 SQL）。
//
// 选型说明：表只有六列、查询只有 insert / select_expired / delete 三条，
// 为此引入 ent 代码生成不划算，因此与迁移 240_image_bed_uploads.sql 一一对应地
// 手写 SQL（先例：channelMonitor 的聚合查询、audit_logs 的保留期清理）。
type imageBedRepository struct {
	db *sql.DB
}

var _ service.ImageBedUploadRepository = (*imageBedRepository)(nil)

func NewImageBedRepository(db *sql.DB) service.ImageBedUploadRepository {
	return &imageBedRepository{db: db}
}

func (r *imageBedRepository) InsertImageBedUpload(ctx context.Context, upload *service.ImageBedUploadRecord) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil image bed repository")
	}
	if upload == nil {
		return fmt.Errorf("nil image bed upload")
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO image_bed_uploads (key, storage_key, user_id, content_type, size_bytes, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		upload.Key,
		upload.StorageKey,
		upload.UserID,
		upload.ContentType,
		upload.SizeBytes,
		upload.CreatedAt.UTC(),
		upload.ExpiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert image bed upload: %w", err)
	}
	return nil
}

// ListExpiredImageBedUploads 按 expires_at 升序取一批过期记录（最久未清理的优先）。
func (r *imageBedRepository) ListExpiredImageBedUploads(ctx context.Context, now time.Time, limit int) ([]*service.ImageBedUploadRecord, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil image bed repository")
	}
	if limit <= 0 {
		limit = defaultImageBedCleanupBatchSize
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, key, storage_key, user_id, content_type, size_bytes, created_at, expires_at
FROM image_bed_uploads
WHERE expires_at <= $1
ORDER BY expires_at
LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired image bed uploads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*service.ImageBedUploadRecord, 0, limit)
	for rows.Next() {
		var record service.ImageBedUploadRecord
		if err := rows.Scan(
			&record.ID,
			&record.Key,
			&record.StorageKey,
			&record.UserID,
			&record.ContentType,
			&record.SizeBytes,
			&record.CreatedAt,
			&record.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan image bed upload: %w", err)
		}
		out = append(out, &record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate image bed uploads: %w", err)
	}
	return out, nil
}

func (r *imageBedRepository) DeleteImageBedUpload(ctx context.Context, id int64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil image bed repository")
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM image_bed_uploads WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete image bed upload: %w", err)
	}
	return nil
}
