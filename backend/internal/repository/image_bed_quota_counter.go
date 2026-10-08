package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// imageBedQuotaKeyPrefix 是图床配额计数键前缀：image_bed:quota:{api_key_id}（票 #36）。
const imageBedQuotaKeyPrefix = "image_bed:quota:"

// imageBedQuotaCounter 用 Redis 计数每 API key 的图床小时配额。
//
// 窗口是「首次上传起算的固定一小时」：SETNX 只在键不存在时写入初值并落 TTL，
// 之后的 INCR 不再续期——若每次上传都 EXPIRE，持续调用会让计数永不归零。
// SETNX + INCR 放在同一个 MULTI/EXEC 里，避免并发首传时丢失 TTL。
type imageBedQuotaCounter struct {
	rdb *redis.Client
}

var _ service.ImageBedQuotaCounter = (*imageBedQuotaCounter)(nil)

func NewImageBedQuotaCounter(rdb *redis.Client) service.ImageBedQuotaCounter {
	return &imageBedQuotaCounter{rdb: rdb}
}

func (c *imageBedQuotaCounter) IncrImageBedQuota(ctx context.Context, apiKeyID int64, window time.Duration) (int64, error) {
	if c == nil || c.rdb == nil {
		return 0, errors.New("nil image bed quota counter")
	}
	if window <= 0 {
		window = time.Hour
	}
	key := imageBedQuotaKeyPrefix + strconv.FormatInt(apiKeyID, 10)
	pipe := c.rdb.TxPipeline()
	pipe.SetNX(ctx, key, 0, window)
	incr := pipe.Incr(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("image bed quota increment: %w", err)
	}
	return incr.Val(), nil
}
