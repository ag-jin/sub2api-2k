package service

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// ──────────────────────────────────────────────────────────
// 测试替身：图床的四个协作方（记账仓储 / 对象存储 / 归属解析 / 配额计数）
// ──────────────────────────────────────────────────────────

type fakeImageBedRepo struct {
	inserted  []*ImageBedUploadRecord
	expired   []*ImageBedUploadRecord
	deleted   []int64
	insertErr error
	listErr   error
	deleteErr map[int64]error
	listNow   time.Time
	listLimit int
}

func (r *fakeImageBedRepo) InsertImageBedUpload(_ context.Context, upload *ImageBedUploadRecord) error {
	if r.insertErr != nil {
		return r.insertErr
	}
	copied := *upload
	r.inserted = append(r.inserted, &copied)
	return nil
}

func (r *fakeImageBedRepo) ListExpiredImageBedUploads(_ context.Context, now time.Time, limit int) ([]*ImageBedUploadRecord, error) {
	r.listNow, r.listLimit = now, limit
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.expired, nil
}

func (r *fakeImageBedRepo) DeleteImageBedUpload(_ context.Context, id int64) error {
	if err := r.deleteErr[id]; err != nil {
		return err
	}
	r.deleted = append(r.deleted, id)
	return nil
}

type fakeImageBedOwnerResolver struct {
	userID int64
	err    error
	calls  []int64
}

func (r *fakeImageBedOwnerResolver) ResolveImageBedOwner(_ context.Context, apiKeyID int64) (int64, error) {
	r.calls = append(r.calls, apiKeyID)
	if r.err != nil {
		return 0, r.err
	}
	return r.userID, nil
}

// fakeImageBedQuotaCounter 模拟 Redis 计数器：start 表示本小时窗口已用张数，
// 每次调用递增后再返回累计值。
type fakeImageBedQuotaCounter struct {
	start  int64
	calls  int
	err    error
	window time.Duration
}

func (c *fakeImageBedQuotaCounter) IncrImageBedQuota(_ context.Context, _ int64, window time.Duration) (int64, error) {
	c.calls++
	c.window = window
	if c.err != nil {
		return 0, c.err
	}
	return c.start + int64(c.calls), nil
}

type imageBedTestHarness struct {
	service *ImageBedService
	repo    *fakeImageBedRepo
	storage *fakeObjectImageStorage
	owners  *fakeImageBedOwnerResolver
	counter *fakeImageBedQuotaCounter
	now     time.Time
}

// newImageBedTestHarness 装配一个图床服务：storageReady 为假时模拟
// 「image_storage 未配置」（resolver 返回未启用），这是生产上无 S3 时的形态。
func newImageBedTestHarness(imageBed config.ImageBedConfig, storageReady bool) *imageBedTestHarness {
	repo := &fakeImageBedRepo{}
	storage := &fakeObjectImageStorage{}
	owners := &fakeImageBedOwnerResolver{userID: 42}
	counter := &fakeImageBedQuotaCounter{}
	resolver := ImageStorageResolver(func() (*ImageResultUploader, bool) {
		if !storageReady {
			return nil, false
		}
		return NewImageResultUploader(storage, "images/", 0, nil), true
	})
	svc := NewImageBedService(repo, resolver, owners, counter, &config.Config{
		Gateway: config.GatewayConfig{ImageBed: imageBed},
	})
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	return &imageBedTestHarness{service: svc, repo: repo, storage: storage, owners: owners, counter: counter, now: now}
}

// ──────────────────────────────────────────────────────────
// Upload：正常路径与记账
// ──────────────────────────────────────────────────────────

