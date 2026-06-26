package kiro

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBudgetToEffort verifies the 5-bucket budget_tokens → client effort mapping.
func TestBudgetToEffort(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, ""},          // no budget → empty (caller uses model default)
		{-5, ""},         // negative → empty
		{1024, "low"},    // Anthropic low tier
		{2048, "low"},    // upper edge of low
		{2049, "medium"}, // crosses into medium
		{4096, "medium"},
		{6144, "medium"},
		{6145, "high"},
		{10240, "high"},
		{16384, "high"},
		{16385, "xhigh"},
		{28672, "xhigh"},
		{28673, "max"},
		{32768, "max"},   // Anthropic max tier
		{1000000, "max"}, // maxed-out client budget → max
	}
	for _, c := range cases {
		if got := budgetToEffort(c.budget); got != c.want {
			t.Errorf("budgetToEffort(%d) = %q, want %q", c.budget, got, c.want)
		}
	}
}

// TestClampEffortToModel verifies xhigh→max fallback for models without xhigh,
// and default fallback for otherwise-illegal values.
func TestClampEffortToModel(t *testing.T) {
	with := effortCapabilities["claude-opus-4.8"]   // has xhigh
	without := effortCapabilities["claude-opus-4.6"] // no xhigh, default high

	if got := clampEffortToModel("xhigh", with); got != "xhigh" {
		t.Errorf("opus-4.8 xhigh should stay xhigh, got %q", got)
	}
	if got := clampEffortToModel("xhigh", without); got != "max" {
		t.Errorf("opus-4.6 xhigh should clamp to max, got %q", got)
	}
	if got := clampEffortToModel("max", without); got != "max" {
		t.Errorf("opus-4.6 max should stay max, got %q", got)
	}
	if got := clampEffortToModel("low", without); got != "low" {
		t.Errorf("opus-4.6 low should stay low, got %q", got)
	}
	if got := clampEffortToModel("bogus", without); got != without.DefaultLevel {
		t.Errorf("opus-4.6 bogus should fall to default %q, got %q", without.DefaultLevel, got)
	}
}

// TestBuildEffortFields verifies per-model behavior: supported models get the
// structured field with thinking pre-enabled; unsupported models get nil.
func TestBuildEffortFields(t *testing.T) {
	// Unsupported model (no effort schema) → nil, even with thinking present.
	for _, m := range []string{"claude-sonnet-4.5", "claude-opus-4.5", "claude-haiku-4.5"} {
		if got := buildEffortFields(&AnthropicRequest{Thinking: &ThinkingConfig{Type: "enabled"}}, m); got != nil {
			t.Errorf("model %s should get nil effort field, got %v", m, got)
		}
	}

	// opus-4.8 with maxed budget → effort max.
	amrf := buildEffortFields(&AnthropicRequest{Thinking: &ThinkingConfig{BudgetTokens: 60000}}, "claude-opus-4.8")
	if amrf == nil {
		t.Fatal("opus-4.8 should get effort field")
	}
	th, _ := amrf["thinking"].(map[string]interface{})
	if th == nil || th["type"] != "adaptive" || th["display"] != "summarized" {
		t.Errorf("thinking shape wrong: %v", amrf["thinking"])
	}
	if oc := amrf["output_config"].(map[string]interface{}); oc["effort"] != "max" {
		t.Errorf("opus-4.8 budget=60000 want effort max, got %v", oc["effort"])
	}

	// opus-4.6 with client xhigh → clamped to max (no xhigh enum).
	amrf2 := buildEffortFields(&AnthropicRequest{OutputCfg: &OutputConfig{Effort: "xhigh"}}, "claude-opus-4.6")
	if oc := amrf2["output_config"].(map[string]interface{}); oc["effort"] != "max" {
		t.Errorf("opus-4.6 xhigh should clamp to max, got %v", oc["effort"])
	}

	// No client effort → model default. opus-4.7 default xhigh, opus-4.8 default high.
	a47 := buildEffortFields(&AnthropicRequest{}, "claude-opus-4.7")
	if oc := a47["output_config"].(map[string]interface{}); oc["effort"] != "xhigh" {
		t.Errorf("opus-4.7 default want xhigh, got %v", oc["effort"])
	}
	a48 := buildEffortFields(&AnthropicRequest{}, "claude-opus-4.8")
	if oc := a48["output_config"].(map[string]interface{}); oc["effort"] != "high" {
		t.Errorf("opus-4.8 default want high, got %v", oc["effort"])
	}
}

// TestEffortFieldsInBody verifies the field lands at the top level of the Kiro
// body for a supported model, and is absent for an unsupported one.
func TestEffortFieldsInBody(t *testing.T) {
	// Supported model (opus-4.8), thinking pre-enabled by default (no client thinking).
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 100,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, _, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	raw, ok := parsed["additionalModelRequestFields"]
	if !ok {
		t.Fatal("opus-4.8 body missing top-level additionalModelRequestFields (thinking should be preset)")
	}
	s := string(raw)
	for _, want := range []string{`"adaptive"`, `"summarized"`, `"output_config"`, `"effort":"high"`} {
		if !strings.Contains(s, want) {
			t.Errorf("additionalModelRequestFields missing %q\n  got: %s", want, s)
		}
	}

	// Unsupported model (sonnet-4.5) → no field even though it's a Claude model.
	req2 := &AnthropicRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 100,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body2, _, err := ConvertRequest(req2)
	if err != nil {
		t.Fatalf("convert2 failed: %v", err)
	}
	if strings.Contains(string(body2), "additionalModelRequestFields") {
		t.Error("sonnet-4.5 (no effort schema) should not include additionalModelRequestFields")
	}
}
