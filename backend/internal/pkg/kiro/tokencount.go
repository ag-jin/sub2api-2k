package kiro

import (
	"encoding/json"
)

// isNonWestern reports whether r is a non-Western character (CJK, Arabic, etc.).
// Mirrors kiro.rs token.rs::is_non_western_char.
func isNonWestern(r rune) bool {
	switch {
	case r >= 0x0000 && r <= 0x007F: // ASCII
		return false
	case r >= 0x0080 && r <= 0x00FF: // Latin-1
		return false
	case r >= 0x0100 && r <= 0x024F: // Latin Extended-A/B
		return false
	case r >= 0x1E00 && r <= 0x1EFF: // Latin Extended Additional
		return false
	case r >= 0x2C60 && r <= 0x2C7F: // Latin Extended-C
		return false
	case r >= 0xA720 && r <= 0xA7FF: // Latin Extended-D
		return false
	case r >= 0xAB30 && r <= 0xAB6F: // Latin Extended-E
		return false
	default:
		return true
	}
}

// CountTextTokens estimates the token count for a single string.
// Mirrors kiro.rs token.rs::count_tokens (char-unit model + range scaling).
func CountTextTokens(text string) int {
	var charUnits float64
	for _, r := range text {
		if isNonWestern(r) {
			charUnits += 4.0
		} else {
			charUnits += 1.0
		}
	}
	tokens := charUnits / 4.0

	var acc float64
	switch {
	case tokens < 100.0:
		acc = tokens * 1.5
	case tokens < 200.0:
		acc = tokens * 1.3
	case tokens < 300.0:
		acc = tokens * 1.25
	case tokens < 800.0:
		acc = tokens * 1.2
	default:
		acc = tokens * 1.0
	}
	return int(acc)
}

// CountTokensRequestBody is the subset of an Anthropic count_tokens request we read.
type CountTokensRequestBody struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system,omitempty"`
	Messages []AnthropicMsg  `json:"messages"`
	Tools    []AnthropicTool `json:"tools,omitempty"`
}

// CountInputTokens computes a local estimate of input tokens for a request body.
// Mirrors kiro.rs token.rs::count_all_tokens_local.
func CountInputTokens(body []byte) int {
	var req CountTokensRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		return 1
	}
	total := 0

	// System (string or array of {text})
	if len(req.System) > 0 {
		total += CountTextTokens(systemText(req.System))
	}

	// Messages
	for _, m := range req.Messages {
		total += countContentTokens(m.Content)
	}

	// Tools
	for _, t := range req.Tools {
		total += CountTextTokens(t.Name)
		total += CountTextTokens(t.Description)
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				total += CountTextTokens(string(b))
			}
		}
	}

	if total < 1 {
		total = 1
	}
	return total
}

// countContentTokens handles message content as plain string or block array.
func countContentTokens(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return CountTextTokens(s)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		total := 0
		for _, b := range blocks {
			if b.Text != "" {
				total += CountTextTokens(b.Text)
			}
		}
		return total
	}
	return 0
}
