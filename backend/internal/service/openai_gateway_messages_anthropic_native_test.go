//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 国产供应商原生 Anthropic 直通路径（api_protocol=anthropic）的 reasoning_effort 记录。
// 回归背景：该路径此前从不提取 Claude 协议的 output_config.effort，也不做
// thinking-enabled 兜底，导致 kimi/zhipu/deepseek 平台分组的 /v1/messages 请求
// usage_log.reasoning_effort 恒为 NULL。

func nativeAnthropicTestAccount() *Account {
	return &Account{
		ID:          702,
		Name:        "kimi-native",
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": APIProtocolAnthropic,
			"api_base_urls": map[string]any{
				APIProtocolAnthropic: "http://anthropic.example",
			},
		},
	}
}

func nativeAnthropicGLMTestAccount() *Account {
	account := nativeAnthropicTestAccount()
	account.Name = "zhipu-native"
	account.Platform = PlatformZhipu
	return account
}

func nativeAnthropicBufferedResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"msg_1","type":"message","role":"assistant","model":"k3",` +
				`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":93,"output_tokens":16}}`,
		)),
	}
}

func nativeAnthropicStreamResponse() *http.Response {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"k3","content":[],"stop_reason":null,"usage":{"input_tokens":93,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":16}}

event: message_stop
data: {"type":"message_stop"}

`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func TestNativeAnthropicPassthroughRecordsOutputConfigEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"k3","max_tokens":32,"stream":false,` +
		`"output_config":{"effort":"low"},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: nativeAnthropicBufferedResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), nativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "low", *result.ReasoningEffort)
}

func TestNativeAnthropicPassthroughThinkingEnabledFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 未显式传 effort，但 thinking 已启用：k3 属于 passback-required 白名单，应兜底记为 high。
	body := []byte(`{"model":"k3","max_tokens":32,"stream":false,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: nativeAnthropicBufferedResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), nativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "high", *result.ReasoningEffort)
}

func TestNativeAnthropicPassthroughStreamRecordsEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"k3","max_tokens":32,"stream":true,` +
		`"output_config":{"effort":"max"},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: nativeAnthropicStreamResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), nativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "max", *result.ReasoningEffort)
}

func TestNativeAnthropicPassthroughNoEffortStaysNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 既无 output_config.effort 也未启用 thinking：保持 nil，不做语义注入。
	body := []byte(`{"model":"k3","max_tokens":32,"stream":false,` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: nativeAnthropicBufferedResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), nativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.ReasoningEffort)
}

