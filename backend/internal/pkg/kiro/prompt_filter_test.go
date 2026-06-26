package kiro

import (
	"strings"
	"testing"
)

func TestApplyPromptFilters_DefaultOffIsVerbatim(t *testing.T) {
	in := "  hello\n--- SYSTEM PROMPT ---\ngitStatus: dirty\nyou are claude code  "
	got := ApplyPromptFilters(PromptFilterConfig{}, in)
	if got != strings.TrimSpace(in) {
		t.Errorf("default-off should only trim, got %q", got)
	}
}

func TestStripBoundaryMarkers(t *testing.T) {
	in := "--- SYSTEM PROMPT ---\nkeep this line\n--- END SYSTEM PROMPT ---"
	got := ApplyPromptFilters(PromptFilterConfig{StripBoundaryMarkers: true}, in)
	if strings.Contains(got, "SYSTEM PROMPT ---") {
		t.Errorf("boundary markers not stripped: %q", got)
	}
	if !strings.Contains(got, "keep this line") {
		t.Errorf("content line lost: %q", got)
	}
}

func TestStripEnvNoise(t *testing.T) {
	in := strings.Join([]string{
		"You are a helpful assistant.",
		"# Environment",
		"working dir: /Users/foo/.claude/projects/bar",
		"platform: darwin",
		"# Real Section",
		"keep me",
		"gitStatus: M file.go",
		"Recent commits: abc123",
		"x-anthropic-billing-header: xyz",
		"normal content",
	}, "\n")
	got := ApplyPromptFilters(PromptFilterConfig{StripEnvNoise: true}, in)
	for _, bad := range []string{"# Environment", ".claude/projects/", "gitStatus:", "Recent commits:", "x-anthropic-billing-header:"} {
		if strings.Contains(got, bad) {
			t.Errorf("env noise %q not stripped: %q", bad, got)
		}
	}
	for _, keep := range []string{"You are a helpful assistant.", "# Real Section", "keep me", "normal content"} {
		if !strings.Contains(got, keep) {
			t.Errorf("content %q lost: %q", keep, got)
		}
	}
}

func TestStripClaudeCode(t *testing.T) {
	// Needs >=2 markers to trigger.
	in := "You are an interactive agent that helps users with software engineering tasks.\n# Doing tasks\nlots of detail here"
	got := ApplyPromptFilters(PromptFilterConfig{StripClaudeCode: true}, in)
	if !strings.Contains(got, "model backend for Claude Code CLI") {
		t.Errorf("CC prompt not replaced: %q", got)
	}
}

func TestStripClaudeCode_OneMarkerNotTriggered(t *testing.T) {
	in := "claude code is great but this is my own prompt with one marker only"
	got := ApplyPromptFilters(PromptFilterConfig{StripClaudeCode: true}, in)
	if strings.Contains(got, "model backend for Claude Code CLI") {
		t.Errorf("should not replace with only 1 marker: %q", got)
	}
}

func TestPromptFilterConfigFromEnv(t *testing.T) {
	t.Setenv("KIRO_PROMPT_FILTER_BOUNDARIES", "1")
	t.Setenv("KIRO_PROMPT_FILTER_ENV_NOISE", "true")
	cfg := PromptFilterConfigFromEnv()
	if !cfg.StripBoundaryMarkers || !cfg.StripEnvNoise {
		t.Errorf("env flags not parsed: %+v", cfg)
	}
	if cfg.StripClaudeCode {
		t.Errorf("unset flag should be false: %+v", cfg)
	}
}
