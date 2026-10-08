package repository

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// S3ImageStorage 用 S3 兼容对象存储实现 service.ImageStorage。
type S3ImageStorage struct {
	client        *s3.Client
	bucket        string
	publicBaseURL string
	presignExpiry time.Duration
}

var _ service.ImageStorage = (*S3ImageStorage)(nil)

// S3 实现同时具备按 key 删除对象的能力（图床 TTL 清理依赖它）。
var _ service.ImageObjectDeleter = (*S3ImageStorage)(nil)

// NewS3ImageStorage 依据配置构造 S3 图片存储（调用方应先确认 cfg.Active()）。
func NewS3ImageStorage(ctx context.Context, cfg *config.ImageStorageConfig) (*S3ImageStorage, error) {
	client, err := newS3Client(ctx, s3ClientParams{
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		ForcePathStyle:  cfg.ForcePathStyle,
	})
	if err != nil {
		return nil, err
	}

	expiry := time.Duration(cfg.PresignExpiry) * time.Hour
	if expiry <= 0 {
		expiry = 24 * time.Hour
	}

	return &S3ImageStorage{
		client:        client,
		bucket:        cfg.Bucket,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
		presignExpiry: expiry,
	}, nil
}

// Save 上传图片字节，返回可访问 URL：配了 public_base_url 则返回公开直链，否则返回 presigned 临时链接。
func (s *S3ImageStorage) Save(ctx context.Context, key, contentType string, data []byte) (string, error) {
	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	finish()
	if err != nil {
		return "", fmt.Errorf("S3 PutObject: %w", err)
	}

	if s.publicBaseURL != "" {
		return s.publicBaseURL + "/" + strings.TrimLeft(key, "/"), nil
	}

	presignClient := s3.NewPresignClient(s.client)
	result, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(s.presignExpiry))
	if err != nil {
		return "", fmt.Errorf("presign url: %w", err)
	}
	return result.URL, nil
}

// Delete 删除 key 对应的对象（图床 TTL 清理用）。
//
// S3 的 DeleteObject 本身是幂等的：对象不存在也返回成功，因此重复清理同一 key
// 不会报错——这正是「先删对象、再删记账行」所需的重试语义。
func (s *S3ImageStorage) Delete(ctx context.Context, key string) error {
	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	finish()
	if err != nil {
		return fmt.Errorf("S3 DeleteObject: %w", err)
	}
	return nil
}
