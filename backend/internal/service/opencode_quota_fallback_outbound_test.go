//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 本文件只调用 216 已存在的转发入口（ForwardAsChatCompletions /
// forwardResponsesViaRawChatCompletions / ForwardAsAnthropic 与
// EvaluateOpenCodeQuotaFallback→ResolveRequest），因此同一份测试可复制回
// literal 216 源码复现红灯：出站 body 仍是 normal 模型 / unknown 直连被拒。
//
// 断言的是真实 httptest 上游收到的 path 与 body.model，而不是 helper 之间的
// 同构返回值。

type quotaFallbackOutboundUpstream struct {
	client *http.Client
}

func (u *quotaFallbackOutboundUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *quotaFallbackOutboundUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func newQuotaFallbackOutboundContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

// newQuotaFallbackOutboundAccount 复刻 dev fixture：客户端别名 deepseek-flash
// 正常映射到 deepseek-v4.1-flash，额度保护再把该 normal 上游映射到
// step-5-preview。三个模型都在账号 model_mapping 白名单中（target 为 identity，
// 与 gate fixture 一致）。
func newQuotaFallbackOutboundAccount(t *testing.T, baseURL string, protocolRules []any, snapshot *OpenCodeGoUsageSnapshot) *Account {
	t.Helper()
	credentials := map[string]any{
		"api_key":      "sk-quota-fallback-upstream",
		"base_url":     baseURL,
		"account_mode": AccountModeGo,
		"model_mapping": map[string]any{
			"deepseek-flash":   "deepseek-v4.1-flash",
			"step-5-preview":   "step-5-preview",
			"gpt-quota-target": "gpt-quota-target",
		},
		OpenCodeQuotaFallbackCredentialKey: map[string]any{
			"enabled":           true,
			"threshold_percent": 98,
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash": "step-5-preview",
			},
		},
	}
	if protocolRules != nil {
		credentials[protocolRulesCredentialKey] = protocolRules
	}
	extra := map[string]any{
		"openai_responses_supported": true,
	}
	if snapshot != nil {
		extra[OpenCodeGoUsageSnapshotExtraKey] = snapshot
	}
	return &Account{
		ID:          9101,
		Name:        "quota-fallback-outbound",
		Platform:    PlatformOpenCode,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: credentials,
		Extra:       extra,
	}
}

func quotaFallbackOutboundSnapshot(rolling, weekly, monthly float64, status string, fetchedAt time.Time) *OpenCodeGoUsageSnapshot {
	return openCodeQuotaFallbackSnapshot(status, openCodeQuotaFallbackUsageData(rolling, weekly, monthly, fetchedAt.Add(time.Hour)), fetchedAt)
}

func newQuotaFallbackOutboundService(t *testing.T, handler http.Handler) (*OpenAIGatewayService, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: &quotaFallbackOutboundUpstream{client: server.Client()},
	}, server
}

// TestOpenCodeQuotaFallbackForwardRawChatUsesTargetModel 驱动 chat 入站命中
// target 的 Chat Completions 协议路径（forwardAsRawChatCompletions）：出站 path
// 必须是 /v1/chat/completions，body.model 必须是替代目标；billing 仍保留 normal。
func TestOpenCodeQuotaFallbackForwardRawChatUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_qf","object":"chat.completion","model":"step-5-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))

	account := newQuotaFallbackOutboundAccount(t, server.URL, nil,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/chat/completions", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel, "billing must keep the normal upstream model")
	require.Equal(t, "step-5-preview", result.UpstreamModel, "actual upstream attribution must be the fallback target")
}

