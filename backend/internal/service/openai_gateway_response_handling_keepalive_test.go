//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// T3: keepalive 生效间隔的三态。
// 续跑 attempt 在全局 keepalive 关闭时兜底 5s；首段保持 0（不新增 keepalive）。
func TestOpenAIStreamKeepaliveInterval(t *testing.T) {
	require.Equal(t, 5*time.Second, openAIStreamKeepaliveInterval(0, true),
		"续跑 attempt + 全局关闭 → 5s 兜底")
	require.Equal(t, time.Duration(0), openAIStreamKeepaliveInterval(0, false),
		"首段 + 全局关闭 → 保持关闭")
	require.Equal(t, 30*time.Second, openAIStreamKeepaliveInterval(30*time.Second, true),
		"配置非 0 时续跑沿用配置值")
	require.Equal(t, 30*time.Second, openAIStreamKeepaliveInterval(30*time.Second, false),
		"配置非 0 时首段沿用配置值")
}
