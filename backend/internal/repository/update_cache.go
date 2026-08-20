package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Repository-specific key so forks (ag-jin/sub2api-2k) never read update info
// cached by upstream binaries under the shared "update:latest" key.
const updateCacheKey = "update:latest:ag-jin/sub2api-2k"

type updateCache struct {
	rdb *redis.Client
}

func NewUpdateCache(rdb *redis.Client) service.UpdateCache {
	return &updateCache{rdb: rdb}
}

func (c *updateCache) GetUpdateInfo(ctx context.Context) (string, error) {
	return c.rdb.Get(ctx, updateCacheKey).Result()
}

func (c *updateCache) SetUpdateInfo(ctx context.Context, data string, ttl time.Duration) error {
	return c.rdb.Set(ctx, updateCacheKey, data, ttl).Err()
}
