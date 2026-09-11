package apicompat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyAnthropicContentFilterRefusal(t *testing.T) {
	resp := &AnthropicResponse{StopReason: AnthropicStopReasonPtr("end_turn")}
	ApplyAnthropicContentFilterRefusal(resp, "content_filter")
	assert.Equal(t, "refusal", AnthropicStopReasonString(resp.StopReason))

	// 非 content_filter 不改写。
	untouched := &AnthropicResponse{StopReason: AnthropicStopReasonPtr("end_turn")}
	ApplyAnthropicContentFilterRefusal(untouched, "stop")
	assert.Equal(t, "end_turn", AnthropicStopReasonString(untouched.StopReason))

	// content_filter 但派生为 tool_use 的不降级改写。
	toolUse := &AnthropicResponse{StopReason: AnthropicStopReasonPtr("tool_use")}
	ApplyAnthropicContentFilterRefusal(toolUse, "content_filter")
	assert.Equal(t, "tool_use", AnthropicStopReasonString(toolUse.StopReason))
}

func TestApplyAnthropicStreamRefusal(t *testing.T) {
	events := []AnthropicStreamEvent{
		{Type: "content_block_stop"},
		{Type: "message_delta", Delta: &AnthropicDelta{StopReason: "end_turn"}},
		{Type: "message_stop"},
	}
	out := ApplyAnthropicStreamRefusal(events, "content_filter")
	mapped := false
	for _, evt := range out {
		if evt.Type == "message_delta" && evt.Delta != nil {
			assert.Equal(t, "refusal", evt.Delta.StopReason)
			mapped = true
		}
	}
	assert.True(t, mapped, "message_delta stop_reason 应改写为 refusal")

	// 非 content_filter 保持原样。
	out2 := ApplyAnthropicStreamRefusal(out, "stop")
	for _, evt := range out2 {
		if evt.Type == "message_delta" && evt.Delta != nil {
			assert.Equal(t, "refusal", evt.Delta.StopReason) // 已被改写过，不再变化
		}
	}
}
