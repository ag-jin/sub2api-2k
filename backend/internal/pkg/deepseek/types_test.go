package deepseek

import "testing"

func TestOpenCodeModels(t *testing.T) {
	models := OpenCodeModels()

	want := []ModelInfo{
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", MaxTokens: 64_000},
		{ID: "glm-5.2", DisplayName: "GLM 5.2", MaxTokens: 64_000},
		{ID: "glm-5.1", DisplayName: "GLM 5.1", MaxTokens: 64_000},
		{ID: "glm-5", DisplayName: "GLM 5", MaxTokens: 64_000},
	}

	if len(models) != len(want) {
		t.Fatalf("OpenCodeModels length = %d, want %d", len(models), len(want))
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("OpenCodeModels[%d] = %+v, want %+v", i, models[i], want[i])
		}
	}
}

func TestOpenCodeBaseURL(t *testing.T) {
	if OpenCodeBaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("OpenCodeBaseURL = %q, want %q", OpenCodeBaseURL, "https://opencode.ai/zen/go/v1")
	}
}

func TestIsOpenCodeModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{model: "glm-5.2", want: true},
		{model: "deepseek-v4-pro", want: true},
		{model: "claude-opus", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := IsOpenCodeModel(tc.model); got != tc.want {
				t.Fatalf("IsOpenCodeModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}
