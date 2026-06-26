// Package deepseek is the shared OpenAI-ChatCompletions compatible provider
// layer for DeepSeek official API and OpenCode Go. It accepts Anthropic Messages
// requests and converts them to/from Chat Completions using the shared
// apicompat conversion chain.
package deepseek

import (
	"strings"
	"time"
)

// DefaultBaseURL is the DeepSeek official API base. Per-account base_url
// overrides it (e.g. OpenCode Go: https://opencode.ai/zen/go/v1).
const DefaultBaseURL = "https://api.deepseek.com"

// OpenCodeBaseURL is the OpenCode Go API base.
const OpenCodeBaseURL = "https://opencode.ai/zen/go/v1"

// DefaultRequestTimeout bounds a single upstream attempt.
const DefaultRequestTimeout = 300 * time.Second

// Credential is the per-account DeepSeek credential. DeepSeek/OpenCode use a
// static bearer API key — no token refresh, unlike Kiro.
type Credential struct {
	APIKey   string `json:"api_key"`
	BaseURL  string `json:"base_url,omitempty"`
	ProxyURL string `json:"proxy_url,omitempty"`
}

// EffectiveBaseURL returns the account base URL or the DeepSeek default,
// trimmed of a trailing slash.
func (c *Credential) EffectiveBaseURL() string {
	b := strings.TrimSpace(c.BaseURL)
	if b == "" {
		b = DefaultBaseURL
	}
	return strings.TrimRight(b, "/")
}

// ModelInfo describes a model exposed via /v1/models for the deepseek platform.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
}

// SupportedModels lists the DeepSeek models the provider exposes. Both the
// official API and OpenCode Go use these exact IDs over /chat/completions.
func SupportedModels() []ModelInfo {
	return []ModelInfo{
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", MaxTokens: 64_000},
		{ID: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash", MaxTokens: 64_000},
	}
}

// OpenCodeModels lists the OpenCode Go models exposed through the opencode platform.
func OpenCodeModels() []ModelInfo {
	return []ModelInfo{
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", MaxTokens: 64_000},
		{ID: "glm-5.2", DisplayName: "GLM 5.2", MaxTokens: 64_000},
		{ID: "glm-5.1", DisplayName: "GLM 5.1", MaxTokens: 64_000},
		{ID: "glm-5", DisplayName: "GLM 5", MaxTokens: 64_000},
	}
}

// MapModel normalizes a requested model name to a DeepSeek upstream model ID.
// A "-thinking" gateway suffix is stripped (thinking is conveyed structurally,
// not via the model name). Unknown names pass through unchanged so an account
// model_mapping can still override them.
func MapModel(model string) string {
	m := StripThinkingSuffix(strings.TrimSpace(model))
	return m
}

// StripThinkingSuffix removes a trailing "-thinking" (case-insensitive).
func StripThinkingSuffix(model string) string {
	const suffix = "-thinking"
	if len(model) >= len(suffix) && strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return model[:len(model)-len(suffix)]
	}
	return model
}

// IsDeepseekModel reports whether a model name targets a DeepSeek model.
func IsDeepseekModel(model string) bool {
	s := strings.ToLower(model)
	return strings.Contains(s, "deepseek") || strings.Contains(s, "glm")
}

// IsOpenCodeModel reports whether a model name targets an OpenCode Go model.
func IsOpenCodeModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "deepseek") || strings.Contains(m, "glm")
}
