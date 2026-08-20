//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// oldUpdateCacheKey is the shared key used by upstream/Wei-Shaw binaries; this
// fork must never read or write update info under it.
const oldUpdateCacheKey = "update:latest"

func newUpdateCacheTestCache(t *testing.T) (*updateCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewUpdateCache(rdb).(*updateCache), mr
}

func TestUpdateCacheKey_IsRepositorySpecific(t *testing.T) {
	require.Equal(t, "update:latest:ag-jin/sub2api-2k", updateCacheKey,
		"update cache key must be scoped to the ag-jin/sub2api-2k fork")
	require.NotEqual(t, oldUpdateCacheKey, updateCacheKey,
		"update cache key must not collide with the upstream Wei-Shaw key")
}

func TestUpdateCache_SetWritesNewKeyOnly(t *testing.T) {
	cache, mr := newUpdateCacheTestCache(t)
	ctx := context.Background()

	require.NoError(t, cache.SetUpdateInfo(ctx, "v1.2.3", 5*time.Minute))

	got, err := mr.Get(updateCacheKey)
	require.NoError(t, err, "new repository-specific key must be written")
	require.Equal(t, "v1.2.3", got)
	require.False(t, mr.Exists(oldUpdateCacheKey), "old shared key must not be written")
}

func TestUpdateCache_GetIgnoresStaleOldKey(t *testing.T) {
	cache, mr := newUpdateCacheTestCache(t)
	ctx := context.Background()

	// Simulate a stale value left behind by an old/upstream binary.
	mr.Set(oldUpdateCacheKey, "stale-from-upstream")
	require.NoError(t, cache.SetUpdateInfo(ctx, "v2.0.0", 5*time.Minute))

	info, err := cache.GetUpdateInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "v2.0.0", info, "must not read the stale value under the old key")

	stale, err := mr.Get(oldUpdateCacheKey)
	require.NoError(t, err, "old key should be untouched by the new cache")
	require.Equal(t, "stale-from-upstream", stale)
}
