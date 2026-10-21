//go:build unit

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestFilterSchedulerExtra_ProjectsStream503AutoContinue 钉死 503 自动续跑开关的
// 调度快照投影：缓存命中时转发层读的是 buildSchedulerMetadataAccount 产出的
// extra，若白名单裁掉该键，管理端开启的账号在热路径会恒读到 false。
// 哨兵键用于证明走的是白名单而非全量拷贝。
func TestFilterSchedulerExtra_ProjectsStream503AutoContinue(t *testing.T) {
	const sentinelKey = "openai_stream_503_auto_continue_sentinel"

	account := service.Account{
		ID:       503,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			service.OpenAIStream503AutoContinueExtraKey: true,
			sentinelKey: "secret",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.Equal(t, true, metadata.Extra[service.OpenAIStream503AutoContinueExtraKey])
	require.NotContains(t, metadata.Extra, sentinelKey)
	require.True(t, metadata.IsOpenAIStream503AutoContinueEnabled())
}
