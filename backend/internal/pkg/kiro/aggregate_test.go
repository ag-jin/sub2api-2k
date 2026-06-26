package kiro

import "testing"

func TestAggregatingWriter_TextResponse(t *testing.T) {
	a := NewAggregatingWriter()
	a.WriteSSE("message_start", map[string]interface{}{
		"message": map[string]interface{}{"usage": map[string]interface{}{"input_tokens": 100}},
	})
	a.WriteSSE("content_block_start", map[string]interface{}{"index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""}})
	a.WriteSSE("content_block_delta", map[string]interface{}{"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": "Hello "}})
	a.WriteSSE("content_block_delta", map[string]interface{}{"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": "world"}})
	a.WriteSSE("content_block_stop", map[string]interface{}{"index": 0})
	a.WriteSSE("message_delta", map[string]interface{}{
		"delta": map[string]interface{}{"stop_reason": "end_turn"}})

	resp := a.BuildResponse("msg_1", "claude-opus-4-8", 5)
	content := resp["content"].([]map[string]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(content))
	}
	if content[0]["text"] != "Hello world" {
		t.Errorf("text = %v", content[0]["text"])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", resp["stop_reason"])
	}
	usage := resp["usage"].(map[string]interface{})
	if usage["input_tokens"] != 100 {
		t.Errorf("input_tokens = %v", usage["input_tokens"])
	}
}

func TestAggregatingWriter_ToolUse(t *testing.T) {
	a := NewAggregatingWriter()
	a.WriteSSE("content_block_start", map[string]interface{}{"index": 0,
		"content_block": map[string]interface{}{"type": "tool_use", "id": "t1", "name": "get_weather", "input": map[string]interface{}{}}})
	a.WriteSSE("content_block_delta", map[string]interface{}{"index": 0,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": `{"city":`}})
	a.WriteSSE("content_block_delta", map[string]interface{}{"index": 0,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": `"NYC"}`}})
	a.WriteSSE("content_block_stop", map[string]interface{}{"index": 0})
	a.WriteSSE("message_delta", map[string]interface{}{
		"delta": map[string]interface{}{"stop_reason": "tool_use"}})

	resp := a.BuildResponse("msg_1", "claude-opus-4-8", 3)
	content := resp["content"].([]map[string]interface{})
	if len(content) != 1 || content[0]["type"] != "tool_use" {
		t.Fatalf("expected tool_use block, got %v", content)
	}
	input, ok := content[0]["input"].(map[string]interface{})
	if !ok || input["city"] != "NYC" {
		t.Errorf("input not parsed correctly: %v", content[0]["input"])
	}
	if resp["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", resp["stop_reason"])
	}
}
