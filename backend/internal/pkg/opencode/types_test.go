package opencode

import "testing"

func TestSupportedModels(t *testing.T) {
	models := SupportedModels()
	if len(models) != 20 {
		t.Fatalf("SupportedModels length = %d, want 20 (match live /zen/go/v1/models)", len(models))
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m.ID == "" {
			t.Fatal("model with empty ID")
		}
		if seen[m.ID] {
			t.Fatalf("duplicate model ID %q", m.ID)
		}
		seen[m.ID] = true
		if m.Protocol != ProtocolMessages && m.Protocol != ProtocolChatCompletions {
			t.Fatalf("model %q has unknown protocol %q", m.ID, m.Protocol)
		}
	}
}

func TestOpenCodeBaseURL(t *testing.T) {
	if DefaultBaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("DefaultBaseURL = %q, want %q", DefaultBaseURL, "https://opencode.ai/zen/go/v1")
	}
}

func TestEffectiveBaseURL(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"", DefaultBaseURL},
		{"https://opencode.ai/zen/go/v1", "https://opencode.ai/zen/go/v1"},
		{"https://opencode.ai/zen/go/v1/", "https://opencode.ai/zen/go/v1"},
		{"https://mirror.example.com/go", "https://mirror.example.com/go"},
	}
	for _, c := range cases {
		cred := &Credential{BaseURL: c.base}
		if got := cred.EffectiveBaseURL(); got != c.want {
			t.Errorf("EffectiveBaseURL(%q)=%q want %q", c.base, got, c.want)
		}
	}
}

func TestUsesMessagesProtocol(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		// Messages-protocol families.
		{"minimax-m3", true},
		{"minimax-m2.7", true},
		{"minimax-m2.5", true},
		{"qwen3.7-max", true},
		{"qwen3.7-plus", true},
		{"qwen3.6-plus", true},
		{"qwen3.5-plus", true},
		// Case-insensitive + -thinking suffix tolerated.
		{"Qwen3.7-Max", true},
		{"minimax-m3-thinking", true},
		// Chat/completions models → false.
		{"glm-5.2", false},
		{"deepseek-v4-pro", false},
		{"kimi-k2.7-code", false},
		{"mimo-v2.5", false},
		{"hy3-preview", false},
		// Non-opencode.
		{"claude-opus", true},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := UsesMessagesProtocol(tc.model); got != tc.want {
				t.Fatalf("UsesMessagesProtocol(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestIsOpenCodeModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"glm-5.2", true},
		{"glm-5.1", true},
		{"GLM-5", true},
		{"deepseek-v4-pro", true},
		{"deepseek-v4-flash", true},
		{"kimi-k2.7-code", true},
		{"minimax-m3", true},
		{"qwen3.7-max", true},
		{"mimo-v2.5", true},
		{"hy3-preview", true},
		{"claude-opus", false},
		{"gpt-4o", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := IsOpenCodeModel(tc.model); got != tc.want {
				t.Fatalf("IsOpenCodeModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestMapModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"glm-5.2", "glm-5.2"},
		{"glm-5.2-thinking", "glm-5.2"},
		{"DEEPSEEK-V4-PRO-THINKING", "DEEPSEEK-V4-PRO"},
		{"  qwen3.7-max  ", "qwen3.7-max"},
		{"unknown-model", "unknown-model"}, // pass-through for model_mapping
	}
	for _, c := range cases {
		if got := MapModel(c.in); got != c.want {
			t.Errorf("MapModel(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestStripThinkingSuffix(t *testing.T) {
	if got := StripThinkingSuffix("glm-5.2-thinking"); got != "glm-5.2" {
		t.Errorf("got %q", got)
	}
	if got := StripThinkingSuffix("glm-5.2"); got != "glm-5.2" {
		t.Errorf("no-suffix should be unchanged, got %q", got)
	}
}
