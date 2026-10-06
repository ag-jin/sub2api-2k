package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 智谱签名 L1 桶计数器的 Redis 实现（design M3.1(b) / 票 24）。
//
// 计数器的产生点与服务端读取语义都在 service 包（zhipu_sign_alert.go，含键规范与
// 内存退化），这里只提供存储：service 包不得直接依赖 redis（depguard），故按
// internal500_counter_cache.go 的既有范式把 Redis 实现放在 repository 并由 wire 装配。

// zhipuSignCounterIncrScript 原子递增并只在键首次创建时设置 TTL：桶键的过期由 TTL
// 兜底，不需要任何清理任务，也不会因为并发首次写入而丢掉 TTL。
var zhipuSignCounterIncrScript = redis.NewScript(`
	local key = KEYS[1]
	local ttl = tonumber(ARGV[1])

	local count = redis.call('INCR', key)
	if count == 1 then
		redis.call('PEXPIRE', key, ttl)
	end

	return count
`)

// zhipuSignCounterOpTimeout 是单次计数读写的上限：签名热路径上的计数只是旁路指标，
// 超过一个常规 Redis 往返时延就按「不可用」处理（退化到进程内存并 warnOnce），
// 绝不让 Redis 抖动拖慢数据面请求。
const zhipuSignCounterOpTimeout = 200 * time.Millisecond

type zhipuSignCounterCache struct {
	rdb *redis.Client
}

// NewZhipuSignCounterCache 构造智谱签名 L1 桶计数器。rdb 为 nil（未配置 Redis）时
// 返回 nil 接口 —— 调用方（service.ZhipuSignAlerts）据此退化为进程内存并只告警一次。
func NewZhipuSignCounterCache(rdb *redis.Client) service.ZhipuSignCounterCache {
	if rdb == nil {
		return nil
	}
	return &zhipuSignCounterCache{rdb: rdb}
}

// IncrZhipuSignCounter 原子递增一个桶键，返回递增后的值。
func (c *zhipuSignCounterCache) IncrZhipuSignCounter(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	opCtx, cancel := zhipuSignCounterContext(ctx)
	defer cancel()

	result, err := zhipuSignCounterIncrScript.Run(opCtx, c.rdb, []string{key}, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("increment zhipu sign counter %s: %w", key, err)
	}
	return result, nil
}

// GetZhipuSignCounter 读取一个桶键；键不存在（Redis 的 nil 回复）是正常状态，返回 0。
func (c *zhipuSignCounterCache) GetZhipuSignCounter(ctx context.Context, key string) (int64, error) {
	opCtx, cancel := zhipuSignCounterContext(ctx)
	defer cancel()

	value, err := c.rdb.Get(opCtx, key).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read zhipu sign counter %s: %w", key, err)
	}
	return value, nil
}

// zhipuSignCounterContext 给计数操作套上短超时；上游请求 context 若已取消则立即返回，
// 计数被跳过并触发退化告警（可接受：指标是旁路，请求本身不受影响）。
func zhipuSignCounterContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, zhipuSignCounterOpTimeout)
}
