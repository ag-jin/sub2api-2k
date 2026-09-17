package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 以下三条是 2026-09-17 生产实测的 CodeBuddy 上游原始帧（captured via
// https://www.facaiai.top/v1，buddy 链路 deepseek-v4.1-flash 流式），
// 保留原始字节以防回归时形状被"优化"掉。
const (
	cbStreamFrameRole = `data: {"id":"bb57907b9c72ec7d0740b03306492d6f","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789632200,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`

	cbStreamFrameReasoning = `data: {"id":"bb57907b9c72ec7d0740b03306492d6f","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789632200,"choices":[{"index":0,"delta":{"content":"","reasoning_content":"We","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`

	cbStreamFrameFinish = `data: {"id":"bb57907b9c72ec7d0740b03306492d6f","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789632200,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":{"name":"","arguments":""},"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":46,"completion_tokens":116,"total_tokens":162}}`
)

func TestNormalizeCodeBuddyChatStreamLine_ReasoningFrame(t *testing.T) {
	out, ok := normalizeCodeBuddyChatStreamLine(cbStreamFrameReasoning)
	require.True(t, ok, "空壳字段帧必须被改写")
	require.True(t, gjson.Valid(out[6:]), "输出仍是合法 JSON 行")

	delta := gjson.Get(out, "choices.0.delta")
	assert.Equal(t, "We", delta.Get("reasoning_content").String(), "思考文本必须保留")
	// 关键回归点：空数组 tool_calls 会让按 `!= null` 判定的客户端逐帧收尾思考块。
	assert.False(t, delta.Get("tool_calls").Exists(), "空 tool_calls 必须删除")
	assert.False(t, delta.Get("content").Exists(), "空串 content 必须删除")
	assert.False(t, delta.Get("refusal").Exists(), "空串 refusal 必须删除")
	assert.False(t, delta.Get("function_call").Exists(), "空壳 function_call 必须删除")
	assert.False(t, delta.Get("extra_fields").Exists(), "供应商私有字段必须删除")
	// 帧外层字段原样保留。
	assert.Equal(t, "bb57907b9c72ec7d0740b03306492d6f", gjson.Get(out, "id").String())
	assert.Equal(t, "deepseek-v4.1-flash", gjson.Get(out, "model").String())
	assert.Equal(t, int64(1789632200), gjson.Get(out, "created").Int())
	assert.True(t, gjson.Get(out, "choices.0.finish_reason").Type == gjson.Null, "空串 finish_reason 归一为 null")
	assert.True(t, gjson.Get(out, "usage").Type == gjson.Null, "usage 字段保留（null）")
}

func TestNormalizeCodeBuddyChatStreamLine_RoleFrameKeepsEmptyContent(t *testing.T) {
	out, ok := normalizeCodeBuddyChatStreamLine(cbStreamFrameRole)
	require.True(t, ok)

	delta := gjson.Get(out, "choices.0.delta")
	assert.Equal(t, "assistant", delta.Get("role").String())
	// 首帧 role+空 content 是官方标准形状，必须保留。
	assert.True(t, delta.Get("content").Exists(), "role 帧的空 content 保留")
	assert.Equal(t, "", delta.Get("content").String())
	assert.False(t, delta.Get("reasoning_content").Exists())
	assert.False(t, delta.Get("tool_calls").Exists())
	assert.False(t, delta.Get("function_call").Exists())
}

func TestNormalizeCodeBuddyChatStreamLine_FinishFrame(t *testing.T) {
	out, ok := normalizeCodeBuddyChatStreamLine(cbStreamFrameFinish)
	require.True(t, ok)

	assert.Equal(t, "stop", gjson.Get(out, "choices.0.finish_reason").String(), "真实 finish_reason 不被改写")
	assert.Equal(t, int64(162), gjson.Get(out, "usage.total_tokens").Int(), "终帧 usage 原样保留")
	delta := gjson.Get(out, "choices.0.delta")
	assert.False(t, delta.Get("tool_calls").Exists())
	assert.False(t, delta.Get("function_call").Exists())
	assert.True(t, delta.Get("content").Exists(), "role 帧的空 content 保留")
}

