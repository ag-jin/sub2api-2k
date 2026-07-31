package opencode

import (
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// BuildUpstreamRequest converts an Anthropic Messages request body into the
// OpenAI Chat Completions request body to send to OpenCode Go's
// /chat/completions upstream.
//
// Chain: AnthropicRequest → ResponsesRequest → ChatCompletionsRequest, reusing
// the shared apicompat converters (the Responses→ChatCompletions path was built
// for /chat/completions-only upstreams). The intermediate Responses request's
// OpenAI-Responses-only fields (store, include, verbosity) are NOT carried into
// ChatCompletionsRequest, so the upstream body stays clean.
//
// Unlike the deepseek path, OpenCode Go does NOT accept a non-standard
// `thinking` object; reasoning_effort (already populated by the apicompat chain
// from Anthropic output_config.effort) is the only thinking signal forwarded.
//
// Returns the JSON bytes, the resolved upstream model id, and whether the
// request asked for streaming. Callers must route via UsesMessagesProtocol
// before calling this — Messages-protocol models should be forwarded as-is,
// not converted.
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

	// OpenAI Chat Completions API uses "max" as the highest reasoning effort,
	// while the Responses API uses "xhigh". The apicompat chain maps
	// Anthropic "max" → Responses "xhigh"; convert it back for CC upstreams
	// (GLM, opencode.ai) which expect "max".
	if ccReq.ReasoningEffort == "xhigh" {
		ccReq.ReasoningEffort = "max"
	}

	out, err := json.Marshal(ccReq)
	if err != nil {
		return nil, "", false, fmt.Errorf("marshal chatcompletions: %w", err)
	}
	return out, upstreamModel, areq.Stream, nil
}
