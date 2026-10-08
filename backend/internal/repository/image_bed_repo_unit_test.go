package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 图床记账仓储是纯 SQL：这里用 sqlmock 钉住语句形态与扫描列，
// 保证它与迁移 240_image_bed_uploads.sql 的列一一对应。
func TestImageBedRepositoryInsertShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	createdAt := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	expiresAt := createdAt.Add(24 * time.Hour)
	mock.ExpectExec(`(?s)INSERT INTO image_bed_uploads \(key, storage_key, user_id, content_type, size_bytes, created_at, expires_at\).*VALUES \(`).
		WithArgs("abc.png", "bed/abc.png", int64(42), "image/png", int64(23), createdAt, expiresAt).
		WillReturnResult(sqlmock.NewResult(1, 1))

	repo := NewImageBedRepository(db)
	require.NoError(t, repo.InsertImageBedUpload(context.Background(), &service.ImageBedUploadRecord{
		Key:         "abc.png",
		StorageKey:  "bed/abc.png",
		UserID:      42,
		ContentType: "image/png",
		SizeBytes:   23,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestImageBedRepositoryListExpiredScansRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	createdAt := now.Add(-25 * time.Hour)
	expiresAt := now.Add(-time.Hour)
	rows := sqlmock.NewRows([]string{
		"id", "key", "storage_key", "user_id", "content_type", "size_bytes", "created_at", "expires_at",
	}).AddRow(int64(7), "aaa.png", "bed/aaa.png", int64(42), "image/png", int64(23), createdAt, expiresAt)
	mock.ExpectQuery(`(?s)SELECT id, key, storage_key, user_id, content_type, size_bytes, created_at, expires_at.*FROM image_bed_uploads.*WHERE expires_at <= \$1.*ORDER BY expires_at.*LIMIT \$2`).
		WithArgs(now, 100).
		WillReturnRows(rows)

	repo := NewImageBedRepository(db)
	got, err := repo.ListExpiredImageBedUploads(context.Background(), now, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.EqualValues(t, 7, got[0].ID)
	require.Equal(t, "aaa.png", got[0].Key)
	require.Equal(t, "bed/aaa.png", got[0].StorageKey)
	require.EqualValues(t, 42, got[0].UserID)
	require.Equal(t, "image/png", got[0].ContentType)
	require.EqualValues(t, 23, got[0].SizeBytes)
	require.True(t, got[0].ExpiresAt.Equal(expiresAt))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestImageBedRepositoryDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec(`(?s)DELETE FROM image_bed_uploads WHERE id = \$1`).
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewImageBedRepository(db)
	require.NoError(t, repo.DeleteImageBedUpload(context.Background(), 7))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 未配置 DB 时（构造期为 nil）不得 panic，而是返回明确错误。
func TestImageBedRepositoryWithoutDatabase(t *testing.T) {
	repo := NewImageBedRepository(nil)
	require.Error(t, repo.InsertImageBedUpload(context.Background(), &service.ImageBedUploadRecord{}))
	_, err := repo.ListExpiredImageBedUploads(context.Background(), time.Now(), 10)
	require.Error(t, err)
	require.Error(t, repo.DeleteImageBedUpload(context.Background(), 1))
}
