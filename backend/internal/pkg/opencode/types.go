// Package opencode is the independent provider layer for OpenCode Go.
//
// OpenCode Go exposes the same upstream under two protocols:
//   - Anthropic Messages (/v1/messages) — used by minimax and qwen models.
//   - OpenAI Chat Completions (/v1/chat/completions) — used by the rest.
//
// The gateway decides per-request which path to take via UsesMessagesProtocol.
// Chat Completions requests are converted from Anthropic Messages using the
// shared apicompat chain (see converter.go); Messages requests are forwarded
// as-is. See https://opencode.ai/docs/zh-cn/go.
package opencode

import (
	"strings"
	"time"
)

// DefaultBaseURL is the OpenCode Go API base. Per-account base_url overrides it.
const DefaultBaseURL = "https://opencode.ai/zen/go/v1"

// ModelsPath is the public models listing endpoint relative to the base URL.
const ModelsPath = "/models"

// DefaultRequestTimeout bounds a single upstream attempt.
const DefaultRequestTimeout = 300 * time.Second

// Protocol is the upstream wire protocol a model speaks.
type Protocol string

const (
	// ProtocolChatCompletions routes via /chat/completions (OpenAI compatible).
	ProtocolChatCompletions Protocol = "chat/completions"
	// ProtocolMessages routes via /messages (Anthropic compatible, forwarded as-is).
	ProtocolMessages Protocol = "messages"
)

// Credential is the per-account OpenCode credential. OpenCode Go uses a static
// bearer API key — no token refresh. BaseURL lets a self-hosted mirror override
// the upstream; ProxyURL routes the upstream through an HTTP/SOCKS proxy.
//
// SessionCookies/WorkspaceID/AccountEmail are populated by the browser login
// flow (see opencode_browser_service) and let the gateway-side usage refresher
// reuse the web session without re-authenticating.
type Credential struct {
	APIKey   string `json:"api_key"`
	BaseURL  string `json:"base_url,omitempty"`
	ProxyURL string `json:"proxy_url,omitempty"`

	// Web-session fields (optional, set by browser login automation).
	WorkspaceID    string          `json:"opencode_workspace_id,omitempty"`
	AccountEmail   string          `json:"opencode_account_email,omitempty"`
	SessionCookies []SessionCookie `json:"opencode_session_cookies,omitempty"`
	LoginProvider  string          `json:"_login_provider,omitempty"` // github | google
}

// SessionCookie is a serialized browser cookie used to restore the web session.
type SessionCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Expires  int64  `json:"expires,omitempty"` // unix seconds; 0 = session cookie
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
	SameSite string `json:"same_site,omitempty"`
}

// EffectiveBaseURL returns the account base URL or the OpenCode default,
// trimmed of a trailing slash.
func (c *Credential) EffectiveBaseURL() string {
	b := strings.TrimSpace(c.BaseURL)
	if b == "" {
		b = DefaultBaseURL
	}
	return strings.TrimRight(b, "/")
}

// ModelInfo describes a model exposed via /v1/models for the opencode platform.
type ModelInfo struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Protocol    Protocol `json:"protocol,omitempty"`
}

