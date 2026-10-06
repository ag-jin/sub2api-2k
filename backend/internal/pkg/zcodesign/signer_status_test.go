//go:build unit

package zcodesign

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 票 28 的管理面状态接口按 apiKeyID 读取握手私钥缓存状态（design M6「渠道签名 V4
// 配置 UI」：缓存命中 / 上次握手时间 / 连续失败计数）。从未在本进程握手过的账号
// 必须显示「未缓存」而不是 0 值伪装成的已缓存。
func TestSignerKeyStatusForANeverHandshakedKey(t *testing.T) {
	t.Parallel()

	signer := NewSigner(testOrigin, "", time.Hour, newStubHandshake(t))

	status := signer.KeyStatus(goldenAPIKeyID)

	assert.False(t, status.Cached)
	assert.Zero(t, status.ConsecutiveFailures)
	assert.True(t, status.LastHandshakeAt.IsZero(), "从未握手时必须为零值，前端渲染为“未缓存”")
	assert.True(t, status.ExpiresAt.IsZero(), "没有缓存私钥就没有过期时间")
}

// 一次成功握手后：命中缓存、上次握手时间 = 握手时刻、过期时间 = 握手时刻 + TTL；
// 过了 TTL 只是不再命中缓存，历史握手时间保持可读（UI 用它显示陈旧程度）。
func TestSignerKeyStatusTracksTheCachedKeyLifecycle(t *testing.T) {
	t.Parallel()

	stub := newStubHandshake(t)
	clock := newFakeClock()
	signer := NewSigner(testOrigin, "", time.Hour, stub)
	signer.now = clock.Now

	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))

	status := signer.KeyStatus(goldenAPIKeyID)
	assert.True(t, status.Cached, "成功握手后私钥在 TTL 内必须命中缓存")
	assert.Equal(t, clock.Now(), status.LastHandshakeAt)
	assert.Equal(t, clock.Now().Add(time.Hour), status.ExpiresAt)
	assert.Zero(t, status.ConsecutiveFailures)

	clock.Advance(2 * time.Hour)

	expired := signer.KeyStatus(goldenAPIKeyID)
	assert.False(t, expired.Cached, "过期私钥不算命中缓存")
	assert.Equal(t, clock.Now().Add(-2*time.Hour), expired.LastHandshakeAt,
		"过期后仍保留上次成功握手时间")
	assert.Zero(t, expired.ConsecutiveFailures)
}

// 连续失败计数按 apiKeyID 独立累计，并在下一次成功握手时归零（账号级熔断 #24 的输入）。
// 被退避窗口挡下的请求不算一次新的握手失败。
func TestSignerKeyStatusCountsConsecutiveHandshakeFailures(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	stub := newStubHandshake(t)
	stub.failWithKey(errors.New("handshake endpoint down"))
	signer := NewSigner(testOrigin, "", time.Hour, stub)
	signer.now = clock.Now

	require.Error(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 1, signer.KeyStatus(goldenAPIKeyID).ConsecutiveFailures)
	assert.False(t, signer.KeyStatus(goldenAPIKeyID).Cached)

	require.ErrorIs(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}), ErrHandshakeBackoff)
	assert.Equal(t, 1, signer.KeyStatus(goldenAPIKeyID).ConsecutiveFailures,
		"退避窗口内的请求没有发起握手，不得计入连续失败")

	clock.Advance(DefaultHandshakeBackoff + time.Second)
	require.Error(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))
	assert.Equal(t, 2, signer.KeyStatus(goldenAPIKeyID).ConsecutiveFailures)

	// 上游恢复：成功握手把连续失败计数清零并刷新上次握手时间。
	clock.Advance(DefaultHandshakeBackoff + time.Second)
	stub.mu.Lock()
	stub.failFor = nil
	stub.mu.Unlock()
	require.NoError(t, signer.Sign(context.Background(), goldenAPIKey, goldenSessionID, http.Header{}))

	recovered := signer.KeyStatus(goldenAPIKeyID)
	assert.True(t, recovered.Cached)
	assert.Zero(t, recovered.ConsecutiveFailures)
	assert.Equal(t, clock.Now(), recovered.LastHandshakeAt)
}