// TestOpenCodeQuotaFallbackForwardResponsesBodyUsesTargetModel 驱动 chat 入站但
// target protocol_rules 选择 Responses 的路径：真实 build/发送的 /v1/responses
// body 必须写入替代目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardResponsesBodyUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_qf\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"step-5-preview\"}}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_qf\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"step-5-preview\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
	}))

	protocolRules := []any{map[string]any{"pattern": "step-5-preview", "protocol": APIProtocolResponses}}
	account := newQuotaFallbackOutboundAccount(t, server.URL, protocolRules,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/responses", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound Responses body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardResponsesToChatUsesTargetModel 驱动 Responses
// 入站降级到 Chat Completions 的真实路径：出站 /v1/chat/completions body 必须是
// 替代目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardResponsesToChatUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_qf2","object":"chat.completion","model":"step-5-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))

	account := newQuotaFallbackOutboundAccount(t, server.URL, nil,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","input":"hello","stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/responses", body)

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, account, body, time.Now())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/chat/completions", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound Chat Completions body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardNativeAnthropicUsesTargetModel 驱动 target
// protocol_rules=anthropic 的原生 /v1/messages 直通：出站 body.model 必须是替代
// 目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardNativeAnthropicUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_qf","type":"message","role":"assistant","model":"step-5-preview","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))

	protocolRules := []any{map[string]any{"pattern": "step-5-preview", "protocol": APIProtocolAnthropic}}
	account := newQuotaFallbackOutboundAccount(t, server.URL, protocolRules,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/messages", body)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/messages", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound native Anthropic body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardUnknownDecisionAllowsConfiguredTarget 走真实
// Evaluate→ResolveRequest 入口：unknown 只放行配置里的直连 target，付费 source
// 仍拒绝。literal 216 的 unknown decision 丢弃 EffectiveMapping，此用例会红。
func TestOpenCodeQuotaFallbackForwardUnknownDecisionAllowsConfiguredTarget(t *testing.T) {
	cfg := openCodeQuotaFallbackEnabledConfig(map[string]string{"deepseek-v4.1-flash": "step-5-preview"})
	snap := openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusFailed, nil, time.Now())

	decision := EvaluateOpenCodeQuotaFallback(cfg, snap, nil, time.Now())
	require.Equal(t, OpenCodeQuotaFallbackStateUnknown, decision.State)

	direct := decision.ResolveRequest("step-5-preview")
	require.True(t, direct.DispatchAllowed, "direct call to the configured target must stay schedulable in unknown state")
	require.Equal(t, "step-5-preview", direct.EffectiveUpstreamModel)
	require.False(t, direct.MappingApplied)

	paid := decision.ResolveRequest("deepseek-v4.1-flash")
	require.False(t, paid.DispatchAllowed, "unknown must not auto-select the target for a paid source")
	require.Equal(t, "deepseek-v4.1-flash", paid.EffectiveUpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardUnknownDirectTargetUsesTargetProtocol 验证 unknown 状态
// 下直连 target 的真实出站 path：target 无 protocol_rules 时默认 Chat Completions，
// 不能因“未发生 source→target 改写”回落到 legacy Responses。
func TestOpenCodeQuotaFallbackForwardUnknownDirectTargetUsesTargetProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_qf3","object":"chat.completion","model":"step-5-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))

	account := newQuotaFallbackOutboundAccount(t, server.URL, nil,
		quotaFallbackOutboundSnapshot(0, 0, 0, OpenCodeGoUsageStatusFailed, time.Now()))
	body := []byte(`{"model":"step-5-preview","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/chat/completions", gotPath, "direct target must use the target protocol, not legacy Responses")
	require.Equal(t, "step-5-preview", gotModel)
	require.Equal(t, "step-5-preview", result.BillingModel, "direct target has no normal mapping to a different paid model")
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardMessagesChatUsesTargetModel 驱动 /v1/messages
// 入站、target protocol_rules=chat_completions 的 Anthropic→CC 路径：真实出站
// /v1/chat/completions body 必须是替代目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardMessagesChatUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_qf_msg","object":"chat.completion","model":"step-5-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))

	protocolRules := []any{map[string]any{"pattern": "step-5-preview", "protocol": APIProtocolChatCompletions}}
	account := newQuotaFallbackOutboundAccount(t, server.URL, protocolRules,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/messages", body)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/chat/completions", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound Anthropic→CC body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardMessagesResponsesUsesTargetModel 驱动
// /v1/messages 入站、target protocol_rules=responses 的 Anthropic→Responses
// 路径：真实出站 /v1/responses body 必须是替代目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardMessagesResponsesUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_qf_msg\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"step-5-preview\"}}\n\n"+
				"data: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_qf\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n"+
				"data: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"item_id\":\"msg_qf\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_qf\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n"+
				"data: {\"type\":\"response.output_item.done\",\"sequence_number\":4,\"output_index\":0,\"item\":{\"id\":\"msg_qf\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}}\n\n"+
				"data: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_qf_msg\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"step-5-preview\",\"output\":[{\"id\":\"msg_qf\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
	}))

	protocolRules := []any{map[string]any{"pattern": "step-5-preview", "protocol": APIProtocolResponses}}
	account := newQuotaFallbackOutboundAccount(t, server.URL, protocolRules,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/messages", body)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/responses", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound Anthropic→Responses body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardChatToNativeAnthropicUsesTargetModel 驱动 chat
// 入站、target protocol_rules=anthropic 的 Chat→native Anthropic 路径：真实出站
// /v1/messages body 必须是替代目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardChatToNativeAnthropicUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "text/event-stream")
		// 该路径无论客户端是否 stream 都强制上游 stream=true，
		// 非流式客户端由 handleCCBufferedFromNativeAnthropic 组装，必须返回 Anthropic SSE。
		_, _ = io.WriteString(w,
			"event: message_start\n"+
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_qf_cc\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"step-5-preview\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
				"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
				"event: content_block_stop\n"+
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"event: message_delta\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n"+
				"event: message_stop\n"+
				"data: {\"type\":\"message_stop\"}\n\n")
	}))

	protocolRules := []any{map[string]any{"pattern": "step-5-preview", "protocol": APIProtocolAnthropic}}
	account := newQuotaFallbackOutboundAccount(t, server.URL, protocolRules,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/messages", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound Chat→native Anthropic body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}

// TestOpenCodeQuotaFallbackForwardResponsesToNativeAnthropicUsesTargetModel 驱动
// Responses 入站、target protocol_rules=anthropic 的 native Anthropic 路径：真实
// 出站 /v1/messages body 必须是替代目标，billing 仍为 normal。
func TestOpenCodeQuotaFallbackForwardResponsesToNativeAnthropicUsesTargetModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotModel string
	svc, server := newQuotaFallbackOutboundService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "text/event-stream")
		// 该路径无论客户端是否 stream 都强制上游 stream=true，
		// 非流式客户端由 handleResponsesBufferedFromNativeAnthropic 组装，必须返回 Anthropic SSE。
		_, _ = io.WriteString(w,
			"event: message_start\n"+
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_qf_resp\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"step-5-preview\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
				"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
				"event: content_block_stop\n"+
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"event: message_delta\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n"+
				"event: message_stop\n"+
				"data: {\"type\":\"message_stop\"}\n\n")
	}))

	protocolRules := []any{map[string]any{"pattern": "step-5-preview", "protocol": APIProtocolAnthropic}}
	account := newQuotaFallbackOutboundAccount(t, server.URL, protocolRules,
		quotaFallbackOutboundSnapshot(100, 10, 5, OpenCodeGoUsageStatusOK, time.Now()))
	body := []byte(`{"model":"deepseek-flash","input":"hello","stream":false}`)
	c, recorder := newQuotaFallbackOutboundContext("/v1/responses", body)

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/messages", gotPath)
	require.Equal(t, "step-5-preview", gotModel, "outbound Responses→native Anthropic body model must be the quota-fallback target")
	require.Equal(t, "deepseek-v4.1-flash", result.BillingModel)
	require.Equal(t, "step-5-preview", result.UpstreamModel)
}