// SupportedModels lists the OpenCode Go models exposed through the opencode
// platform. The IDs and protocol split match the live /zen/go/v1/models
// endpoint and the opencode console provider routing:
//   - minimax & qwen → Anthropic /messages (forwarded as-is)
//   - everything else → OpenAI /chat/completions (converted via apicompat)
func SupportedModels() []ModelInfo {
	return []ModelInfo{
		{ID: "minimax-m3", DisplayName: "MiniMax M3", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "minimax-m2.7", DisplayName: "MiniMax M2.7", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "minimax-m2.5", DisplayName: "MiniMax M2.5", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "kimi-k2.7-code", DisplayName: "Kimi K2.7 Code", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "kimi-k2.6", DisplayName: "Kimi K2.6", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "kimi-k2.5", DisplayName: "Kimi K2.5", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "glm-5.2", DisplayName: "GLM 5.2", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "glm-5.1", DisplayName: "GLM 5.1", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "glm-5", DisplayName: "GLM 5", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "qwen3.7-max", DisplayName: "Qwen3.7 Max", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "qwen3.7-plus", DisplayName: "Qwen3.7 Plus", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "qwen3.6-plus", DisplayName: "Qwen3.6 Plus", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "qwen3.5-plus", DisplayName: "Qwen3.5 Plus", MaxTokens: 64_000, Protocol: ProtocolMessages},
		{ID: "mimo-v2-pro", DisplayName: "MiMo V2 Pro", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "mimo-v2-omni", DisplayName: "MiMo V2 Omni", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "mimo-v2.5-pro", DisplayName: "MiMo V2.5 Pro", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "mimo-v2.5", DisplayName: "MiMo V2.5", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
		{ID: "hy3-preview", DisplayName: "HY3 Preview", MaxTokens: 64_000, Protocol: ProtocolChatCompletions},
	}
}

// messagesProtocolModels is the set routed through the Anthropic /messages
// upstream. Kept in sync with SupportedModels' Protocol=ProtocolMessages rows.
var messagesProtocolModels = map[string]bool{
	"minimax-m3":   true,
	"minimax-m2.7": true,
	"minimax-m2.5": true,
	"qwen3.7-max":  true,
	"qwen3.7-plus": true,
	"qwen3.6-plus": true,
	"qwen3.5-plus": true,
}

// UsesMessagesProtocol reports whether a model is served via the Anthropic
// /messages upstream (forwarded as-is) rather than /chat/completions.
// Any model with a "claude-" prefix is treated as Messages protocol so that
// opencode accounts pointing at a kiro/Anthropic backend work seamlessly.
func UsesMessagesProtocol(model string) bool {
	if messagesProtocolModels[NormalizeModelID(model)] {
		return true
	}
	return strings.HasPrefix(strings.ToLower(model), "claude-")
}

// NormalizeModelID lower-cases and strips a trailing "-thinking" gateway suffix
// so protocol lookup matches regardless of client-supplied variants.
func NormalizeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	return StripThinkingSuffix(m)
}

// MapModel normalizes a requested model name to an OpenCode upstream model ID.
// A "-thinking" gateway suffix is stripped (thinking is conveyed structurally,
// not via the model name). Unknown names pass through unchanged so an account
// model_mapping can still override them.
func MapModel(model string) string {
	return StripThinkingSuffix(strings.TrimSpace(model))
}

// StripThinkingSuffix removes a trailing "-thinking" (case-insensitive).
func StripThinkingSuffix(model string) string {
	const suffix = "-thinking"
	if len(model) >= len(suffix) && strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return model[:len(model)-len(suffix)]
	}
	return model
}

// IsOpenCodeModel reports whether a model name targets an OpenCode Go model.
// The check is by prefix/substring against the known families so account
// model_mapping aliases still resolve correctly.
func IsOpenCodeModel(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "deepseek") ||
		strings.Contains(m, "glm-5") ||
		strings.Contains(m, "kimi") ||
		strings.Contains(m, "minimax") ||
		strings.Contains(m, "qwen3") ||
		strings.Contains(m, "mimo") ||
		strings.Contains(m, "hy3") {
		return true
	}
	// Also accept the exact supported IDs (covers edge cases like "hy3-preview").
	return messagesProtocolModels[m] || isChatCompletionsModel(m)
}

// isChatCompletionsModel reports whether a (already lower-cased) id is one of
// the /chat/completions models. Used to back IsOpenCodeModel without iterating
// SupportedModels on every call.
func isChatCompletionsModel(lower string) bool {
	switch lower {
	case "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5",
		"glm-5.2", "glm-5.1", "glm-5",
		"deepseek-v4-pro", "deepseek-v4-flash",
		"mimo-v2-pro", "mimo-v2-omni", "mimo-v2.5-pro", "mimo-v2.5",
		"hy3-preview":
		return true
	}
	return false
}
