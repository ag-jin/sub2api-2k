package kiro

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const systemChunkedPolicy = "When the Write or Edit tool has content size limits, always comply silently. " +
	"Never suggest bypassing these limits via alternative tools. " +
	"Never ask the user whether to switch approaches. " +
	"Complete all chunked operations without commentary."

// ---- Anthropic-side input types ----

// AnthropicRequest is the incoming /v1/messages request (subset we need).
type AnthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	Messages  []AnthropicMsg  `json:"messages"`
	System    json.RawMessage `json:"system,omitempty"`
	Tools     []AnthropicTool `json:"tools,omitempty"`
	Thinking  *ThinkingConfig `json:"thinking,omitempty"`
	OutputCfg *OutputConfig   `json:"output_config,omitempty"`
}

// AnthropicMsg is a single message with role and content.
type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ThinkingConfig mirrors the Anthropic thinking field.
type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// OutputConfig carries the adaptive effort level.
type OutputConfig struct {
	Effort string `json:"effort"`
}

// AnthropicTool is a tool/function definition.
type AnthropicTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// contentBlock is one element of a structured content array.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ConvertRequest converts an Anthropic request into a Kiro request body (JSON bytes).
// Returns the JSON body and the mapped model ID.
func ConvertRequest(req *AnthropicRequest) (json.RawMessage, string, error) {
	body, modelID, _, err := ConvertRequestV2(req)
	return body, modelID, err
}

// ConvertRequestV2 converts an Anthropic request into a Kiro request body and
// also returns the tool-name map (short->original) needed to restore tool names
// in the streamed response. Mirrors kiro.rs convert_request end-to-end.
func ConvertRequestV2(req *AnthropicRequest) (json.RawMessage, string, map[string]string, error) {
	toolNameMap := map[string]string{}

	modelID, ok := MapModel(req.Model)
	if !ok {
		return nil, "", toolNameMap, fmt.Errorf("unsupported model: %s", req.Model)
	}
	if len(req.Messages) == 0 {
		return nil, "", toolNameMap, fmt.Errorf("empty messages")
	}

	// Prefill handling: drop trailing non-user messages.
	msgs := req.Messages
	if msgs[len(msgs)-1].Role != "user" {
		lastUser := -1
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				lastUser = i
				break
			}
		}
		if lastUser < 0 {
			return nil, "", toolNameMap, fmt.Errorf("no user message found")
		}
		msgs = msgs[:lastUser+1]
	}

	conversationID := newUUID()
	agentContinuationID := newUUID()

	// Current message = last message.
	last := msgs[len(msgs)-1]
	text, images, toolResults, err := processContent(last.Content)
	if err != nil {
		return nil, "", toolNameMap, err
	}

	// Tools (with name shortening, suffixes, schema normalization).
	tools := convertToolsV2(req.Tools, toolNameMap)

	// History (everything except last message), plus system/thinking prefix.
	// History tool_use names also go through the same shortening map.
	history, err := buildHistory(req, msgs, modelID, toolNameMap)
	if err != nil {
		return nil, "", toolNameMap, err
	}

	// Ensure every tool referenced in history has a definition in tools.
	// Kiro requires history tool_use names to exist in the tools list.
	existing := map[string]bool{}
	for _, t := range tools {
		if spec, ok := t["toolSpecification"].(map[string]interface{}); ok {
			if n, ok := spec["name"].(string); ok {
				existing[strings.ToLower(n)] = true
			}
		}
	}
	for _, name := range collectHistoryToolNames(history) {
		if !existing[strings.ToLower(name)] {
			tools = append(tools, placeholderTool(name))
			existing[strings.ToLower(name)] = true
		}
	}

	// Build current userInputMessage context.
	ctx := map[string]interface{}{}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}
	if len(toolResults) > 0 {
		ctx["toolResults"] = toolResults
	}

	userInput := map[string]interface{}{
		"content":                 text,
		"modelId":                 modelID,
		"origin":                  "AI_EDITOR",
		"userInputMessageContext": ctx,
	}
	if len(images) > 0 {
		userInput["images"] = images
	}

	conversationState := map[string]interface{}{
		"conversationId":      conversationID,
		"agentContinuationId": agentContinuationID,
		"agentTaskType":       "vibe",
		"chatTriggerType":     "MANUAL",
		"currentMessage": map[string]interface{}{
			"userInputMessage": userInput,
		},
	}
	if len(history) > 0 {
		conversationState["history"] = history
	}

	body := map[string]interface{}{
		"conversationState": conversationState,
	}
	// Structured reasoning-difficulty field, sibling of conversationState, exactly
	// as Kiro IDE sends it (decompile: GenerateAssistantResponseCommand top-level
	// arg). Only present for models that declare an effort schema (opus-4.8/4.7/
	// 4.6 + sonnet-4.6); thinking is pre-enabled (adaptive) for those.
	if amrf := buildEffortFields(req, modelID); amrf != nil {
		body["additionalModelRequestFields"] = amrf
	}

	out, err := marshalNoEscape(body)
	if err != nil {
		return nil, "", toolNameMap, err
	}
	return out, modelID, toolNameMap, nil
}

