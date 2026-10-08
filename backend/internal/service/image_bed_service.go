package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 站点图床（票 #36）：把 API 调用方上传的图片落到 image_storage 配置的对象存储，
// 返回公开直链供外部抓取（智谱识图工具链等），并按 TTL 清理过期对象。
const (
	// imageBedStorageKeyPrefix 把图床对象与生图结果（images/ 前缀）隔离开。
	// 公开直链形如 <public_base_url>/bed/<key>，所以这里不套用 uploader 的 prefix。
	imageBedStorageKeyPrefix = "bed/"
	// imageBedQuotaWindow 是配额窗口长度（票面：每 key 每小时）。
	// 计数键的形态（image_bed:quota:{api_key_id}）由 Redis 实现持有：
	// repository/image_bed_quota_counter.go。
	imageBedQuotaWindow = time.Hour
	// imageBedIDBytes 是不透明文件名的随机字节数（32 位十六进制）。
	imageBedIDBytes = 16

	defaultImageBedMaxBytes         int64 = 10 << 20 // 10 MiB
	defaultImageBedHourlyLimit            = 60
	defaultImageBedTTL                    = 24 * time.Hour
	defaultImageBedCleanupInterval        = 30 * time.Minute
	defaultImageBedCleanupBatchSize       = 100
	// imageBedMemoryQuotaMaxEntries 限制进程内存配额表的规模（键为 API key id）。
	imageBedMemoryQuotaMaxEntries = 1024
)

// 错误码用 snake_case（与网关其它端点的大写码不同）：票面点名的
// image_bed_storage_unavailable 就是这套码的形态。
var (
	ErrImageBedDisabled           = infraerrors.New(http.StatusNotFound, "image_bed_disabled", "image bed API is disabled")
	ErrImageBedStorageUnavailable = infraerrors.New(http.StatusServiceUnavailable, "image_bed_storage_unavailable", "image bed object storage is not configured")
	ErrImageBedMissingFile        = infraerrors.New(http.StatusBadRequest, "image_bed_missing_file", `multipart field "file" is required`)
	ErrImageBedInvalidUpload      = infraerrors.New(http.StatusBadRequest, "image_bed_invalid_upload", "multipart upload could not be read")
	ErrImageBedEmptyFile          = infraerrors.New(http.StatusBadRequest, "image_bed_empty_file", "uploaded image is empty")
	ErrImageBedFileTooLarge       = infraerrors.New(http.StatusBadRequest, "image_bed_file_too_large", "uploaded image exceeds the size limit")
	ErrImageBedUnsupportedType    = infraerrors.New(http.StatusBadRequest, "image_bed_unsupported_type", "unsupported image type (png/jpeg/webp/gif only)")
	ErrImageBedQuotaExceeded      = infraerrors.New(http.StatusTooManyRequests, "image_bed_quota_exceeded", "hourly image bed upload quota exceeded")
	ErrImageBedUploadFailed       = infraerrors.New(http.StatusBadGateway, "image_bed_upload_failed", "image bed upload failed")
)

var errImageBedQuotaCounterMissing = errors.New("image bed: no quota counter configured")

