package apicompat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 票 #38 C2：合成 Anthropic 响应的 id 必须带 Anthropic 规范前缀
// （message → msg_，tool_use → toolu_）。证据：acct509-nonstream2.json 的合成
// 响应 id 直接透传上游 `0217914…`；acct509k-t3.sse 的 tool_use 块 id 透传
// `call_00_lo5kp1rjy7szvx6pmqq4gy61`。上游原值已由 usage_logs.upstream_request_id
// 落库，改写不丢可追溯性。

func TestChatCompletionsResponseToAnthropic_NormalizesSynthesizedIDs(t *testing.T) {
	const upstreamID = "021791444165008fd3ae808d5ba084b55a7f9e1600032cd66e910"
	const upstreamToolID = "call_00_VFlbhA2SmVEan38jnrQl0081"

	resp := &ChatCompletionsResponse{
		ID:    upstreamID,
		Model: "deepseek-v4.1-flash",
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ChatToolCall{{
					ID:       upstreamToolID,
					Type:     "function",
					Function: ChatFunctionCall{Name: "get_weather", Arguments: `{"city":"SF"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	out := ChatCompletionsResponseToAnthropic(resp, "deepseek-v4.1-flash")
	require.True(t, strings.HasPrefix(out.ID, "msg_"), "message id must use the msg_ prefix, got %q", out.ID)
	require.NotContains(t, out.ID, upstreamID, "上游原 id 不得直接透传")
	require.Len(t, out.Content, 1)
	require.Equal(t, "tool_use", out.Content[0].Type)
	require.True(t, strings.HasPrefix(out.Content[0].ID, "toolu_"),
		"tool_use id must use the toolu_ prefix, got %q", out.Content[0].ID)
	require.NotContains(t, out.Content[0].ID, upstreamToolID, "上游 tool_call id 不得直接透传")

	// 同一上游 id 恒定映射到同一合成 id：重试/续跑/多轮引用不漂移。
	again := ChatCompletionsResponseToAnthropic(resp, "deepseek-v4.1-flash")
	require.Equal(t, out.ID, again.ID)
	require.Equal(t, out.Content[0].ID, again.Content[0].ID)

	// 不同上游 id 必须映射到不同合成 id（不得退化为常量）。
	other := ChatCompletionsResponseToAnthropic(&ChatCompletionsResponse{
		ID: "021791443161620a4c6d1552d81c7895c46a24cfcfdd887bd7870",
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: []byte(`"ok"`)},
			FinishReason: "stop",
		}},
	}, "deepseek-v4.1-flash")
	require.NotEqual(t, out.ID, other.ID)
}

func TestChatCompletionsResponseToAnthropic_KeepsCanonicalIDs(t *testing.T) {
	// 上游本就给 Anthropic 形态 id（Claude 兼容 CC 端点）时原样保留：
	// 客户端下一轮回传该 id 时仍能与上游对上。
	resp := &ChatCompletionsResponse{
		ID: "msg_01ABCxyz",
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ChatToolCall{{
					ID:       "toolu_01XYZabc",
					Type:     "function",
					Function: ChatFunctionCall{Name: "noop"},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	out := ChatCompletionsResponseToAnthropic(resp, "claude-compatible-model")
	require.Equal(t, "msg_01ABCxyz", out.ID)
	require.Len(t, out.Content, 1)
	require.Equal(t, "toolu_01XYZabc", out.Content[0].ID)
}

func TestChatCompletionsChunkToAnthropicEvents_SynthesizedIDsNormalized(t *testing.T) {
	const upstreamID = "021791443161620a4c6d1552d81c7895c46a24cfcfdd887bd7870"
	const upstreamToolID = "call_00_lo5kp1rjy7szvx6pmqq4gy61"

	events := collectAnthropicStreamEvents(t, []string{
		`{"id":"` + upstreamID + `","choices":[{"index":0,"delta":{"reasoning_content":"查天气"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + upstreamToolID + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})

	var messageID string
	for _, e := range events {
		if e.Type == "message_start" && e.Message != nil {
			messageID = e.Message.ID
		}
	}
	require.True(t, strings.HasPrefix(messageID, "msg_"), "message_start id must use the msg_ prefix, got %q", messageID)
	require.NotContains(t, messageID, upstreamID, "上游原 id 不得直接透传")

	tools := assembleToolUseBlocks(events)
	require.Len(t, tools, 1)
	require.True(t, strings.HasPrefix(tools[0].ID, "toolu_"),
		"streamed tool_use id must use the toolu_ prefix, got %q", tools[0].ID)
	require.NotContains(t, tools[0].ID, upstreamToolID, "上游 tool_call id 不得直接透传")
}

func TestChatCompletionsChunkToAnthropicEvents_ToolUseIDNormalizedWithoutUpstreamID(t *testing.T) {
	// 上游 tool_call 不带 id 时本地生成占位 id，同样必须落在 toolu_ 前缀下。
	events := collectAnthropicStreamEvents(t, []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"noop","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})

	tools := assembleToolUseBlocks(events)
	require.Len(t, tools, 1)
	require.True(t, strings.HasPrefix(tools[0].ID, "toolu_"),
		"locally generated tool_use id must use the toolu_ prefix, got %q", tools[0].ID)
}