func TestNativeAnthropicPassthroughNormalizesGLM53Thinking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		stream     bool
		preference string
		wantEffort string
	}{
		{name: "disabled buffered", preference: `"thinking":{"type":"disabled"},`, wantEffort: "low"},
		{name: "off buffered", preference: `"thinking":{"type":"off"},`, wantEffort: "low"},
		{name: "none buffered", preference: `"thinking":{"type":"none"},`, wantEffort: "low"},
		{name: "enabled buffered", preference: `"thinking":{"type":"enabled"},`, wantEffort: "high"},
		{name: "enabled streaming", stream: true, preference: `"thinking":{"type":"enabled"},`, wantEffort: "high"},
		{name: "adaptive buffered", preference: `"thinking":{"type":"adaptive"},`, wantEffort: "high"},
		{name: "adaptive streaming", stream: true, preference: `"thinking":{"type":"adaptive"},`, wantEffort: "high"},
		{name: "minimal buffered", preference: `"output_config":{"effort":"minimal"},`, wantEffort: "low"},
		{name: "low buffered", preference: `"output_config":{"effort":"low"},`, wantEffort: "low"},
		{name: "medium streaming", stream: true, preference: `"output_config":{"effort":"medium"},`, wantEffort: "high"},
		{name: "high buffered", preference: `"output_config":{"effort":"high"},`, wantEffort: "high"},
		{name: "xhigh buffered", preference: `"output_config":{"effort":"xhigh"},`, wantEffort: "max"},
		{name: "max buffered", preference: `"output_config":{"effort":"max"},`, wantEffort: "max"},
		{name: "ultra buffered", preference: `"output_config":{"effort":"ultra"},`, wantEffort: "max"},
		{name: "output effort wins over thinking", preference: `"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},`, wantEffort: "low"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"glm-5.3","max_tokens":32,"stream":%t,%s"messages":[{"role":"user","content":"hi"}]}`, tt.stream, tt.preference))
			response := nativeAnthropicBufferedResponse()
			if tt.stream {
				response = nativeAnthropicStreamResponse()
			}
			upstream := &httpUpstreamRecorder{resp: response}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

			_, err := svc.ForwardAsAnthropic(context.Background(),
				adaptiveProtocolTestContext("/v1/messages", body), nativeAnthropicGLMTestAccount(), body, "", "")
			require.NoError(t, err)
			require.Equal(t, "enabled", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
			require.Equal(t, tt.wantEffort, gjson.GetBytes(upstream.lastBody, "output_config.effort").String())
		})
	}
}

func TestNativeAnthropicPassthroughLeavesOtherThinkingUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		body string
	}{
		{name: "glm 5.3 unspecified", body: `{"model":"glm-5.3","max_tokens":32,"stream":false,"messages":[]}`},
		{name: "glm 5.2 disabled", body: `{"model":"glm-5.2","max_tokens":32,"stream":false,"thinking":{"type":"disabled"},"messages":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			upstream := &httpUpstreamRecorder{resp: nativeAnthropicBufferedResponse()}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			_, err := svc.ForwardAsAnthropic(context.Background(),
				adaptiveProtocolTestContext("/v1/messages", body), nativeAnthropicGLMTestAccount(), body, "", "")
			require.NoError(t, err)
			require.JSONEq(t, tt.body, string(upstream.lastBody))
		})
	}
}

// zhipuNativeAnthropicTestAccount 构造票 #33 回归用的最小智谱账号：原生
// Anthropic 协议（升级渠道的既定配置，design M3），固定 base_url 以便断言出站端点。
func zhipuNativeAnthropicTestAccount() *Account {
	return &Account{
		ID:          703,
		Name:        "zhipu-native",
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": APIProtocolAnthropic,
			"base_url":     "http://anthropic.example",
		},
	}
}

// TestNativeAnthropicPassthroughPreservesImageBlocks 是票 #33 的回归锁：
// api_protocol=anthropic 渠道经 /v1/messages 直通时，请求体里的 base64 image 块
// 必须原样到达上游（type/source/media_type/base64 数据全保留，不得清洗或重建）。
//
// 回归背景：生产渠道走 chat_completions（api_protocol=openai），智谱
// /api/paas/v4/chat/completions 对 glm-5.3 系**静默忽略** image_url parts，
// 图片从未到达模型（识图表现为纯幻觉/input_tokens 不含图片）。升级渠道改成
// anthropic 原生直通（→ open.bigmodel.cn/api/anthropic/v1/messages）即天然保图；
// 本测试防止后续对直通 body 的清洗/重建把这条修复弄丢。
func TestNativeAnthropicPassthroughPreservesImageBlocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 1x1 PNG，测试用固定数据，无任何用户图片内容（R0）。
	const imageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	contentBlocks := `[{"type":"text","text":"这张图里有什么"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + imageData + `"}}]`
	body := []byte(`{"model":"glm-5.3","max_tokens":64,"stream":false,` +
		`"messages":[{"role":"user","content":` + contentBlocks + `}]}`)

	upstream := &httpUpstreamRecorder{resp: nativeAnthropicBufferedResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), zhipuNativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "http://anthropic.example/v1/messages", upstream.lastReq.URL.String())

	sent := upstream.lastBody
	require.NotEmpty(t, sent)
	// 图片块逐字段保留，base64 数据完整（不得截断/重编码）。
	require.Equal(t, "image", gjson.GetBytes(sent, "messages.0.content.1.type").String())
	require.Equal(t, "base64", gjson.GetBytes(sent, "messages.0.content.1.source.type").String())
	require.Equal(t, "image/png", gjson.GetBytes(sent, "messages.0.content.1.source.media_type").String())
	require.Equal(t, imageData, gjson.GetBytes(sent, "messages.0.content.1.source.data").String())
	// 最强断言：整个 content 块数组与入站逐字段一致（无清洗/无重建）。
	require.JSONEq(t, contentBlocks, gjson.GetBytes(sent, "messages.0.content").Raw)
}

