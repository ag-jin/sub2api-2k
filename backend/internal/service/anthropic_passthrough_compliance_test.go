//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 票 #38 C1/C2：Anthropic 原生透传路径的响应侧合规。
//
// C1：上游未给 Content-Type 时，默认值必须跟随客户端请求的流式意图
// （流式 text/event-stream / 非流式 application/json）。此前两处透传辅助函数
// 无条件默认 text/event-stream，非流式客户端会拿到 SSE 的 Content-Type。
// C2 守卫：原生透传是字节级中继，合成路径的 id 规范化（msg_/toolu_）不得
// 泄漏到这里——上游 id 必须原样保留。

const anthropicPassthroughUpstreamMessageID = "021791444165008fd3ae808d5ba084b55a7f9e1600032cd66e910"

// anthropicPassthroughSSEStream 返回与原生直通测试同构的最小 SSE 流
// （message_start → 文本块 → message_delta → message_stop）。
func anthropicPassthroughSSEStream() string {
	return strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"` + anthropicPassthroughUpstreamMessageID + `","type":"message","role":"assistant","model":"k3","content":[],"stop_reason":null,"usage":{"input_tokens":93,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":16}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
}

func anthropicPassthroughUpstreamResponse(contentType string, body string) *http.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestAnthropicPassthroughContentTypeDefault 覆盖两处透传辅助函数的 CT 兜底：
// 上游显式值优先；上游缺省时按客户端请求的流式意图取 SSE / JSON。
func TestAnthropicPassthroughContentTypeDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		upstreamCT string
		stream     bool
		want       string
	}{
		{name: "非流式请求默认 application/json", upstreamCT: "", stream: false, want: "application/json"},
		{name: "流式请求默认 text/event-stream", upstreamCT: "", stream: true, want: "text/event-stream"},
		{name: "上游显式 CT 优先（非流式也不改写）", upstreamCT: "text/event-stream", stream: false, want: "text/event-stream"},
		{name: "上游显式 JSON 优先", upstreamCT: "application/json", stream: true, want: "application/json"},
	}

	t.Run("gatewayService/anthropic APIKey passthrough", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

				svc := &GatewayService{
					cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
					rateLimitService: &RateLimitService{},
				}
				resp := anthropicPassthroughUpstreamResponse(tt.upstreamCT, anthropicPassthroughSSEStream())

				_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c,
					&Account{ID: 1}, time.Now(), "claude-3-7-sonnet-20250219", tt.stream)
				require.NoError(t, err)
				require.Equal(t, tt.want, rec.Header().Get("Content-Type"))
			})
		}
	})

	t.Run("openaiGatewayService/native anthropic endpoint", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
				resp := anthropicPassthroughUpstreamResponse(tt.upstreamCT, anthropicPassthroughSSEStream())

				_, err := svc.handleNativeAnthropicStreamingResponse(context.Background(), resp, c,
					nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(), tt.stream)
				require.NoError(t, err)
				require.Equal(t, tt.want, rec.Header().Get("Content-Type"))
			})
		}
	})
}

// TestNativeAnthropicPassthroughKeepsUpstreamResponseID 是 C2 守卫：原生透传
// （非流式原样回写 / 流式逐行中继）必须保留上游 id，不得套用合成路径的
// msg_ 规范化。
func TestNativeAnthropicPassthroughKeepsUpstreamResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("non-stream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		body := `{"id":"` + anthropicPassthroughUpstreamMessageID + `","type":"message","role":"assistant","model":"k3",` +
			`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":93,"output_tokens":16}}`
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}

		_, err := svc.handleNativeAnthropicBufferedResponse(context.Background(),
			anthropicPassthroughUpstreamResponse("application/json", body), c,
			nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now())
		require.NoError(t, err)
		require.Equal(t, anthropicPassthroughUpstreamMessageID, gjson.Get(rec.Body.String(), "id").String(),
			"原生透传不得改写上游 message id")
	})

	t.Run("stream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}

		_, err := svc.handleNativeAnthropicStreamingResponse(context.Background(),
			anthropicPassthroughUpstreamResponse("text/event-stream", anthropicPassthroughSSEStream()), c,
			nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(), true)
		require.NoError(t, err)
		require.Contains(t, rec.Body.String(), `"id":"`+anthropicPassthroughUpstreamMessageID+`"`,
			"原生透传不得改写上游 message id")
		require.NotContains(t, rec.Body.String(), `"id":"msg_`,
			"合成路径的 msg_ 规范化不得泄漏到原生透传")
	})
}
