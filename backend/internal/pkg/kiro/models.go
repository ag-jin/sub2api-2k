package kiro

import "strings"

// MapModel translates an Anthropic-style model name to the Kiro backend model ID.
// Returns the mapped name and true if recognized, or "" and false otherwise.
// Mirrors kiro.rs src/anthropic/converter.rs::map_model.
func MapModel(model string) (string, bool) {
	m := strings.ToLower(model)

	switch {
	case strings.Contains(m, "sonnet"):
		switch {
		case strings.Contains(m, "4-6") || strings.Contains(m, "4.6"):
			return "claude-sonnet-4.6", true
		case strings.Contains(m, "4-5") || strings.Contains(m, "4.5"):
			return "claude-sonnet-4.5", true
		default:
			return "", false
		}
	case strings.Contains(m, "opus"):
		switch {
		case strings.Contains(m, "4-5") || strings.Contains(m, "4.5"):
			return "claude-opus-4.5", true
		case strings.Contains(m, "4-6") || strings.Contains(m, "4.6"):
			return "claude-opus-4.6", true
		case strings.Contains(m, "4-7") || strings.Contains(m, "4.7"):
			return "claude-opus-4.7", true
		case strings.Contains(m, "4-8") || strings.Contains(m, "4.8"):
			return "claude-opus-4.8", true
		default:
			return "", false
		}
	case strings.Contains(m, "haiku"):
		return "claude-haiku-4.5", true
	default:
		return "", false
	}
}

// IsThinkingModel reports whether the model name carries the -thinking suffix.
func IsThinkingModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "thinking")
}

// StripThinkingSuffix removes the gateway "-thinking" suffix (case-insensitive)
// so the model name echoed back to the client is a valid Anthropic model name.
// The prefix's original case is preserved; a name without the suffix is returned
// unchanged.
func StripThinkingSuffix(model string) string {
	const suffix = "-thinking"
	if len(model) >= len(suffix) {
		tail := model[len(model)-len(suffix):]
		if strings.EqualFold(tail, suffix) {
			return model[:len(model)-len(suffix)]
		}
	}
	return model
}

// EffortCapability describes a Kiro model's reasoning-effort schema, mirroring
// the live ListAvailableModels response (management.{region}.kiro.dev). Only
// models that declare an effort schema get an additionalModelRequestFields field
// in the request — exactly as Kiro IDE does (decompile Er(): fills the field only
// when the model has effortLevel + effortSchemaPath).
type EffortCapability struct {
	SchemaPath   string          // "output_config" (all current Claude models) or "reasoning"
	DefaultLevel string          // per-model default effort (IDE defaultEffortLevel)
	Allowed      map[string]bool // legal effort enum for this model
}

// effortCapabilities is keyed by the mapped Kiro modelId (MapModel output).
// Source: live ListAvailableModels (2026-06-10, account 322). Only these 4
// Claude models support effort; all others (opus-4.5/sonnet-4.5/sonnet-4/
// haiku-4.5, non-Claude) declare no schema and get no field — matching IDE.
//   - opus-4.8:    default high,  enum low/medium/high/xhigh/max
//   - opus-4.7:    default xhigh, enum low/medium/high/xhigh/max
//   - opus-4.6:    default high,  enum low/medium/high/max   (NO xhigh)
//   - sonnet-4.6:  default high,  enum low/medium/high/max   (NO xhigh)
var effortCapabilities = map[string]EffortCapability{
	"claude-opus-4.8": {
		SchemaPath:   "output_config",
		DefaultLevel: "high",
		Allowed:      map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
	},
	"claude-opus-4.7": {
		SchemaPath:   "output_config",
		DefaultLevel: "xhigh",
		Allowed:      map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
	},
	"claude-opus-4.6": {
		SchemaPath:   "output_config",
		DefaultLevel: "high",
		Allowed:      map[string]bool{"low": true, "medium": true, "high": true, "max": true},
	},
	"claude-sonnet-4.6": {
		SchemaPath:   "output_config",
		DefaultLevel: "high",
		Allowed:      map[string]bool{"low": true, "medium": true, "high": true, "max": true},
	},
}

// EffortCapabilityFor returns the effort capability for a mapped Kiro modelId,
// and whether the model supports effort at all.
func EffortCapabilityFor(modelID string) (EffortCapability, bool) {
	c, ok := effortCapabilities[modelID]
	return c, ok
}


// ContextWindowSize returns the context window size for a model.
// Mirrors kiro.rs::get_context_window_size.
func ContextWindowSize(model string) int {
	mapped, ok := MapModel(model)
	if !ok {
		return 200_000
	}
	switch mapped {
	case "claude-sonnet-4.6", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8":
		return 1_000_000
	default:
		return 200_000
	}
}

// ModelInfo describes a model exposed via /v1/models.
type ModelInfo struct {
	ID          string
	DisplayName string
	MaxTokens   int
}

// SupportedModels lists all models the Kiro provider exposes.
// Mirrors the list in kiro.rs src/anthropic/handlers.rs.
func SupportedModels() []ModelInfo {
	return []ModelInfo{
		{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", MaxTokens: 128_000},
		{ID: "claude-opus-4-8-thinking", DisplayName: "Claude Opus 4.8 (Thinking)", MaxTokens: 128_000},
		{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7", MaxTokens: 64_000},
		{ID: "claude-opus-4-7-thinking", DisplayName: "Claude Opus 4.7 (Thinking)", MaxTokens: 64_000},
		{ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6", MaxTokens: 64_000},
		{ID: "claude-opus-4-6-thinking", DisplayName: "Claude Opus 4.6 (Thinking)", MaxTokens: 64_000},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", MaxTokens: 64_000},
		{ID: "claude-sonnet-4-6-thinking", DisplayName: "Claude Sonnet 4.6 (Thinking)", MaxTokens: 64_000},
		{ID: "claude-opus-4-5-20251101", DisplayName: "Claude Opus 4.5", MaxTokens: 64_000},
		{ID: "claude-opus-4-5-20251101-thinking", DisplayName: "Claude Opus 4.5 (Thinking)", MaxTokens: 64_000},
		{ID: "claude-sonnet-4-5-20250929", DisplayName: "Claude Sonnet 4.5", MaxTokens: 64_000},
		{ID: "claude-sonnet-4-5-20250929-thinking", DisplayName: "Claude Sonnet 4.5 (Thinking)", MaxTokens: 64_000},
		{ID: "claude-haiku-4-5-20251001", DisplayName: "Claude Haiku 4.5", MaxTokens: 64_000},
		{ID: "claude-haiku-4-5-20251001-thinking", DisplayName: "Claude Haiku 4.5 (Thinking)", MaxTokens: 64_000},
	}
}
