package deepseek

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// BuildUpstreamRequest converts an Anthropic Messages request body into the
// DeepSeek (OpenAI Chat Completions) request body to send upstream.
//
// Chain: AnthropicRequest → ResponsesRequest → ChatCompletionsRequest, reusing
// the shared apicompat converters (the Responses→ChatCompletions path was built
// for exactly this kind of /chat/completions-only upstream — see
// responses_to_chatcompletions normalize-for-DeepSeek note). The intermediate
// Responses request's OpenAI-Responses-only fields (store, include, verbosity)
// are NOT carried into ChatCompletionsRequest, so the upstream body stays clean.
//
// DeepSeek thinking control (api-docs.deepseek.com/guides/thinking_mode):
//   - reasoning_effort: top-level string (high/max; low/medium→high, xhigh→max
//     are collapsed by DeepSeek itself). Already populated by the apicompat chain
//     from Anthropic output_config.effort, defaulting to medium.
//   - thinking: {type: enabled|disabled} — defaults to enabled upstream. We make
//     the toggle explicit so a client that omits thinking still gets DeepSeek's
//     default thinking behavior, and a client that disables it is honored.
//
// Returns the JSON bytes, the resolved upstream model id, and whether the
// request asked for streaming.
func BuildUpstreamRequest(anthropicBody []byte) (body []byte, model string, stream bool, err error) {
	var areq apicompat.AnthropicRequest
	if err = json.Unmarshal(anthropicBody, &areq); err != nil {
		return nil, "", false, fmt.Errorf("parse anthropic request: %w", err)
	}

	upstreamModel := MapModel(areq.Model)

	respReq, err := apicompat.AnthropicToResponses(&areq)
	if err != nil {
		return nil, "", false, fmt.Errorf("anthropic→responses: %w", err)
	}
	ccReq, err := apicompat.ResponsesToChatCompletionsRequest(respReq)
	if err != nil {
		return nil, "", false, fmt.Errorf("responses→chatcompletions: %w", err)
	}
	ccReq.Model = upstreamModel
	ccReq.Stream = areq.Stream

	// Marshal the standard Chat Completions request, then inject DeepSeek's
	// thinking toggle (not a standard Chat Completions field, so it has no struct
	// slot). resolveThinkingType derives enabled/disabled from the Anthropic
	// request; reasoning_effort is already on ccReq from the chain.
	out, err := injectDeepseekFields(ccReq, resolveThinkingType(&areq))
	if err != nil {
		return nil, "", false, err
	}
	return out, upstreamModel, areq.Stream, nil
}

// resolveThinkingType maps the Anthropic request to DeepSeek's thinking.type.
// Anthropic thinking.type "disabled" → disabled; everything else (enabled,
// adaptive, or unset) → enabled, matching DeepSeek's default-on behavior and the
// user requirement "default use thinking".
func resolveThinkingType(areq *apicompat.AnthropicRequest) string {
	if areq.Thinking != nil && strings.EqualFold(areq.Thinking.Type, "disabled") {
		return "disabled"
	}
	return "enabled"
}

// injectDeepseekFields marshals the Chat Completions request and adds the
// non-standard `thinking` object DeepSeek understands. Done via a map patch so
// we don't have to fork apicompat's ChatCompletionsRequest type.
func injectDeepseekFields(ccReq *apicompat.ChatCompletionsRequest, thinkingType string) ([]byte, error) {
	raw, err := json.Marshal(ccReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chatcompletions: %w", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("remarshal chatcompletions: %w", err)
	}
	thinking, _ := json.Marshal(map[string]string{"type": thinkingType})
	obj["thinking"] = thinking
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream body: %w", err)
	}
	return out, nil
}