func TestNormalizeCodeBuddyChatStreamLine_RealToolCallPreserved(t *testing.T) {
	line := `data: {"id":"x","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"","reasoning_content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\""}}],"refusal":"","extra_fields":null,"function_call":null},"logprobs":null,"finish_reason":""}],"usage":null}`
	out, ok := normalizeCodeBuddyChatStreamLine(line)
	require.True(t, ok)

	tc := gjson.Get(out, "choices.0.delta.tool_calls")
	require.True(t, tc.IsArray())
	require.Len(t, tc.Array(), 1, "真实工具调用必须保留")
	assert.Equal(t, "Bash", tc.Array()[0].Get("function.name").String())
	assert.Equal(t, `{"command"`, tc.Array()[0].Get("function.arguments").String())
	assert.False(t, gjson.Get(out, "choices.0.delta.content").Exists())
	assert.False(t, gjson.Get(out, "choices.0.delta.refusal").Exists())
	assert.False(t, gjson.Get(out, "choices.0.delta.extra_fields").Exists())
	assert.True(t, gjson.Get(out, "choices.0.finish_reason").Type == gjson.Null)
}

func TestNormalizeCodeBuddyChatStreamLine_CleanFrameUntouched(t *testing.T) {
	// opencode 等标准上游形状（content:null、无空壳字段）不做任何改写——
	// 归一化只服务 codebuddy 账号，且必须对健康流零副作用。
	line := `data: {"id":"935591df","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789632203,"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"We"},"finish_reason":null}],"usage":null}`
	out, ok := normalizeCodeBuddyChatStreamLine(line)
	assert.False(t, ok, "无空壳字段的帧不应发生改写")
	assert.Equal(t, line, out)
}

func TestNormalizeCodeBuddyChatStreamLine_NonDataAndSentinel(t *testing.T) {
	for _, line := range []string{
		"",
		": heartbeat",
		"event: ping",
		"data: [DONE]",
		"data: ",
		"data: not-json",
		`data: {"id":"x","choices":[]}`,
		`data: {"id":"x"}`,
	} {
		out, ok := normalizeCodeBuddyChatStreamLine(line)
		assert.False(t, ok, "不应改写: %q", line)
		assert.Equal(t, line, out, "原样返回: %q", line)
	}
}

func TestNormalizeCodeBuddyChatStreamLine_MultipleChoices(t *testing.T) {
	line := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"","reasoning_content":"a","tool_calls":[],"extra_fields":null},"finish_reason":""},{"index":1,"delta":{"content":"b","tool_calls":[],"refusal":""},"finish_reason":""}],"usage":null}`
	out, ok := normalizeCodeBuddyChatStreamLine(line)
	require.True(t, ok)

	assert.False(t, gjson.Get(out, "choices.0.delta.tool_calls").Exists())
	assert.False(t, gjson.Get(out, "choices.1.delta.tool_calls").Exists(), "第二个 choice 同样归一")
	assert.Equal(t, "a", gjson.Get(out, "choices.0.delta.reasoning_content").String())
	assert.Equal(t, "b", gjson.Get(out, "choices.1.delta.content").String(), "真实 content 保留")
	assert.False(t, gjson.Get(out, "choices.1.delta.refusal").Exists())
	assert.True(t, gjson.Get(out, "choices.0.finish_reason").Type == gjson.Null)
	assert.True(t, gjson.Get(out, "choices.1.finish_reason").Type == gjson.Null)
}

func TestNormalizeCodeBuddyChatStreamLine_Idempotent(t *testing.T) {
	first, ok := normalizeCodeBuddyChatStreamLine(cbStreamFrameReasoning)
	require.True(t, ok)
	second, ok2 := normalizeCodeBuddyChatStreamLine(first)
	assert.False(t, ok2, "二次归一化不应再改写")
	assert.Equal(t, first, second)
}

// TestNormalizeCodeBuddyChatStreamLine_NoTriggerFieldsLeft 逐个断言"会让 ZCode
// 适配器收尾思考块"的字段在归一化后不复存在——这是本次修复的可观测契约。
func TestNormalizeCodeBuddyChatStreamLine_NoTriggerFieldsLeft(t *testing.T) {
	frames := []string{cbStreamFrameRole, cbStreamFrameReasoning, cbStreamFrameFinish}
	for _, frame := range frames {
		out, _ := normalizeCodeBuddyChatStreamLine(frame)
		delta := gjson.Get(out, "choices.0.delta")
		assert.False(t, delta.Get("tool_calls").Exists(), "tool_calls 不得残留（空数组即触发）")
		assert.False(t, delta.Get("extra_fields").Exists())
		assert.False(t, delta.Get("function_call").Exists())
		assert.False(t, delta.Get("refusal").Exists())
	}
}