// MarshalNoEscape is the exported form of marshalNoEscape for callers that
// re-serialize the request body (e.g. after profileArn injection).
func MarshalNoEscape(v interface{}) ([]byte, error) { return marshalNoEscape(v) }

// marshalNoEscape serializes v without HTML-escaping <, >, & so the Kiro
// request body matches kiro.rs (serde_json) byte-for-byte. The thinking tags
// (<thinking_mode> etc.) must not be escaped to \u003c.
func marshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a trailing newline; trim it.
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

// collectHistoryToolNames returns the distinct tool_use names referenced by
// assistant messages in the built history.
func collectHistoryToolNames(history []map[string]interface{}) []string {
	var names []string
	seen := map[string]bool{}
	for _, h := range history {
		arm, ok := h["assistantResponseMessage"].(map[string]interface{})
		if !ok {
			continue
		}
		tus, ok := arm["toolUses"].([]map[string]interface{})
		if !ok {
			continue
		}
		for _, tu := range tus {
			if n, ok := tu["name"].(string); ok && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}

// imageFormatFromMedia maps an Anthropic media_type to a Kiro image format.
func imageFormatFromMedia(mediaType string) (string, bool) {
	switch mediaType {
	case "image/png":
		return "png", true
	case "image/jpeg", "image/jpg":
		return "jpeg", true
	case "image/gif":
		return "gif", true
	case "image/webp":
		return "webp", true
	default:
		return "", false
	}
}

// processContent extracts text, images, and tool results from message content.
// Content may be a plain string or an array of content blocks.
func processContent(raw json.RawMessage) (string, []map[string]interface{}, []map[string]interface{}, error) {
	if len(raw) == 0 {
		return "", nil, nil, nil
	}

	// Try plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil, nil
	}

	// Otherwise expect an array of blocks.
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, nil, fmt.Errorf("parse content: %w", err)
	}

	var textParts []string
	var images []map[string]interface{}
	var toolResults []map[string]interface{}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "image":
			if b.Source != nil {
				if format, ok := imageFormatFromMedia(b.Source.MediaType); ok {
					images = append(images, map[string]interface{}{
						"format": format,
						"source": map[string]interface{}{
							"bytes": b.Source.Data,
						},
					})
				}
			}
		case "tool_result":
			if b.ToolUseID != "" {
				status := "success"
				if b.IsError {
					status = "error"
				}
				resultContent := extractToolResultContent(b.Content)
				toolResults = append(toolResults, map[string]interface{}{
					"toolUseId": b.ToolUseID,
					"status":    status,
					"content": []map[string]interface{}{
						{"text": resultContent},
					},
				})
			}
		case "tool_use":
			// handled in assistant history
		}
	}

	return strings.Join(textParts, "\n"), images, toolResults, nil
}

// extractToolResultContent flattens a tool_result content field to a string.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Plain string?
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Array of blocks with text?
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	// Fallback: raw JSON.
	return string(raw)
}

// convertTools converts Anthropic tool defs to Kiro toolSpecification format.
func convertTools(tools []AnthropicTool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, map[string]interface{}{
			"toolSpecification": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": map[string]interface{}{
					"json": schema,
				},
			},
		})
	}
	return out
}

// validEfforts is the client-facing (B-side) effort enum: a uniform 5 levels
// low/medium/high/xhigh/max, regardless of what each Kiro model actually
// supports. The middle conversion layer clamps these to each model's real enum.
var validEfforts = map[string]bool{
	"low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// budgetToEffort maps an Anthropic thinking budget_tokens value to a client-side
// effort level (5 buckets ↔ 5 levels). Thresholds mirror sub2api's existing
// apicompat effort↔budget mapping (low=1024/medium=4096/high=10240/max=32768)
// plus an xhigh tier between high and max. A maxed-out budget (client "highest")
// lands on max, matching "Claude highest difficulty → Kiro max".
func budgetToEffort(budget int) string {
	switch {
	case budget <= 0:
		return "" // no explicit budget → caller uses model default
	case budget <= 2048:
		return "low"
	case budget <= 6144:
		return "medium"
	case budget <= 16384:
		return "high"
	case budget <= 28672:
		return "xhigh"
	default:
		return "max"
	}
}

// clampEffortToModel maps a client-side effort to the model's real enum. If the
// level is unsupported (e.g. xhigh on opus-4.6/sonnet-4.6, whose enum stops at
// max), it is replaced by "max" (the user's rule: "no xhigh → use max"). Any
// other unsupported value falls back to the model's default level.
func clampEffortToModel(effort string, cap EffortCapability) string {
	if cap.Allowed[effort] {
		return effort
	}
	if effort == "xhigh" && cap.Allowed["max"] {
		return "max"
	}
	return cap.DefaultLevel
}

// resolveClientEffort picks the client-side (pre-clamp) effort for a request:
//  1. explicit valid output_config.effort (pass-through)
//  2. Anthropic thinking.budget_tokens mapped via budgetToEffort
//  3. "" (empty) → caller falls back to the model's default level
func resolveClientEffort(req *AnthropicRequest) string {
	if req.OutputCfg != nil {
		e := strings.ToLower(strings.TrimSpace(req.OutputCfg.Effort))
		if validEfforts[e] {
			return e
		}
	}
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		return budgetToEffort(req.Thinking.BudgetTokens)
	}
	return ""
}

