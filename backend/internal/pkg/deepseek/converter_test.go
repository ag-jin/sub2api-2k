package deepseek

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMapModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"deepseek-v4-pro", "deepseek-v4-pro"},
		{"deepseek-v4-pro-thinking", "deepseek-v4-pro"},
		{"deepseek-v4-flash-thinking", "deepseek-v4-flash"},
		{"DEEPSEEK-V4-PRO-THINKING", "DEEPSEEK-V4-PRO"},
		{"deepseek-v4-flash", "deepseek-v4-flash"},
		{"  deepseek-v4-pro  ", "deepseek-v4-pro"},
	}
	for _, c := range cases {
		if got := MapModel(c.in); got != c.want {
			t.Errorf("MapModel(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestStripThinkingSuffix(t *testing.T) {
	if got := StripThinkingSuffix("deepseek-v4-pro-thinking"); got != "deepseek-v4-pro" {
		t.Errorf("got %q", got)
	}
	if got := StripThinkingSuffix("deepseek-v4-pro"); got != "deepseek-v4-pro" {
		t.Errorf("no-suffix should be unchanged, got %q", got)
	}
}

func TestEffectiveBaseURL(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"", DefaultBaseURL},
		{"https://api.deepseek.com", "https://api.deepseek.com"},
		{"https://api.deepseek.com/", "https://api.deepseek.com"},
		{"https://opencode.ai/zen/go/v1", "https://opencode.ai/zen/go/v1"},
		{"https://opencode.ai/zen/go/v1/", "https://opencode.ai/zen/go/v1"},
	}
	for _, c := range cases {
		cred := &Credential{BaseURL: c.base}
		if got := cred.EffectiveBaseURL(); got != c.want {
			t.Errorf("EffectiveBaseURL(%q)=%q want %q", c.base, got, c.want)
		}
	}
}

// TestBuildUpstreamRequest verifies the Anthropic→ChatCompletions chain produces
// a clean OpenAI body: messages present, reasoning_effort mapped from
// output_config.effort, thinking object injected, model resolved, stream flag.
func TestBuildUpstreamRequest(t *testing.T) {
	anthropic := `{
		"model": "deepseek-v4-pro-thinking",
		"max_tokens": 100,
		"stream": true,
		"output_config": {"effort": "high"},
		"messages": [{"role": "user", "content": "hi"}]
	}`
	body, model, stream, err := BuildUpstreamRequest([]byte(anthropic))
	if err != nil {
		t.Fatalf("BuildUpstreamRequest: %v", err)
	}
	if model != "deepseek-v4-pro" {
		t.Errorf("model=%q want deepseek-v4-pro (suffix stripped)", model)
	}
	if !stream {
		t.Error("stream should be true")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	// model echoed
	if string(obj["model"]) != `"deepseek-v4-pro"` {
		t.Errorf("upstream model=%s", obj["model"])
	}
	// reasoning_effort present (high)
	if got := string(obj["reasoning_effort"]); got != `"high"` {
		t.Errorf("reasoning_effort=%s want \"high\"", got)
	}
	// thinking injected, type enabled (default-on)
	if th := string(obj["thinking"]); !strings.Contains(th, `"type":"enabled"`) {
		t.Errorf("thinking=%s want type enabled", th)
	}
	// messages present, NOT Responses-only fields (store/include/verbosity)
	if _, ok := obj["messages"]; !ok {
		t.Error("messages missing")
	}
	for _, leaked := range []string{"store", "include", "text", "instructions"} {
		if _, ok := obj[leaked]; ok {
			t.Errorf("Responses-only field %q leaked into ChatCompletions body", leaked)
		}
	}
}

// TestBuildUpstreamRequest_ThinkingDisabled verifies a client disabling thinking
// is honored.
func TestBuildUpstreamRequest_ThinkingDisabled(t *testing.T) {
	anthropic := `{
		"model": "deepseek-v4-pro",
		"max_tokens": 100,
		"thinking": {"type": "disabled"},
		"messages": [{"role": "user", "content": "hi"}]
	}`
	body, _, _, err := BuildUpstreamRequest([]byte(anthropic))
	if err != nil {
		t.Fatalf("BuildUpstreamRequest: %v", err)
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(body, &obj)
	if th := string(obj["thinking"]); !strings.Contains(th, `"type":"disabled"`) {
		t.Errorf("thinking=%s want type disabled", th)
	}
}

// TestConvertNonStream verifies a DeepSeek Chat Completions JSON response with
// reasoning_content converts to an Anthropic response with a thinking block.
func TestConvertNonStream(t *testing.T) {
	ccResp := `{
		"id": "cc-1",
		"object": "chat.completion",
		"model": "deepseek-v4-pro",
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
	out, usage, err := ConvertNonStream([]byte(ccResp), "deepseek-v4-pro")
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
