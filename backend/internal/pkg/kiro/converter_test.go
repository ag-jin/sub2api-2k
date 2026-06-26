package kiro

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseBody(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return m
}

func TestConvertRequest_SimpleText(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"hello world"`)},
		},
	}
	body, modelID, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if modelID != "claude-opus-4.8" {
		t.Errorf("modelID = %q, want claude-opus-4.8", modelID)
	}
	m := parseBody(t, body)
	cs := m["conversationState"].(map[string]interface{})
	cm := cs["currentMessage"].(map[string]interface{})
	uim := cm["userInputMessage"].(map[string]interface{})
	if uim["content"] != "hello world" {
		t.Errorf("content = %v", uim["content"])
	}
	if uim["modelId"] != "claude-opus-4.8" {
		t.Errorf("modelId = %v", uim["modelId"])
	}
	if uim["origin"] != "AI_EDITOR" {
		t.Errorf("origin = %v", uim["origin"])
	}
}

func TestConvertRequest_UnsupportedModel(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-4",
		MaxTokens: 64,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	_, _, err := ConvertRequest(req)
	if err == nil {
		t.Fatal("expected unsupported model error")
	}
}

func TestConvertRequest_SystemMessage(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 64,
		System:    json.RawMessage(`"You are helpful."`),
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, _, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	m := parseBody(t, body)
	cs := m["conversationState"].(map[string]interface{})
	hist, ok := cs["history"].([]interface{})
	if !ok || len(hist) < 2 {
		t.Fatalf("expected history with system pair, got %v", cs["history"])
	}
	first := hist[0].(map[string]interface{})
	uim := first["userInputMessage"].(map[string]interface{})
	content := uim["content"].(string)
	if !strings.Contains(content, "You are helpful.") {
		t.Errorf("system content missing: %q", content)
	}
	if !strings.Contains(content, "chunked") && !strings.Contains(content, "Chunked") {
		t.Errorf("chunked policy missing from system content")
	}
}

func TestConvertRequest_ThinkingModel(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8-thinking",
		MaxTokens: 64,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, _, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	s := string(body)
	// Thinking is conveyed via structured additionalModelRequestFields (IDE
	// alignment), NOT via legacy <thinking_mode> text tags.
	if strings.Contains(s, "<thinking_mode>") || strings.Contains(s, "<thinking_effort>") {
		t.Errorf("legacy thinking text tags should be gone, got: %s", s)
	}
	m := parseBody(t, body)
	amrf, ok := m["additionalModelRequestFields"].(map[string]interface{})
	if !ok {
		t.Fatalf("opus-4.8 should carry structured additionalModelRequestFields, body: %s", s)
	}
	th, _ := amrf["thinking"].(map[string]interface{})
	if th == nil || th["type"] != "adaptive" {
		t.Errorf("expected adaptive thinking, got %v", amrf["thinking"])
	}
	oc, _ := amrf["output_config"].(map[string]interface{})
	if oc == nil || oc["effort"] != "high" { // opus-4.8 default
		t.Errorf("expected default effort high, got %v", amrf["output_config"])
	}
}

func TestConvertRequest_MultiTurnHistory(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"first question"`)},
			{Role: "assistant", Content: json.RawMessage(`"first answer"`)},
			{Role: "user", Content: json.RawMessage(`"second question"`)},
		},
	}
	body, _, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	m := parseBody(t, body)
	cs := m["conversationState"].(map[string]interface{})
	hist := cs["history"].([]interface{})
	if len(hist) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(hist))
	}
	// current message should be "second question"
	cm := cs["currentMessage"].(map[string]interface{})
	uim := cm["userInputMessage"].(map[string]interface{})
	if uim["content"] != "second question" {
		t.Errorf("current content = %v", uim["content"])
	}
}

func TestConvertRequest_PrefillDropped(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"question"`)},
			{Role: "assistant", Content: json.RawMessage(`"prefill"`)},
		},
	}
	body, _, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	m := parseBody(t, body)
	cs := m["conversationState"].(map[string]interface{})
	cm := cs["currentMessage"].(map[string]interface{})
	uim := cm["userInputMessage"].(map[string]interface{})
	if uim["content"] != "question" {
		t.Errorf("after prefill drop, current = %v", uim["content"])
	}
}

func TestConvertRequest_ToolResult(t *testing.T) {
	content := `[{"type":"tool_result","tool_use_id":"tool_123","content":"result data"}]`
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(content)},
		},
	}
	body, _, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	m := parseBody(t, body)
	cs := m["conversationState"].(map[string]interface{})
	cm := cs["currentMessage"].(map[string]interface{})
	uim := cm["userInputMessage"].(map[string]interface{})
	ctx := uim["userInputMessageContext"].(map[string]interface{})
	trs, ok := ctx["toolResults"].([]interface{})
	if !ok || len(trs) != 1 {
		t.Fatalf("expected 1 toolResult, got %v", ctx["toolResults"])
	}
	tr := trs[0].(map[string]interface{})
	if tr["toolUseId"] != "tool_123" {
		t.Errorf("toolUseId = %v", tr["toolUseId"])
	}
}