func TestImageBedServiceUploadStoresObjectAndRecordsRow(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, TTLHours: 24}, true)

	got, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.NoError(t, err)

	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{32}\.png$`), got.Key)
	require.Equal(t, "https://cdn.test/bed/"+got.Key, got.URL)
	require.Equal(t, h.now.Add(24*time.Hour), got.ExpiresAt)

	// 对象落在 bed/ 前缀，内容与声明一致（不套生图结果的 images/ 前缀）。
	require.Len(t, h.storage.saved, 1)
	require.Equal(t, "bed/"+got.Key, h.storage.saved[0].key)
	require.Equal(t, "image/png", h.storage.saved[0].contentType)
	require.Equal(t, pngBytes, h.storage.saved[0].data)

	// TTL 记账行：key + 归属 + 过期时间（清理任务按 expires_at 扫描）。
	require.Len(t, h.repo.inserted, 1)
	row := h.repo.inserted[0]
	require.Equal(t, got.Key, row.Key)
	require.Equal(t, "bed/"+got.Key, row.StorageKey)
	require.EqualValues(t, 42, row.UserID)
	require.Equal(t, "image/png", row.ContentType)
	require.EqualValues(t, len(pngBytes), row.SizeBytes)
	require.Equal(t, h.now, row.CreatedAt)
	require.Equal(t, h.now.Add(24*time.Hour), row.ExpiresAt)

	require.Equal(t, []int64{9}, h.owners.calls)
	require.Equal(t, 1, h.counter.calls)
	require.Equal(t, imageBedQuotaWindow, h.counter.window)
}

// 未指定 TTL 时用默认 24h（票面：默认 24h 自动清理）。
func TestImageBedServiceUploadUsesDefaultTTL(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)

	got, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.NoError(t, err)
	require.Equal(t, h.now.Add(defaultImageBedTTL), got.ExpiresAt)
}

// ──────────────────────────────────────────────────────────
// Upload：类型魔数校验（仅 png/jpg/jpeg/webp/gif）
// ──────────────────────────────────────────────────────────

func TestImageBedServiceUploadValidatesImageType(t *testing.T) {
	jpegBytes := []byte("\xFF\xD8\xFF\xE0fake-jpeg-payload")
	gifBytes := []byte("GIF89a-fake-gif-payload")
	webpBytes := []byte("RIFF\x00\x00\x00\x00WEBPVP8 fake-webp")

	cases := []struct {
		name     string
		data     []byte
		declared string
		wantType string
		wantExt  string
	}{
		{name: "png", data: pngBytes, declared: "image/png", wantType: "image/png", wantExt: ".png"},
		{name: "jpeg", data: jpegBytes, declared: "image/jpeg", wantType: "image/jpeg", wantExt: ".jpg"},
		{name: "jpg alias", data: jpegBytes, declared: "image/jpg", wantType: "image/jpeg", wantExt: ".jpg"},
		{name: "gif", data: gifBytes, declared: "image/gif", wantType: "image/gif", wantExt: ".gif"},
		{name: "webp", data: webpBytes, declared: "image/webp", wantType: "image/webp", wantExt: ".webp"},
		{name: "declared type absent", data: pngBytes, declared: "", wantType: "image/png", wantExt: ".png"},
		{name: "declared type with charset", data: pngBytes, declared: "image/png; charset=binary", wantType: "image/png", wantExt: ".png"},
		// multipart 部件默认就是 application/octet-stream（curl/Python 常见），
		// 非图片声明不作判据，真实类型看魔数。
		{name: "declared octet-stream", data: pngBytes, declared: "application/octet-stream", wantType: "image/png", wantExt: ".png"},
		{name: "declared type mismatched with magic", data: pngBytes, declared: "image/jpeg", wantType: "image/png", wantExt: ".png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)

			got, err := h.service.Upload(context.Background(), 9, tc.data, tc.declared)
			require.NoError(t, err)
			require.Len(t, h.storage.saved, 1)
			require.Equal(t, tc.wantType, h.storage.saved[0].contentType)
			require.True(t, strings.HasSuffix(got.Key, tc.wantExt), "key %q must end with %q", got.Key, tc.wantExt)
		})
	}
}

func TestImageBedServiceUploadRejectsNonImagePayloads(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		declared string
	}{
		{name: "text bytes declared as png", data: []byte("hello, definitely not an image"), declared: "image/png"},
		{name: "declared unsupported image type", data: pngBytes, declared: "image/bmp"},
		{name: "svg", data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), declared: "image/svg+xml"},
		{name: "bmp", data: []byte("BM fake-bmp"), declared: "image/bmp"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)

			_, err := h.service.Upload(context.Background(), 9, tc.data, tc.declared)
			require.ErrorIs(t, err, ErrImageBedUnsupportedType)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Empty(t, h.storage.saved)
			require.Empty(t, h.repo.inserted)
		})
	}
}

// ──────────────────────────────────────────────────────────
// Upload：大小限制（默认单文件 ≤10MiB）
// ──────────────────────────────────────────────────────────

func TestImageBedServiceUploadRejectsEmptyAndOversizePayloads(t *testing.T) {
	t.Run("empty payload", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)

		_, err := h.service.Upload(context.Background(), 9, nil, "image/png")
		require.ErrorIs(t, err, ErrImageBedEmptyFile)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Empty(t, h.storage.saved)
	})

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, MaxBytes: int64(len(pngBytes))}, true)

		_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
		require.NoError(t, err)
	})

	t.Run("one byte over the limit is rejected", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, MaxBytes: int64(len(pngBytes))}, true)
		oversize := append(append([]byte(nil), pngBytes...), 'x')

		_, err := h.service.Upload(context.Background(), 9, oversize, "image/png")
		require.ErrorIs(t, err, ErrImageBedFileTooLarge)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Empty(t, h.storage.saved)
		require.Empty(t, h.repo.inserted)
	})

	t.Run("default limit is 10MiB", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
		require.EqualValues(t, 10<<20, h.service.MaxUploadBytes())
	})
}

// ──────────────────────────────────────────────────────────
// Upload：存储不可用（无 S3）→ 503，且不消耗配额
// ──────────────────────────────────────────────────────────

func TestImageBedServiceUploadWithoutStorageReturnsUnavailable(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, false)

	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedStorageUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Equal(t, "image_bed_storage_unavailable", infraerrors.Reason(err))
	require.Zero(t, h.counter.calls, "存储不可用时不得消耗配额")
	require.Empty(t, h.repo.inserted)
}

// 图床总开关（gateway.image_bed.enabled）关闭时整体不可用。
func TestImageBedServiceUploadDisabled(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: false}, true)

	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedDisabled)
	require.Equal(t, http.StatusNotFound, infraerrors.Code(err))
	require.Empty(t, h.storage.saved)
	require.Zero(t, h.counter.calls)
}

// ──────────────────────────────────────────────────────────
// Upload：每小时配额（Redis 计数；Redis 故障退化为进程内存）
// ──────────────────────────────────────────────────────────

func TestImageBedServiceUploadEnforcesHourlyQuota(t *testing.T) {
	t.Run("at the limit is accepted", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, HourlyLimitPerKey: 2}, true)
		h.counter.start = 1 // 本小时已用 1 张

		_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
		require.NoError(t, err)
	})

	t.Run("over the limit is rejected", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, HourlyLimitPerKey: 2}, true)
		h.counter.start = 2

		_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
		require.ErrorIs(t, err, ErrImageBedQuotaExceeded)
		require.Equal(t, http.StatusTooManyRequests, infraerrors.Code(err))
		require.Equal(t, "image_bed_quota_exceeded", infraerrors.Reason(err))
		require.Empty(t, h.storage.saved, "超限请求不得写对象")
		require.Empty(t, h.repo.inserted)
	})

	t.Run("default limit is 60 per key per hour", func(t *testing.T) {
		h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
		h.counter.start = 59

		_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
		require.NoError(t, err)
		require.Equal(t, imageBedQuotaWindow, h.counter.window)
	})
}

func TestImageBedServiceUploadDegradesQuotaToMemoryWhenCounterFails(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, HourlyLimitPerKey: 2}, true)
	h.counter.err = errors.New("redis unavailable")

	for i := 0; i < 2; i++ {
		_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
		require.NoError(t, err)
	}
	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedQuotaExceeded, "Redis 故障时必须退化为进程内存计数")
}

func TestImageBedServiceUploadWithoutCounterCountsPerKeyInMemory(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true, HourlyLimitPerKey: 1}, true)
	h.service.counter = nil

	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.NoError(t, err)
	_, err = h.service.Upload(context.Background(), 10, pngBytes, "image/png")
	require.NoError(t, err, "另一个 key 的配额互不影响")
	_, err = h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedQuotaExceeded)
}

// ──────────────────────────────────────────────────────────
// Upload：失败路径不得留下无记账的对象
// ──────────────────────────────────────────────────────────

func TestImageBedServiceUploadFailsWhenStorageWriteFails(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
	h.storage.err = errors.New("bucket unreachable")

	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedUploadFailed)
	require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
	require.Empty(t, h.repo.inserted)
}

func TestImageBedServiceUploadRemovesObjectWhenRecordInsertFails(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
	h.repo.insertErr = errors.New("insert failed")

	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrImageBedUploadFailed)
	require.Len(t, h.storage.saved, 1)
	require.Equal(t, []string{h.storage.saved[0].key}, h.storage.deleted, "记账失败必须回收已写入的对象")
}

func TestImageBedServiceUploadFailsWhenOwnerUnresolved(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
	h.owners.err = ErrAPIKeyNotFound

	_, err := h.service.Upload(context.Background(), 9, pngBytes, "image/png")
	require.ErrorIs(t, err, ErrAPIKeyNotFound)
	require.Empty(t, h.storage.saved)
	require.Empty(t, h.repo.inserted)
}

// ──────────────────────────────────────────────────────────
// 清理：按 expires_at 扫描，先删对象再删行
// ──────────────────────────────────────────────────────────

func TestImageBedServiceRunCleanupOnceDeletesExpiredObjectsAndRows(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
	h.repo.expired = []*ImageBedUploadRecord{
		{ID: 1, Key: "aaa.png", StorageKey: "bed/aaa.png"},
		{ID: 2, Key: "bbb.png", StorageKey: "bed/bbb.png"},
	}

	got, err := h.service.RunCleanupOnce(context.Background(), h.now)
	require.NoError(t, err)
	require.Equal(t, ImageBedCleanupResult{Deleted: 2}, got)
	require.Equal(t, []string{"bed/aaa.png", "bed/bbb.png"}, h.storage.deleted)
	require.Equal(t, []int64{1, 2}, h.repo.deleted)
	require.Equal(t, h.now, h.repo.listNow)
	require.Equal(t, defaultImageBedCleanupBatchSize, h.repo.listLimit)
}

func TestImageBedServiceRunCleanupOnceWithoutExpiredRows(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)

	got, err := h.service.RunCleanupOnce(context.Background(), h.now)
	require.NoError(t, err)
	require.Equal(t, ImageBedCleanupResult{}, got)
	require.Empty(t, h.storage.deleted)
	require.Empty(t, h.repo.deleted)
}

// 对象删除失败时保留记账行，下一轮再试——否则对象会变成孤儿。
func TestImageBedServiceRunCleanupOnceKeepsRowWhenObjectDeleteFails(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
	h.repo.expired = []*ImageBedUploadRecord{{ID: 1, StorageKey: "bed/aaa.png"}}
	h.storage.err = errors.New("bucket unreachable")

	got, err := h.service.RunCleanupOnce(context.Background(), h.now)
	require.NoError(t, err)
	require.Equal(t, ImageBedCleanupResult{Failures: 1}, got)
	require.Empty(t, h.repo.deleted)
}

func TestImageBedServiceRunCleanupOnceSkipsWhenStorageUnavailable(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, false)
	h.repo.expired = []*ImageBedUploadRecord{{ID: 1, StorageKey: "bed/aaa.png"}}

	got, err := h.service.RunCleanupOnce(context.Background(), h.now)
	require.NoError(t, err)
	require.Equal(t, ImageBedCleanupResult{Skipped: 1}, got)
	require.Empty(t, h.repo.deleted)
	require.Empty(t, h.storage.deleted)
}

func TestImageBedServiceRunCleanupOnceSurfacesListFailure(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)
	h.repo.listErr = errors.New("db down")

	_, err := h.service.RunCleanupOnce(context.Background(), h.now)
	require.ErrorIs(t, err, h.repo.listErr)
}

// ──────────────────────────────────────────────────────────
// 清理定时器：重复 Start/Stop 不 panic、不重复启动
// ──────────────────────────────────────────────────────────

func TestImageBedServiceCleanupTickerStartStopIsIdempotent(t *testing.T) {
	h := newImageBedTestHarness(config.ImageBedConfig{Enabled: true}, true)

	require.NotPanics(t, func() {
		h.service.StartCleanup()
		h.service.StartCleanup()
		h.service.Stop()
		h.service.Stop()
	})
}