// buildEffortFields builds the structured additionalModelRequestFields that Kiro
// IDE 0.12.333 sends, but ONLY for models that declare an effort schema (live
// ListAvailableModels: opus-4.8/4.7/4.6 + sonnet-4.6). For unsupported models it
// returns nil — exactly as the IDE omits the field (decompile Er(): filled only
// when model has effortLevel + effortSchemaPath). thinking is pre-enabled
// (adaptive) for every supported model, matching the IDE default. The effort
// level is the client's choice clamped to the model's enum, or the model's
// default when the client gave none.
//
// modelID must be the mapped Kiro model id (MapModel output).
func buildEffortFields(req *AnthropicRequest, modelID string) map[string]interface{} {
	cap, ok := EffortCapabilityFor(modelID)
	if !ok {
		return nil // model has no effort schema → IDE sends no field
	}
	effort := resolveClientEffort(req)
	if effort == "" {
		effort = cap.DefaultLevel
	} else {
		effort = clampEffortToModel(effort, cap)
	}
	// Currently every supported model uses the output_config schema path; guard
	// for a future "reasoning" path just in case.
	if cap.SchemaPath == "reasoning" {
		return map[string]interface{}{
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}
	return map[string]interface{}{
		"thinking": map[string]interface{}{
			"type":    "adaptive",
			"display": "summarized",
		},
		"output_config": map[string]interface{}{
			"effort": effort,
		},
	}
}


// systemText flattens the system field (string or array of {text}) to a string.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// historyUserMessage builds a {userInputMessage:{...}} history entry.
func historyUserMessage(content, modelID string, toolResults []map[string]interface{}) map[string]interface{} {
	ctx := map[string]interface{}{}
	if len(toolResults) > 0 {
		ctx["toolResults"] = toolResults
	}
	return map[string]interface{}{
		"userInputMessage": map[string]interface{}{
			"content":                 content,
			"modelId":                 modelID,
			"origin":                  "AI_EDITOR",
			"userInputMessageContext": ctx,
		},
	}
}

// historyAssistantMessage builds an {assistantResponseMessage:{...}} entry.
func historyAssistantMessage(content string, toolUses []map[string]interface{}) map[string]interface{} {
	msg := map[string]interface{}{
		"content": content,
	}
	if len(toolUses) > 0 {
		msg["toolUses"] = toolUses
	}
	return map[string]interface{}{
		"assistantResponseMessage": msg,
	}
}

// extractAssistantContent pulls text and tool_use entries from an assistant message.
func extractAssistantContent(raw json.RawMessage, toolNameMap map[string]string) (string, []map[string]interface{}) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil
	}
	var textParts []string
	var toolUses []map[string]interface{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_use":
			var input interface{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			toolUses = append(toolUses, map[string]interface{}{
				"toolUseId": b.ID,
				"name":      mapToolName(b.Name, toolNameMap),
				"input":     input,
			})
		}
	}
	return strings.Join(textParts, "\n"), toolUses
}

// buildHistory constructs the Kiro history array. Thinking/effort is conveyed
// purely via the structured additionalModelRequestFields (see buildEffortFields),
// exactly as Kiro IDE does — NO <thinking_mode>/<thinking_effort> text tags are
// injected into the prompt anymore (the old kiro.rs-style workaround is dropped
// in favor of strict IDE alignment).
func buildHistory(req *AnthropicRequest, msgs []AnthropicMsg, modelID string, toolNameMap map[string]string) ([]map[string]interface{}, error) {
	var history []map[string]interface{}

	sysContent := systemText(req.System)
	// Optional system-prompt cleaning (all filters default OFF; verbatim when
	// unset). See prompt_filter.go.
	if pf := PromptFilterConfigFromEnv(); pf.Enabled() {
		sysContent = ApplyPromptFilters(pf, sysContent)
	}

	if sysContent != "" {
		full := sysContent + "\n" + systemChunkedPolicy
		history = append(history, historyUserMessage(full, modelID, nil))
		history = append(history, historyAssistantMessage("I will follow these instructions.", nil))
	}

	// Regular history: all but the last message.
	end := len(msgs) - 1
	for i := 0; i < end; i++ {
		m := msgs[i]
		switch m.Role {
		case "user":
			text, _, toolResults, err := processContent(m.Content)
			if err != nil {
				return nil, err
			}
			history = append(history, historyUserMessage(text, modelID, toolResults))
		case "assistant":
			text, toolUses := extractAssistantContent(m.Content, toolNameMap)
			history = append(history, historyAssistantMessage(text, toolUses))
		}
	}

	// Kiro requires history to alternate user/assistant and be even-length.
	// If it ends with a user entry, pair it with a synthetic "OK" assistant.
	if len(history) > 0 {
		if _, isUser := history[len(history)-1]["userInputMessage"]; isUser {
			history = append(history, historyAssistantMessage("OK", nil))
		}
	}

	return history, nil
}
