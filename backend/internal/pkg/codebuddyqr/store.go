// Package codebuddyqr 提供后台 CodeBuddy 扫码登录流程的 state 存储实现
// （Redis，go-redis v9）：记录发起者绑定、节流时间戳、并发的每发起者上限。
package codebuddyqr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// RecordTTL state 记录的 Redis TTL。略长于逻辑有效期（StateTTL），保证
	// 轮询能命中显式 expired 而不是 404 未知态；逻辑过期由服务端按
	// createdAt + StateTTL 判定。
	RecordTTL = 330 * time.Second
	// StateTTL state 逻辑有效期（参考实现口径 300s）。
	StateTTL = 300 * time.Second
	// MaxStatesPerActor 每个发起者最多并发的扫码 state 数（防滥用）。
	MaxStatesPerActor = 2
)

// StateRecord 扫码 state 的服务端记账（不含任何凭据内容）。
type StateRecord struct {
	ActorID      string `json:"actorId"`
	TokenVersion string `json:"tokenVersion,omitempty"`
	CreatedAt    int64  `json:"createdAt"`              // unix 毫秒
	LastPolledAt int64  `json:"lastPolledAt,omitempty"` // unix 毫秒
}

// ErrCapacityExceeded 发起者并发 state 数已满。
var (
	ErrCapacityExceeded = errors.New("codebuddy qr state per-actor capacity exceeded")
	ErrStoreUnavailable = errors.New("codebuddy qr state store not configured")
	ErrStateExists      = errors.New("codebuddy qr state already exists")
)

// Store 扫码状态存储接口（流程语义：他人不可见；poll 只答发起者）。
type Store interface {
	Create(ctx context.Context, state string, rec StateRecord) error
	Load(ctx context.Context, state string) (*StateRecord, error)
	Save(ctx context.Context, state string, rec StateRecord, remaining time.Duration) error
	// Drop 焚毁 state 记录与发起者活跃登记（"取到 token 当刻即焚"）。
	Drop(ctx context.Context, state string) error
	// CountActorStates 返回发起者当前活跃（未过逻辑期）state 数。
	CountActorStates(ctx context.Context, actorID string) (int, error)
}

// RedisStore Store 的 Redis 实现。state 键 `codebuddy:qr:{state}`（JSON）；
// 发起者活跃集合 `codebuddy:qr:actor:{actorId}`（ZSET，member=state，
// score=创建毫秒）。
type RedisStore struct {
	rdb *redis.Client
}

func NewRedisStore(rdb *redis.Client) *RedisStore {
	return &RedisStore{rdb: rdb}
}

func (s *RedisStore) stateKey(state string) string {
	return "codebuddy:qr:" + strings.TrimSpace(state)
}

func (s *RedisStore) actorKey(actorID string) string {
	return "codebuddy:qr:actor:" + strings.TrimSpace(actorID)
}

func marshalRecord(rec StateRecord) (string, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("codebuddy qr state marshal: %w", err)
	}
	return string(raw), nil
}

func (s *RedisStore) Create(ctx context.Context, state string, rec StateRecord) error {
	if s == nil || s.rdb == nil {
		return ErrStoreUnavailable
	}
	key := s.stateKey(state)
	raw, err := marshalRecord(rec)
	if err != nil {
		return err
	}
	// NX 写入防撞（state 为 UUID4 形态，碰撞仅理论可能）。
	if set, err := s.rdb.SetNX(ctx, key, raw, RecordTTL).Result(); err != nil {
		return fmt.Errorf("codebuddy qr state create: %w", err)
	} else if !set {
		return ErrStateExists
	}
	if _, err := s.rdb.ZAdd(ctx, s.actorKey(rec.ActorID), redis.Z{
		Score:  float64(rec.CreatedAt),
		Member: state,
	}).Result(); err != nil {
		_ = s.rdb.Del(ctx, key)
		return fmt.Errorf("codebuddy qr actor registration: %w", err)
	}
	return nil
}

func (s *RedisStore) Load(ctx context.Context, state string) (*StateRecord, error) {
	if s == nil || s.rdb == nil {
		return nil, ErrStoreUnavailable
	}
	raw, err := s.rdb.Get(ctx, s.stateKey(state)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codebuddy qr state load: %w", err)
	}
	var rec StateRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		_ = s.rdb.Del(ctx, s.stateKey(state))
		return nil, fmt.Errorf("codebuddy qr state decode: %w", err)
	}
	return &rec, nil
}

func (s *RedisStore) Save(ctx context.Context, state string, rec StateRecord, remaining time.Duration) error {
	if s == nil || s.rdb == nil {
		return ErrStoreUnavailable
	}
	raw, err := marshalRecord(rec)
	if err != nil {
		return err
	}
	if err := s.rdb.Set(ctx, s.stateKey(state), raw, remaining).Err(); err != nil {
		return fmt.Errorf("codebuddy qr state save: %w", err)
	}
	return nil
}

// Drop 焚毁 state 记录并从发起者活跃集合移除。不校验发起者一致性
// （服务端 state 天然无鉴权属性）；调用方须已完成绑定检查。
func (s *RedisStore) Drop(ctx context.Context, state string) error {
	return s.DropWithActor(ctx, state, "")
}

// DropWithActor 焚毁 state 并从指定发起者集合移除（actorID 为空则跳过集合清理）。
func (s *RedisStore) DropWithActor(ctx context.Context, state string, actorID string) error {
	if s == nil || s.rdb == nil {
		return ErrStoreUnavailable
	}
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, s.stateKey(state))
	if actorID != "" {
		pipe.ZRem(ctx, s.actorKey(actorID), state)
	}
	_, err := pipe.Exec(ctx)
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("codebuddy qr state drop: %w", err)
	}
	return nil
}

// CountActorStates 统计发起者活跃 state 数（顺带清理已过逻辑期的登记）。
func (s *RedisStore) CountActorStates(ctx context.Context, actorID string) (int, error) {
	if s == nil || s.rdb == nil {
		return 0, ErrStoreUnavailable
	}
	key := s.actorKey(actorID)
	// 清理超过逻辑期的成员（score=创建毫秒）。
	if _, err := s.rdb.ZRemRangeByScore(ctx, key, "-inf",
		fmt.Sprintf("(%d", time.Now().Add(-StateTTL).UnixMilli())).Result(); err != nil {
		return 0, fmt.Errorf("codebuddy qr actor prune: %w", err)
	}
	count, err := s.rdb.ZCard(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("codebuddy qr actor count: %w", err)
	}
	return int(count), nil
}
