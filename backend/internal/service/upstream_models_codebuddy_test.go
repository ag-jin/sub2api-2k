package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CodeBuddy 上游 copilot.tencent.com 不提供模型列表端点（/v1/models、
// /v2/models、/models 实测均 404），"同步上游模型"探测必须走平台静态清单，
// 不发任何 HTTP。deepseek-v4.1-flash 为上游 auto 实测解析出的模型。

func codeBuddyModelSyncTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://copilot.tencent.com"},
	}
}

// Scenario: 探测直接返回静态清单且零 HTTP 请求。
func TestFetchUpstreamSupportedModelsCodeBuddyStatic(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{err: errors.New("codebuddy probe must not issue HTTP")}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          upstreamModelSyncTestConfig(),
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), codeBuddyModelSyncTestAccount(81))
	require.NoError(t, err)
	require.Contains(t, models, "deepseek-v4.1-flash")
	require.Contains(t, models, CodeBuddyAutoModel)
	require.Empty(t, upstream.requests, "codebuddy 探测不得发出任何 HTTP 请求")
}

// Scenario: 探测结果不含非平台伪模型。
func TestFetchUpstreamSupportedModelsCodeBuddyNoFakeModels(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{err: errors.New("codebuddy probe must not issue HTTP")}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          upstreamModelSyncTestConfig(),
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), codeBuddyModelSyncTestAccount(83))
	require.NoError(t, err)
	for _, m := range models {
		assert.NotContains(t, m, "gpt-", "不得混入 OpenAI 伪模型")
	}
}

// Scenario: 静态清单严格等于 15+auto 冻结契约（auto 置顶 + 14 个实测模型）。
func TestCodeBuddyStaticModelsExact15FrozenList(t *testing.T) {
	t.Parallel()

	want := []string{
		"auto",
		"deepseek-v4-flash",
		"deepseek-v4-pro",
		"deepseek-v4.1-flash",
		"glm-5.1",
		"glm-5.2",
		"glm-5v-turbo",
		"hy3",
		"hy3-preview",
		"hy3-preview-agent",
		"kimi-k2.5",
		"kimi-k2.6",
		"kimi-k2.7",
		"minimax-m3",
		"minimax-m3-pay",
	}
	require.Equal(t, want, CodeBuddyStaticModelIDs(),
		"15+auto 冻结契约清单（auto 置顶；与前端 useModelWhitelist 成对同步）")
}