// TestNativeAnthropicVisionBridgeRewritesBlindModelImageBlocks 是票 #35 挂点的主用例：
// 智谱登录托管账号 + 盲模型（glm-5.3）+ 内联图片 → 站点先用同账号 flash 识别，把图片块
// 原地替换为 "[图片 1 内容] <描述>" 后再下发主请求（主请求体里不得再有 image 块）。
func TestNativeAnthropicVisionBridgeRewritesBlindModelImageBlocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	contentBlocks := `[{"type":"text","text":"这张图里是什么"},` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]`
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":` + contentBlocks + `}]`)

	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "一只红色的圆球"},
		mainModel:    "glm-5.3",
		mainResponse: nativeAnthropicBufferedResponse(),
	}
	svc := &OpenAIGatewayService{
		cfg:          zhipuVisionBridgeTestConfig(),
		httpUpstream: upstream,
		zhipuSigner:  &zhipuSignStubSigner{},
	}

	_, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), zhipuVisionBridgeTestAccount(t), body, "", "")
	require.NoError(t, err)

	require.Len(t, upstream.requests, 2, "一次 flash 识图 + 一次主请求")
	require.Equal(t, 1, upstream.posts, "flash 调用恰好一次")
	// 第 1 次出站是桥调用（模型名为桥模型、带原图）；第 2 次是主请求。
	require.Equal(t, "glm-5.3-flash", gjson.GetBytes(upstream.bodies[0], "model").String())
	require.Equal(t, zhipuVisionBridgeTestImageDataA, zhipuVisionBridgeFlashImageData(upstream.bodies[0]))
	mainBody := upstream.bodies[1]
	require.Equal(t, "glm-5.3", gjson.GetBytes(mainBody, "model").String())
	require.Zero(t, zhipuVisionBridgeImageBlockCount(mainBody), "主请求不得再残留 image 块")
	require.JSONEq(t,
		`[{"type":"text","text":"这张图里是什么"},{"type":"text","text":"[图片 1 内容] 一只红色的圆球"}]`,
		gjson.GetBytes(mainBody, "messages.0.content").Raw)
}

