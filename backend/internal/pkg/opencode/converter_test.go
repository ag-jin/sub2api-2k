package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildUpstreamRequest verifies the Anthropic→ChatCompletions chain produces
// a clean OpenAI body: messages present, reasoning_effort mapped from
// output_config.effort, model resolved, stream flag — and critically NO
// deepseek-style `thinking` object (OpenCode Go does not accept it).
func TestBuildUpstreamRequest(t *testing.T) {
	anthropic := `{
		"model": "glm-5.2-thinking",
		"max_tokens": 100,
		"stream": true,
		"output_config": {"effort": "high"},
		"messages": [{"role": "user", "content": "hi"}]
	}`
	body, model, stream, err := BuildUpstreamRequest([]byte(anthropic))
	if err != nil {
		t.Fatalf("BuildUpstreamRequest: %v", err)
	}
	if model != "glm-5.2" {
		t.Errorf("model=%q want glm-5.2 (suffix stripped)", model)
	}
	if !stream {
		t.Error("stream should be true")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if string(obj["model"]) != `"glm-5.2"` {
		t.Errorf("upstream model=%s", obj["model"])
	}
	if got := string(obj["reasoning_effort"]); got != `"high"` {
		t.Errorf("reasoning_effort=%s want \"high\"", got)
	}
	// OpenCode Go must NOT receive a thinking object (deepseek-only field).
	if th, ok := obj["thinking"]; ok {
		t.Errorf("thinking object leaked into OpenCode body: %s", th)
	}
	if _, ok := obj["messages"]; !ok {
		t.Error("messages missing")
	}
	for _, leaked := range []string{"store", "include", "text", "instructions"} {
		if _, ok := obj[leaked]; ok {
			t.Errorf("Responses-only field %q leaked into ChatCompletions body", leaked)
		}
	}
}

// TestConvertNonStream verifies a Chat Completions JSON response with
// reasoning_content converts to an Anthropic response with a thinking block.
func TestConvertNonStream(t *testing.T) {
	ccResp := `{
		"id": "cc-1",
		"object": "chat.completion",
		"model": "glm-5.2",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"reasoning_content": "let me think...",
				"content": "the answer is 42"
			},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`
	out, usage, err := ConvertNonStream([]byte(ccResp), "glm-5.2")
	if err != nil {
		t.Fatalf("ConvertNonStream: %v", err)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("usage=%+v want in=10 out=5", usage)
	}
	s := string(out)
	if !strings.Contains(s, `"thinking"`) || !strings.Contains(s, "let me think") {
		t.Errorf("expected thinking block with reasoning, got: %s", s)
	}
	if !strings.Contains(s, "the answer is 42") {
		t.Errorf("expected answer text, got: %s", s)
	}
}
