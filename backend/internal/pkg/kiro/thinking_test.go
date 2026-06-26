package kiro

import (
	"encoding/json"
	"testing"
)

// thinkingFields extracts the structured additionalModelRequestFields from a
// converted body, or nil if absent.
func thinkingFields(t *testing.T, model string) map[string]interface{} {
	t.Helper()
	req := &AnthropicRequest{
		Model:    model,
		Messages: []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, _, _, err := ConvertRequestV2(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := m["additionalModelRequestFields"]
	if !ok {
		return nil
	}
	var amrf map[string]interface{}
	if err := json.Unmarshal(raw, &amrf); err != nil {
		t.Fatalf("unmarshal amrf: %v", err)
	}
	return amrf
}

// TestThinking_Opus46_Adaptive: opus-4.6 supports effort, default high, no xhigh.
func TestThinking_Opus46_Adaptive(t *testing.T) {
	amrf := thinkingFields(t, "claude-opus-4-6-thinking")
	if amrf == nil {
		t.Fatal("opus-4.6 should carry structured thinking field")
	}
	if th := amrf["thinking"].(map[string]interface{}); th["type"] != "adaptive" {
		t.Errorf("opus-4.6 should use adaptive thinking, got %v", th)
	}
	if oc := amrf["output_config"].(map[string]interface{}); oc["effort"] != "high" {
		t.Errorf("opus-4.6 default effort should be high, got %v", oc["effort"])
	}
}

// TestThinking_Opus48_Default: opus-4.8 supports effort, default high.
func TestThinking_Opus48_Default(t *testing.T) {
	amrf := thinkingFields(t, "claude-opus-4-8-thinking")
	if amrf == nil {
		t.Fatal("opus-4.8 should carry structured thinking field")
	}
	if th := amrf["thinking"].(map[string]interface{}); th["type"] != "adaptive" {
		t.Errorf("opus-4.8 should use adaptive thinking, got %v", th)
	}
	if oc := amrf["output_config"].(map[string]interface{}); oc["effort"] != "high" {
		t.Errorf("opus-4.8 default effort should be high, got %v", oc["effort"])
	}
}

// TestThinking_UnsupportedModels: models without an effort schema get no field,
// matching Kiro IDE (opus-4.5/sonnet-4.5/sonnet-4/haiku-4.5).
func TestThinking_UnsupportedModels(t *testing.T) {
	for _, m := range []string{
		"claude-opus-4-5-thinking",
		"claude-sonnet-4-5-thinking",
		"claude-haiku-4-5-thinking",
	} {
		if amrf := thinkingFields(t, m); amrf != nil {
			t.Errorf("%s has no effort schema; should carry no field, got %v", m, amrf)
		}
	}
}
