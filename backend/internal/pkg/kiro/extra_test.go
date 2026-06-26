package kiro

import (
	"encoding/json"
	"testing"
)

func TestCountTextTokens_Basic(t *testing.T) {
	if CountTextTokens("") != 0 {
		t.Errorf("empty should be 0, got %d", CountTextTokens(""))
	}
	// Western text: "hello world" = 11 chars / 4 = 2.75 tokens, <100 so *1.5
	got := CountTextTokens("hello world")
	if got <= 0 {
		t.Errorf("expected positive token count, got %d", got)
	}
	// Non-western (CJK) counts more units per char
	cjk := CountTextTokens("你好世界你好世界")
	ascii := CountTextTokens("12345678")
	if cjk <= ascii {
		t.Errorf("CJK (%d) should exceed same-length ASCII (%d)", cjk, ascii)
	}
}

func TestCountInputTokens_Request(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","system":"You are helpful.","messages":[{"role":"user","content":"Hello there, how are you?"}]}`)
	n := CountInputTokens(body)
	if n < 1 {
		t.Fatalf("expected >=1 token, got %d", n)
	}
}

func TestCountInputTokens_WithTools(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"Get weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)
	withTools := CountInputTokens(body)
	bodyNoTools := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	noTools := CountInputTokens(bodyNoTools)
	if withTools <= noTools {
		t.Errorf("tools should add tokens: with=%d no=%d", withTools, noTools)
	}
}

func TestHasWebSearchTool(t *testing.T) {
	req := &AnthropicRequest{
		Model:    "claude-opus-4-8",
		Messages: []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"search cats"`)}},
		Tools:    []AnthropicTool{{Name: "web_search"}},
	}
	if !HasWebSearchTool(req) {
		t.Error("should detect web_search tool")
	}
	req.Tools = append(req.Tools, AnthropicTool{Name: "other"})
	if HasWebSearchTool(req) {
		t.Error("should NOT detect when more than one tool")
	}
}

func TestExtractSearchQuery(t *testing.T) {
	req := &AnthropicRequest{
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"first"`)},
			{Role: "assistant", Content: json.RawMessage(`"ignored"`)},
			{Role: "user", Content: json.RawMessage(`"latest query"`)},
		},
	}
	if q := ExtractSearchQuery(req); q != "latest query" {
		t.Errorf("expected 'latest query', got %q", q)
	}
}

// emitCapture collects events to verify the websearch SSE sequence shape.
type wsCapture struct{ names []string }

func (c *wsCapture) WriteSSE(event string, data map[string]interface{}) error {
	c.names = append(c.names, event)
	return nil
}

func TestEmitWebSearchEvents_Sequence(t *testing.T) {
	cap := &wsCapture{}
	results := &WebSearchResults{Results: []WebSearchResult{
		{Title: "Cats", URL: "https://example.com/cats", Snippet: "About cats"},
	}}
	if err := emitWebSearchEvents(cap, "claude-opus-4-8", "cats", "web_search_tooluse_x", results, 10); err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Must start with message_start and end with message_stop
	if cap.names[0] != "message_start" {
		t.Errorf("first event = %s", cap.names[0])
	}
	if cap.names[len(cap.names)-1] != "message_stop" {
		t.Errorf("last event = %s", cap.names[len(cap.names)-1])
	}
	// Must contain a server_tool_use and web_search_tool_result block start
	joined := ""
	for _, n := range cap.names {
		joined += n + ","
	}
	wantSeq := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	for _, w := range wantSeq {
		found := false
		for _, n := range cap.names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %s in %v", w, cap.names)
		}
	}
}