// TestNativeAnthropicVisionBridgeNeverBridgesTheBridgeModel 是防递归红线的挂点锁：
// 请求模型就是桥模型（flash）时不得触发桥——即使运维把 flash 误配进盲模型集，运行期
// 的互斥解析也必须把它剔除（宁可漏桥，不可「flash 被桥」）。
func TestNativeAnthropicVisionBridgeNeverBridgesTheBridgeModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	contentBlocks := `[{"type":"text","text":"这张图里是什么"},` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]`

	tests := []struct {
		name        string
		blindModels string
	}{
		{name: "默认盲集不含 flash", blindModels: "glm-5.3,glm-5.2"},
		{name: "误把 flash 配进盲集也必须互斥", blindModels: "glm-5.3,glm-5.3-flash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := zhipuVisionBridgeTestConfig()
			cfg.Gateway.Zhipu.VisionBridge.BlindModels = tt.blindModels
			body := []byte(`{"model":"glm-5.3-flash","max_tokens":64,"stream":false,"messages":[{"role":"user","content":` +
				contentBlocks + `}]}`)
			upstream := &zhipuVisionBridgeUpstreamFake{
				fallbackDescription: "不应被调用",
				mainModel:           "glm-5.3-flash",
				mainResponse:        nativeAnthropicBufferedResponse(),
			}
			svc := &OpenAIGatewayService{
				cfg:          cfg,
				httpUpstream: upstream,
				zhipuSigner:  &zhipuSignStubSigner{},
			}

			_, err := svc.ForwardAsAnthropic(context.Background(),
				adaptiveProtocolTestContext("/v1/messages", body), zhipuVisionBridgeTestAccount(t), body, "", "")
			require.NoError(t, err)

			require.Zero(t, upstream.posts, "flash 请求本身不得触发桥调用")
			require.Len(t, upstream.requests, 1)
			require.JSONEq(t, contentBlocks, gjson.GetBytes(upstream.bodies[0], "messages.0.content").Raw,
				"flash 直通保图语义不变（#33 回归口径）")
		})
	}
}

// TestNativeAnthropicVisionBridgeGateMatrix 覆盖挂点的触发矩阵：开关关闭、非盲模型、
// 非智谱账号三种情形一律直通现状——图片块逐字段原样到达上游，且没有任何 flash 调用。
func TestNativeAnthropicVisionBridgeGateMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	contentBlocks := `[{"type":"text","text":"这张图里是什么"},` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]`

	tests := []struct {
		name    string
		mutate  func(*config.Config)
		account func(*testing.T) *Account
		model   string
	}{
		{
			name:    "开关关闭 → 直通现状",
			mutate:  func(cfg *config.Config) { cfg.Gateway.Zhipu.VisionBridge.Enabled = false },
			account: zhipuVisionBridgeTestAccount,
			model:   "glm-5.3",
		},
		{
			name:    "非盲模型 → 直通",
			mutate:  func(*config.Config) {},
			account: zhipuVisionBridgeTestAccount,
			model:   "glm-4.7",
		},
		{
			name:    "非智谱账号 → 直通",
			mutate:  func(*config.Config) {},
			account: func(*testing.T) *Account { return nativeAnthropicTestAccount() },
			model:   "glm-5.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := zhipuVisionBridgeTestConfig()
			tt.mutate(cfg)
			body := []byte(`{"model":"` + tt.model + `","max_tokens":64,"stream":false,"messages":[{"role":"user","content":` +
				contentBlocks + `}]}`)
			upstream := &zhipuVisionBridgeUpstreamFake{
				fallbackDescription: "不应被调用",
				mainModel:           tt.model,
				mainResponse:        nativeAnthropicBufferedResponse(),
			}
			svc := &OpenAIGatewayService{
				cfg:          cfg,
				httpUpstream: upstream,
				zhipuSigner:  &zhipuSignStubSigner{},
			}

			_, err := svc.ForwardAsAnthropic(context.Background(),
				adaptiveProtocolTestContext("/v1/messages", body), tt.account(t), body, "", "")
			require.NoError(t, err)

			require.Zero(t, upstream.posts, "未命中桥条件时不得有任何 flash 调用")
			require.Len(t, upstream.requests, 1, "未命中桥条件时只有主请求")
			require.JSONEq(t, contentBlocks, gjson.GetBytes(upstream.bodies[0], "messages.0.content").Raw,
				"图片块必须原样到达上游（#33 保图语义）")
		})
	}
}

// TestNativeAnthropicVisionBridgeAppliesToAnyZhipuAccount 锁定触发面的账号判定（票面挂点
// 口径「zhipu 账号」）：非登录托管的 payg api_key 形态同样适用桥（桥模型与请求同账号），
// 不因账号形态而漏桥；非 zhipu 平台一律不桥（见 GateMatrix 的非智谱用例）。
func TestNativeAnthropicVisionBridgeAppliesToAnyZhipuAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	contentBlocks := `[{"type":"text","text":"这张图里是什么"},` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]`
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":` + contentBlocks + `}]`)

	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "一只红色的圆球"},
		mainModel:    "glm-5.3",
		mainResponse: nativeAnthropicBufferedResponse(),
	}
	svc := &OpenAIGatewayService{cfg: zhipuVisionBridgeTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(),
		adaptiveProtocolTestContext("/v1/messages", body), zhipuNativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)

	require.Equal(t, 1, upstream.posts)
	mainBody := upstream.bodies[1]
	require.Zero(t, zhipuVisionBridgeImageBlockCount(mainBody))
	require.Equal(t, "[图片 1 内容] 一只红色的圆球",
		gjson.GetBytes(mainBody, "messages.0.content.1.text").String())
}
