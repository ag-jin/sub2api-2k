package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 票 #38 B：合成 Anthropic 响应里的 thinking 块必须带签名。
// 证据：内层 /v1/messages（opencode 账号 deepseek-v4.1-flash）抓包
// content_block_start(thinking) + thinking_delta×N + content_block_stop，
// 全程没有 signature_delta；非流式 thinking 块也没有 signature 字段。
// 上游是 OpenAI 兼容 CC 端点，永远不会给签名，只能在合成层本地生成。

// anthropicBlockSequence 按内容块索引归组的事件类型序列与签名值。
type anthropicBlockSequence struct {
	types     []string
	signature string
}

// collectAnthropicBlockSequences 把流事件按 index 归组，供逐块核对生命周期。
func collectAnthropicBlockSequences(events []AnthropicStreamEvent) map[int]*anthropicBlockSequence {
	out := map[int]*anthropicBlockSequence{}
	for _, e := range events {
		if e.Index == nil {
			continue
		}
		seq, ok := out[*e.Index]
		if !ok {
			seq = &anthropicBlockSequence{}
			out[*e.Index] = seq
		}
		switch e.Type {
		case "content_block_start":
			blockType := ""
			if e.ContentBlock != nil {
				blockType = e.ContentBlock.Type
			}
			seq.types = append(seq.types, "start:"+blockType)
		case "content_block_delta":
			deltaType := ""
			if e.Delta != nil {
				deltaType = e.Delta.Type
				if deltaType == "signature_delta" {
					seq.signature = e.Delta.Signature
				}
			}
			seq.types = append(seq.types, "delta:"+deltaType)
		case "content_block_stop":
			seq.types = append(seq.types, "stop")
		}
	}
	return out
}

