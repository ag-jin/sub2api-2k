package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// fakeLocalImageBedStorage 是本地磁盘后端的替身：记录写入/删除/读取，
// 直链形态与 repository.LocalImageBedStorage 一致（<base>/v1/images/bed/<key>）。
type fakeLocalImageBedStorage struct {
	saved   []savedImage
	deleted []string
	opened  []string
	body    string
}

func (f *fakeLocalImageBedStorage) Save(_ context.Context, key, contentType string, data []byte) (string, error) {
	f.saved = append(f.saved, savedImage{key: key, contentType: contentType, data: append([]byte(nil), data...)})
	return "https://ai.facaiai.top/v1/images/bed/" + strings.TrimPrefix(key, "bed/"), nil
}

func (f *fakeLocalImageBedStorage) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeLocalImageBedStorage) Open(key string) (io.ReadCloser, string, error) {
	f.opened = append(f.opened, key)
	return io.NopCloser(strings.NewReader(f.body)), "image/png", nil
}

// newImageBedProviderHarness 走真实的 ProvideImageBedService 装配。
// settings 为 nil 模拟「image_storage 未配置」——生产上无 S3 时的形态。
func newImageBedProviderHarness(t *testing.T, imageBed config.ImageBedConfig, settings *ImageStorageSettingService, local ImageBedLocalStorage) *ImageBedService {
	t.Helper()
	svc := ProvideImageBedService(
		&fakeImageBedRepo{},
		settings,
		&fakeImageBedOwnerResolver{userID: 42},
		&fakeImageBedQuotaCounter{},
		&config.Config{Gateway: config.GatewayConfig{ImageBed: imageBed}},
		local,
	)
	t.Cleanup(svc.Stop)
	return svc
}

// 配好 S3 时行为不变：对象进 S3，本地兜底不参与（公开直链仍是对象存储的直链）。
func TestProvideImageBedServicePrefersS3OverLocalStorage(t *testing.T) {
	s3Storage := &fakeObjectImageStorage{}
	settings := NewImageStorageSettingService(nil, nil, nil, func(context.Context, *config.ImageStorageConfig) (ImageStorage, error) {
		return s3Storage, nil
	}, config.ImageStorageConfig{
		Enabled:         true,
		Bucket:          "bed",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		PublicBaseURL:   "https://cdn.test",
	})
	local := &fakeLocalImageBedStorage{}
	svc := newImageBedProviderHarness(t, config.ImageBedConfig{Enabled: true, LocalEnabled: true}, settings, local)

	got, err := svc.Upload(context.Background(), 9, pngBytes, "image/png")
	require.NoError(t, err)

	require.Len(t, s3Storage.saved, 1)
	require.Equal(t, "bed/"+got.Key, s3Storage.saved[0].key)
	require.Equal(t, "https://cdn.test/bed/"+got.Key, got.URL)
	require.Empty(t, local.saved)
}

// 无 S3 且本地兜底开启（默认）：上传走本地磁盘，返回本站匿名直链，
// 且该 key 能经 OpenLocal 读回——这就是「站点自己当图床」的闭环。
func TestProvideImageBedServiceFallsBackToLocalStorageWithoutS3(t *testing.T) {
	local := &fakeLocalImageBedStorage{body: "fake-png-payload"}
	svc := newImageBedProviderHarness(t, config.ImageBedConfig{Enabled: true, LocalEnabled: true}, nil, local)

	got, err := svc.Upload(context.Background(), 9, pngBytes, "image/png")
	require.NoError(t, err)

	require.Len(t, local.saved, 1)
	require.Equal(t, "bed/"+got.Key, local.saved[0].key)
	require.Equal(t, "image/png", local.saved[0].contentType)
	require.Equal(t, "https://ai.facaiai.top/v1/images/bed/"+got.Key, got.URL)

	reader, contentType, err := svc.OpenLocal(got.Key)
	require.NoError(t, err)
	require.Equal(t, "image/png", contentType)
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, "fake-png-payload", string(body))
	require.Equal(t, []string{got.Key}, local.opened)
}

// 无 S3 且本地兜底被显式关掉：上传仍是 503（现状不变），公开直链 404。
func TestProvideImageBedServiceWithoutS3AndLocalDisabledStaysUnavailable(t *testing.T) {
	local := &fakeLocalImageBedStorage{}
	svc := newImageBedProviderHarness(t, config.ImageBedConfig{Enabled: true, LocalEnabled: false}, nil, local)

	_, err := svc.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedStorageUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Empty(t, local.saved)

	_, _, err = svc.OpenLocal("0123456789abcdef.png")
	require.ErrorIs(t, err, ErrImageBedNotFound)
}

// 未注入本地后端（例如本地兜底关闭）时公开直链回 404，而不是 503：
// 静态资源 404 才是「拿不到」的正确语义。
func TestImageBedServiceOpenLocalWithoutLocalStorageReturnsNotFound(t *testing.T) {
	svc := NewImageBedService(&fakeImageBedRepo{}, nil, nil, nil, &config.Config{})

	_, _, err := svc.OpenLocal("0123456789abcdef.png")
	require.ErrorIs(t, err, ErrImageBedNotFound)
	require.Equal(t, http.StatusNotFound, infraerrors.Code(err))
}

// 本地兜底同样吃 TTL 清理：过期的记账行删的是本地对象（与写入共用同一条
// resolver 链，所以清理不需要按后端分支）。这里刻意不装配定时器，
// 让断言落在确定的状态上（StartCleanup 会立刻扫一轮，与显式调用重复消费同一行）。
func TestImageBedLocalFallbackCleanupDeletesLocalObjects(t *testing.T) {
	local := &fakeLocalImageBedStorage{}
	repo := &fakeImageBedRepo{expired: []*ImageBedUploadRecord{{
		ID:         3,
		Key:        "0123456789abcdef.png",
		StorageKey: "bed/0123456789abcdef.png",
	}}}
	svc := NewImageBedService(
		repo,
		imageBedResolverWithLocalFallback(nil, local),
		&fakeImageBedOwnerResolver{userID: 42},
		&fakeImageBedQuotaCounter{},
		&config.Config{},
	)

	result, err := svc.RunCleanupOnce(context.Background(), time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, result.Deleted)
	require.Equal(t, []string{"bed/0123456789abcdef.png"}, local.deleted)
	require.Equal(t, []int64{3}, repo.deleted)
}
