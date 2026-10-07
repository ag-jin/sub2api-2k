package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeObjectImageStorage 在 fakeImageStorage（见 image_storage_test.go）之上补一个
// Delete，用来验证图床需要的两项存储能力：按显式 key 写入、按 key 删除对象。
// 注意：不声明自有 err 字段——Save/Delete 必须共用内嵌 fakeImageStorage.err，
// 否则字段遮蔽会让 Save 路径看不到测试注入的错误（曾致 Upload 失败路径漏测）。
type fakeObjectImageStorage struct {
	fakeImageStorage
	deleted []string
}

func (f *fakeObjectImageStorage) Delete(_ context.Context, key string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, key)
	return nil
}

// 图床对象必须落在调用方给定的 key 上（bed/ 前缀）：公开直链是
// <public_base_url>/bed/<key>，因此不能复用生图结果的 images/ 前缀。
func TestImageResultUploaderSaveObjectUsesExplicitKey(t *testing.T) {
	storage := &fakeObjectImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 0, nil)

	url, err := uploader.SaveObject(context.Background(), "bed/abc123.png", "image/png", pngBytes)
	require.NoError(t, err)
	require.Equal(t, "https://cdn.test/bed/abc123.png", url)
	require.Len(t, storage.saved, 1)
	require.Equal(t, "bed/abc123.png", storage.saved[0].key)
	require.Equal(t, "image/png", storage.saved[0].contentType)
	require.Equal(t, pngBytes, storage.saved[0].data)
}

func TestImageResultUploaderSaveObjectWithoutStorage(t *testing.T) {
	uploader := NewImageResultUploader(nil, "images/", 0, nil)

	_, err := uploader.SaveObject(context.Background(), "bed/abc123.png", "image/png", pngBytes)
	require.ErrorIs(t, err, ErrImageStorageUnavailable)
}

func TestImageResultUploaderDeleteObject(t *testing.T) {
	storage := &fakeObjectImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 0, nil)

	require.NoError(t, uploader.DeleteObject(context.Background(), "bed/abc123.png"))
	require.Equal(t, []string{"bed/abc123.png"}, storage.deleted)
}

// 未实现删除能力的存储（当前只有 S3 兼容实现有）必须报明确错误，
// 而不是静默当成删除成功——否则图床清理会悄悄漏掉对象。
func TestImageResultUploaderDeleteObjectUnsupportedByStorage(t *testing.T) {
	uploader := NewImageResultUploader(&fakeImageStorage{}, "images/", 0, nil)

	err := uploader.DeleteObject(context.Background(), "bed/abc123.png")
	require.ErrorIs(t, err, ErrImageStorageDeleteUnsupported)
}

func TestImageResultUploaderDeleteObjectWithoutStorage(t *testing.T) {
	uploader := NewImageResultUploader(nil, "images/", 0, nil)

	err := uploader.DeleteObject(context.Background(), "bed/abc123.png")
	require.ErrorIs(t, err, ErrImageStorageUnavailable)
}