// TestChatCompletionsChunkToAnthropicEvents_ThinkingBlockEndsWithSignatureDelta
// 钉住流式 thinking 块的事件序列：start → thinking_delta* → signature_delta → stop，
// 签名非空且只出现在 thinking 块上（文本块不得多出签名帧）。
func TestChatCompletionsChunkToAnthropicEvents_ThinkingBlockEndsWithSignatureDelta(t *testing.T) {
	events := collectAnthropicStreamEvents(t, []string{
		`{"id":"02179144314747650b1c418616fd9b5f78a16c5c9d34162ea7cf3","choices":[{"index":0,"delta":{"reasoning_content":"剖析"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"需求"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答复"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	})

	sequences := collectAnthropicBlockSequences(events)
	thinkingSeq := sequences[0]
	require.NotNil(t, thinkingSeq, "thinking 块必须开在索引 0")
	require.Equal(t, []string{
		"start:thinking",
		"delta:thinking_delta",
		"delta:thinking_delta",
		"delta:signature_delta",
		"stop",
	}, thinkingSeq.types, "thinking 块必须在 stop 前补发 signature_delta")
	require.NotEmpty(t, thinkingSeq.signature, "signature_delta 必须带非空签名")

	textSeq := sequences[1]
	require.NotNil(t, textSeq, "文本块必须开在索引 1")
	require.Equal(t, []string{
		"start:text",
		"delta:text_delta",
		"stop",
	}, textSeq.types, "文本块不得被补签名")
	require.Empty(t, textSeq.signature)
}

// TestChatCompletionsChunkToAnthropicEvents_ThinkingSignatureStablePerContent
// 同一深思内容两次合成的签名必须一致（客户端多轮回传时稳定）。
func TestChatCompletionsChunkToAnthropicEvents_ThinkingSignatureStablePerContent(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"同"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"一段"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答案"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}

	first := collectAnthropicBlockSequences(collectAnthropicStreamEvents(t, chunks))
	second := collectAnthropicBlockSequences(collectAnthropicStreamEvents(t, chunks))

	require.NotNil(t, first[0], "thinking 块必须开在索引 0")
	require.NotNil(t, second[0], "thinking 块必须开在索引 0")
	require.NotEmpty(t, first[0].signature)
	require.Equal(t, first[0].signature, second[0].signature, "同内容必须得到同一签名")
}

// TestChatCompletionsResponseToAnthropic_ThinkingBlockCarriesSignature
// 非流式合成响应：thinking 块补 signature 字段，文本块不补；且与流式路径同源
// （同一深思文本在两条路径上派生出同一签名）。
func TestChatCompletionsResponseToAnthropic_ThinkingBlockCarriesSignature(t *testing.T) {
	const reasoning = "先看用户要两个字，然后照做。"

	resp := &ChatCompletionsResponse{
		ID:    "7a67b6ab-cd69-4f35-94be-e71697fcd8c5",
		Model: "deepseek-v4.1-flash",
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessage{
				Role:             "assistant",
				Content:          json.RawMessage(`"收到"`),
				ReasoningContent: reasoning,
			},
			FinishReason: "stop",
		}},
	}

	out := ChatCompletionsResponseToAnthropic(resp, "deepseek-v4.1-flash")
	require.Len(t, out.Content, 2)
	require.Equal(t, "thinking", out.Content[0].Type)
	require.Equal(t, reasoning, out.Content[0].Thinking)
	require.NotEmpty(t, out.Content[0].Signature, "非流式 thinking 块必须带 signature")
	require.Equal(t, "text", out.Content[1].Type)
	require.Empty(t, out.Content[1].Signature, "文本块不得被补签名")

	streamed := collectAnthropicBlockSequences(collectAnthropicStreamEvents(t, []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"先看用户要两个字，然后照做。"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"收到"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}))
	require.NotNil(t, streamed[0], "thinking 块必须开在索引 0")
	require.Equal(t, out.Content[0].Signature, streamed[0].signature,
		"流式与非流式必须对同一深思文本派生出同一签名")
}

// TestAnthropicToChatCompletionsRequest_AcceptsThinkingSignature 是入站守卫：
// 客户端把网关合成过的 thinking 块（带 signature）原样回传时，Anthropic → CC
// 转换不得报错，也不得把 signature 当未知字段塞给 Chat Completions 上游。
func TestAnthropicToChatCompletionsRequest_AcceptsThinkingSignature(t *testing.T) {
	const signature = "c2lnbmF0dXJlLW9wYXF1ZS12YWx1ZQ=="

	withToolCall := &AnthropicRequest{
		Model:     "deepseek-v4.1-flash",
		MaxTokens: 128,
		Tools: []AnthropicTool{
			{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"北京天气"`)},
			{Role: "assistant", Content: json.RawMessage(`[` +
				`{"type":"thinking","thinking":"需要调用工具查天气","signature":"` + signature + `"},` +
				`{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{"city":"Beijing"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_abc","content":"晴"}]`)},
		},
	}

	converted, err := AnthropicToChatCompletionsRequest(withToolCall)
	require.NoError(t, err, "回传带 signature 的 thinking 块不得报错")
	require.Len(t, converted.Messages, 3)
	assistant := converted.Messages[1]
	require.Equal(t, "assistant", assistant.Role)
	require.Equal(t, "需要调用工具查天气", assistant.ReasoningContent,
		"signature 不得妨碍 reasoning_content 回传（DeepSeek 工具轮必需）")
	require.Len(t, assistant.ToolCalls, 1)
	require.Equal(t, "get_weather", assistant.ToolCalls[0].Function.Name)

	body, err := json.Marshal(converted)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(body), "signature"),
		"signature 不得作为未知字段下发上游：%s", string(body))

	// 无工具调用的纯文本轮：thinking 依既有语义丢弃，但仍不得报错。
	textOnly := &AnthropicRequest{
		Model:     "deepseek-v4.1-flash",
		MaxTokens: 128,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"继续"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"上一轮推理","signature":"` + signature + `"}]`)},
		},
	}
	convertedTextOnly, err := AnthropicToChatCompletionsRequest(textOnly)
	require.NoError(t, err)
	require.Len(t, convertedTextOnly.Messages, 2)
	require.Empty(t, convertedTextOnly.Messages[1].ReasoningContent)
}
