//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 2026-09-17 生产实测：opencode 账号"正常调用 200、管理面点测试 400
// MissingSessionID"——测试路径没有注入 x-opencode-session（真实转发路径有）。
// 该用例锁死"管理面测试与真实转发同源注入"这条约束。
func TestAccountTestService_OpenCodeConnectionCarriesSessionHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          460,
		Name:        "opencode-go",
		Platform:    PlatformOpenCode,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-opencode-test",
			"base_url": "https://opencode-upstream.example/v1",
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "deepseek-v4.1-flash", "hello", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)

	session := upstream.lastReq.Header.Get(openCodeSessionHeader)
	require.NotEmpty(t, session, "缺 x-opencode-session 时 opencode GO 一律 400 MissingSessionID")
	require.Len(t, session, 36, "会话头应为 UUID 形状")
	require.Equal(t, openCodeUpstreamUserAgent, upstream.lastReq.Header.Get("user-agent"),
		"opencode GO 要求自有 agent UA，测试路径同样不能裸奔 Go-http-client")
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

func TestApplyOpenCodeUpstreamHeaders_ScopeAndPrecedence(t *testing.T) {
	conversation := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`)

	// 非 opencode 账号：一律不动（同一构造代码被多平台共用）。
	nonOpenCode := http.Header{}
	applyOpenCodeUpstreamHeaders(nonOpenCode, &Account{ID: 1, Platform: PlatformOpenAI}, 7, conversation, "")
	require.Empty(t, nonOpenCode.Get(openCodeSessionHeader))
	require.Empty(t, nonOpenCode.Get("user-agent"))

	// 空账号 / 空 header：不 panic、不写入。
	nilSafe := http.Header{}
	applyOpenCodeUpstreamHeaders(nilSafe, nil, 0, conversation, "")
	applyOpenCodeUpstreamHeaders(nil, &Account{ID: 2, Platform: PlatformOpenCode}, 0, conversation, "")
	require.Empty(t, nilSafe.Get(openCodeSessionHeader))

	// opencode + 调用方自定义 UA：会话头注入，UA 以调用方为准。
	customUA := http.Header{}
	applyOpenCodeUpstreamHeaders(customUA, &Account{ID: 3, Platform: PlatformOpenCode}, 0, conversation, "my-coding-agent/1.0")
	require.NotEmpty(t, customUA.Get(openCodeSessionHeader))
	require.Empty(t, customUA.Get("user-agent"))

	// opencode 无自定义 UA：兜底自有 UA。
	fallbackUA := http.Header{}
	applyOpenCodeUpstreamHeaders(fallbackUA, &Account{ID: 4, Platform: PlatformOpenCode}, 0, conversation, "")
	require.Equal(t, openCodeUpstreamUserAgent, fallbackUA.Get("user-agent"))
	require.Len(t, fallbackUA.Get(openCodeSessionHeader), 36)
}

func TestOpenCodeSessionHeaderValue_StableWithinConversation(t *testing.T) {
	turn1 := []byte(`{"messages":[{"role":"user","content":"same first message"}]}`)
	turn2 := []byte(`{"messages":[{"role":"user","content":"same first message"},{"role":"assistant","content":"reply"},{"role":"user","content":"follow up"}]}`)
	other := []byte(`{"messages":[{"role":"user","content":"different first message"}]}`)

	require.Equal(t,
		openCodeSessionHeaderValue(11, 0, turn1),
		openCodeSessionHeaderValue(11, 0, turn2),
		"同一对话跨轮（首条 user 不变）会话头必须稳定",
	)
	require.NotEqual(t,
		openCodeSessionHeaderValue(11, 0, turn1),
		openCodeSessionHeaderValue(11, 0, other),
		"不同对话应派生不同会话头",
	)
	require.NotEqual(t,
		openCodeSessionHeaderValue(11, 0, turn1),
		openCodeSessionHeaderValue(12, 0, turn1),
		"不同账号应派生不同会话头",
	)
}