// ImageBedUploadRecord 是一条图床上传记账行（表 image_bed_uploads）。
// 清理任务按 ExpiresAt 扫描：先删对象，再删行。
type ImageBedUploadRecord struct {
	ID          int64
	Key         string // URL 里的不透明文件名（不含前缀）
	StorageKey  string // 对象存储中的 key：imageBedStorageKeyPrefix + Key
	UserID      int64
	ContentType string
	SizeBytes   int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// ImageBedUploadRepository 是图床上传记账仓储（纯 SQL 实现见 repository/image_bed_repo.go）。
type ImageBedUploadRepository interface {
	InsertImageBedUpload(ctx context.Context, upload *ImageBedUploadRecord) error
	ListExpiredImageBedUploads(ctx context.Context, now time.Time, limit int) ([]*ImageBedUploadRecord, error)
	DeleteImageBedUpload(ctx context.Context, id int64) error
}

// ImageBedOwnerResolver 把 API key id 解析成归属用户 id。
// 上传只带 apiKeyID（网关鉴权得到的身份），而记账行需要 user_id，故用这个窄适配器
// 隔离 APIKeyService（与 BatchImageAccountResolver 同类）。
type ImageBedOwnerResolver interface {
	ResolveImageBedOwner(ctx context.Context, apiKeyID int64) (int64, error)
}

// imageBedOwnerResolverFunc 让 provider 用闭包直接实现 ImageBedOwnerResolver。
type imageBedOwnerResolverFunc func(ctx context.Context, apiKeyID int64) (int64, error)

func (f imageBedOwnerResolverFunc) ResolveImageBedOwner(ctx context.Context, apiKeyID int64) (int64, error) {
	return f(ctx, apiKeyID)
}

// ImageBedQuotaCounter 是每 API key 的配额计数器（Redis 实现，跨实例共享）。
// 未装配或调用失败时，ImageBedService 退化为进程内存计数（先例：zhipu 计数）。
type ImageBedQuotaCounter interface {
	IncrImageBedQuota(ctx context.Context, apiKeyID int64, window time.Duration) (int64, error)
}

// ImageBedUploadResult 是上传接口的响应体。
type ImageBedUploadResult struct {
	Key       string    `json:"key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ImageBedCleanupResult 是一次过期清理的结果。
type ImageBedCleanupResult struct {
	// Deleted 是「对象与行都已删除」的条数。
	Deleted int
	// Failures 是删除对象/行失败、留待下一轮重试的条数。
	Failures int
	// Skipped 是对象存储不可用、整轮跳过的条数（行保留，绝不静默漏掉对象）。
	Skipped int
}

type imageBedSettings struct {
	enabled     bool
	maxBytes    int64
	hourlyLimit int
	ttl         time.Duration
}

// ImageBedService 是站点图床：上传、公开直链返回、TTL 记账与过期清理。
type ImageBedService struct {
	repo    ImageBedUploadRepository
	resolve ImageStorageResolver
	owners  ImageBedOwnerResolver
	counter ImageBedQuotaCounter
	cfg     *config.Config
	// local 是本地磁盘后端（无 S3 时的兜底），由 ProvideImageBedService 注入；
	// 公开直链 GET /v1/images/bed/:key 经 OpenLocal 读它（见 image_bed_local.go）。
	local ImageBedLocalStorage

	now func() time.Time

	quotaMu       sync.Mutex
	memoryQuota   map[int64]imageBedQuotaEntry
	quotaDegraded sync.Once

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

type imageBedQuotaEntry struct {
	count   int64
	resetAt time.Time
}

func NewImageBedService(
	repo ImageBedUploadRepository,
	resolve ImageStorageResolver,
	owners ImageBedOwnerResolver,
	counter ImageBedQuotaCounter,
	cfg *config.Config,
) *ImageBedService {
	return &ImageBedService{
		repo:    repo,
		resolve: resolve,
		owners:  owners,
		counter: counter,
		cfg:     cfg,
		now:     time.Now,
	}
}

// MaxUploadBytes 是单文件大小上限（后台可配，默认 10MiB）。
// handler 用它限制 multipart 读取量，避免把超大请求体整个读进内存。
func (s *ImageBedService) MaxUploadBytes() int64 {
	settings := s.settings()
	return settings.maxBytes
}

// Upload 校验并存储一张图片，登记 TTL 行，返回公开 URL。
//
// 校验顺序：开关 → 大小/类型 → 存储可用性 → 配额 → 归属 → 写对象 → 记账。
// 存储不可用时在扣配额之前就返回 503，避免把「服务未配置」算到调用方头上。
func (s *ImageBedService) Upload(ctx context.Context, apiKeyID int64, data []byte, contentType string) (*ImageBedUploadResult, error) {
	if s == nil || s.repo == nil {
		return nil, ErrImageBedUploadFailed
	}
	settings := s.settings()
	if !settings.enabled {
		return nil, ErrImageBedDisabled
	}
	if len(data) == 0 {
		return nil, ErrImageBedEmptyFile
	}
	if int64(len(data)) > settings.maxBytes {
		return nil, ErrImageBedFileTooLarge
	}
	imageType, err := imageBedImageType(data, contentType)
	if err != nil {
		return nil, err
	}
	uploader, ok := s.storageUploader()
	if !ok {
		return nil, ErrImageBedStorageUnavailable
	}
	if s.reserveQuota(ctx, apiKeyID) > int64(settings.hourlyLimit) {
		return nil, ErrImageBedQuotaExceeded
	}
	userID, err := s.resolveOwner(ctx, apiKeyID)
	if err != nil {
		return nil, err
	}
	key, err := newImageBedKey(imageType)
	if err != nil {
		return nil, ErrImageBedUploadFailed.WithCause(err)
	}
	storageKey := imageBedStorageKeyPrefix + key
	url, err := uploader.SaveObject(ctx, storageKey, imageType, data)
	if err != nil {
		return nil, ErrImageBedUploadFailed.WithCause(err)
	}

	now := s.now().UTC()
	record := &ImageBedUploadRecord{
		Key:         key,
		StorageKey:  storageKey,
		UserID:      userID,
		ContentType: imageType,
		SizeBytes:   int64(len(data)),
		CreatedAt:   now,
		ExpiresAt:   now.Add(settings.ttl),
	}
	if err := s.repo.InsertImageBedUpload(ctx, record); err != nil {
		// 记账失败时对象会失去 TTL 依据（成为孤儿），尽力回收后返回错误。
		if delErr := uploader.DeleteObject(ctx, storageKey); delErr != nil {
			logger.L().Warn("image_bed.orphan_object",
				zap.String("storage_key", storageKey),
				zap.Error(delErr),
			)
		}
		return nil, ErrImageBedUploadFailed.WithCause(err)
	}
	return &ImageBedUploadResult{Key: key, URL: url, ExpiresAt: record.ExpiresAt}, nil
}

// RunCleanupOnce 扫一轮过期记账行：先删对象，再删行。
// 对象删除失败或存储不可用时保留行，下一轮重试——绝不把对象悄悄漏掉。
func (s *ImageBedService) RunCleanupOnce(ctx context.Context, now time.Time) (ImageBedCleanupResult, error) {
	result := ImageBedCleanupResult{}
	if s == nil || s.repo == nil {
		return result, ErrImageBedUploadFailed
	}
	if now.IsZero() {
		now = s.now()
	}
	rows, err := s.repo.ListExpiredImageBedUploads(ctx, now, defaultImageBedCleanupBatchSize)
	if err != nil {
		return result, err
	}
	if len(rows) == 0 {
		return result, nil
	}
	uploader, ok := s.storageUploader()
	if !ok {
		logger.L().Warn("image_bed.cleanup_skipped_storage_unavailable",
			zap.Int("pending_rows", len(rows)),
		)
		result.Skipped = len(rows)
		return result, nil
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		if err := uploader.DeleteObject(ctx, row.StorageKey); err != nil {
			logger.L().Warn("image_bed.cleanup_object_delete_failed",
				zap.String("storage_key", row.StorageKey),
				zap.Error(err),
			)
			result.Failures++
			continue
		}
		if err := s.repo.DeleteImageBedUpload(ctx, row.ID); err != nil {
			logger.L().Warn("image_bed.cleanup_row_delete_failed",
				zap.Int64("upload_id", row.ID),
				zap.Error(err),
			)
			result.Failures++
			continue
		}
		result.Deleted++
	}
	return result, nil
}

// StartCleanup 启动 TTL 清理定时器（幂等：已启动则直接返回）。
// 总开关关闭时仍继续清理——已经上传的对象不该因为开关变动而失去 TTL。
func (s *ImageBedService) StartCleanup() {
	if s == nil || s.repo == nil || defaultImageBedCleanupInterval <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	done := make(chan struct{})
	s.done = done
	go func() {
		// 局部捕获：goroutine 首跑时 s.done 可能已被 Stop 置 nil（注册期竞态）。
		defer close(done)
		ticker := time.NewTicker(defaultImageBedCleanupInterval)
		defer ticker.Stop()
		for {
			// 单轮失败（例如库暂时不可用）只告警，不终止定时器；行级失败已在
			// RunCleanupOnce 内逐条留痕。ctx 已取消是正常收尾，不告警。
			if _, err := s.RunCleanupOnce(ctx, time.Now()); err != nil && ctx.Err() == nil {
				logger.L().Warn("image_bed.cleanup_sweep_failed", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop 停止清理定时器并等待当前一轮结束（可重复调用）。
func (s *ImageBedService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *ImageBedService) settings() imageBedSettings {
	out := imageBedSettings{
		enabled:     true,
		maxBytes:    defaultImageBedMaxBytes,
		hourlyLimit: defaultImageBedHourlyLimit,
		ttl:         defaultImageBedTTL,
	}
	if s == nil || s.cfg == nil {
		return out
	}
	in := s.cfg.Gateway.ImageBed
	out.enabled = in.Enabled
	if in.MaxBytes > 0 {
		out.maxBytes = in.MaxBytes
	}
	if in.HourlyLimitPerKey > 0 {
		out.hourlyLimit = in.HourlyLimitPerKey
	}
	if in.TTLHours > 0 {
		out.ttl = time.Duration(in.TTLHours) * time.Hour
	}
	return out
}

func (s *ImageBedService) storageUploader() (*ImageResultUploader, bool) {
	if s == nil || s.resolve == nil {
		return nil, false
	}
	uploader, ok := s.resolve()
	if !ok || uploader == nil {
		return nil, false
	}
	return uploader, true
}

// reserveQuota 返回当前窗口内该 key 的累计上传数（含本次）。
// Redis 计数器可用时以它为准（跨实例）；否则退化为进程内存计数。
func (s *ImageBedService) reserveQuota(ctx context.Context, apiKeyID int64) int64 {
	if s.counter == nil {
		s.reportQuotaDegrade(errImageBedQuotaCounterMissing)
		return s.incrMemoryQuota(apiKeyID)
	}
	count, err := s.counter.IncrImageBedQuota(ctx, apiKeyID, imageBedQuotaWindow)
	if err != nil {
		s.reportQuotaDegrade(err)
		return s.incrMemoryQuota(apiKeyID)
	}
	return count
}

// reportQuotaDegrade 只在首次降级时告警：Redis 抖动不该刷日志。
func (s *ImageBedService) reportQuotaDegrade(err error) {
	s.quotaDegraded.Do(func() {
		logger.L().Warn("image_bed.quota_counter_unavailable; degrading to process memory quota", zap.Error(err))
	})
}

// incrMemoryQuota 是配额的内存兜底：每个 key 一个固定窗口，
// 窗口自首次上传起算，过期后重新计数（与 Redis 实现的窗口语义一致）。
func (s *ImageBedService) incrMemoryQuota(apiKeyID int64) int64 {
	now := s.now().UTC()
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.memoryQuota == nil {
		s.memoryQuota = make(map[int64]imageBedQuotaEntry)
	}
	entry, ok := s.memoryQuota[apiKeyID]
	if !ok || !now.Before(entry.resetAt) {
		if len(s.memoryQuota) >= imageBedMemoryQuotaMaxEntries {
			for id, stale := range s.memoryQuota {
				if !now.Before(stale.resetAt) {
					delete(s.memoryQuota, id)
				}
			}
		}
		entry = imageBedQuotaEntry{resetAt: now.Add(imageBedQuotaWindow)}
	}
	entry.count++
	s.memoryQuota[apiKeyID] = entry
	return entry.count
}

func (s *ImageBedService) resolveOwner(ctx context.Context, apiKeyID int64) (int64, error) {
	if s.owners == nil {
		return 0, ErrImageBedUploadFailed.WithCause(errors.New("image bed owner resolver is not configured"))
	}
	userID, err := s.owners.ResolveImageBedOwner(ctx, apiKeyID)
	if err != nil {
		return 0, fmt.Errorf("resolve image bed owner for api key %d: %w", apiKeyID, err)
	}
	if userID <= 0 {
		return 0, ErrImageBedUploadFailed.WithCause(fmt.Errorf("image bed owner for api key %d is unknown", apiKeyID))
	}
	return userID, nil
}

// imageBedImageType 用魔数判定真实图片类型（仅 png/jpeg/webp/gif）。
//
// declared 是客户端声明的 MIME，只用来拒绝「明确声明了不支持的图片类型」
// （image/svg+xml、image/bmp 等）；application/octet-stream、空值这类非图片声明
// 不作判据——curl/Python 按扩展名猜 MIME 猜错不该误杀合法图片，真实类型始终以魔数为准。
// 于是「text 字节 + image/png 声明」仍会被拒绝。
func imageBedImageType(data []byte, declared string) (string, error) {
	declared = imageBedNormalizeMediaType(declared)
	if strings.HasPrefix(declared, "image/") && !isImageBedMediaType(declared) {
		return "", ErrImageBedUnsupportedType
	}
	detected := imageBedNormalizeMediaType(http.DetectContentType(data))
	if !isImageBedMediaType(detected) {
		return "", ErrImageBedUnsupportedType
	}
	return detected, nil
}

func imageBedNormalizeMediaType(value string) string {
	if idx := strings.IndexByte(value, ';'); idx >= 0 {
		value = value[:idx]
	}
	value = strings.ToLower(strings.TrimSpace(value))
	// image/jpg 是非标准写法，与 jpg 扩展名一并接受。
	if value == "image/jpg" {
		return "image/jpeg"
	}
	return value
}

func isImageBedMediaType(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// newImageBedKey 生成不透明文件名：32 位十六进制 + 扩展名。
func newImageBedKey(imageType string) (string, error) {
	var buf [imageBedIDBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]) + extensionForContentType(imageType), nil
}
