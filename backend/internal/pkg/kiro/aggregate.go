package kiro

import "encoding/json"

// AggregatingWriter implements SSEWriter but instead of emitting SSE frames it
// accumulates content blocks so a non-streaming Anthropic JSON response can be
// built. Mirrors kiro.rs handle_non_stream_request aggregation.
type AggregatingWriter struct {
	blocks      []map[string]interface{} // ordered content blocks by index
	blockIndex  map[int]int              // sse index -> position in blocks
	toolInputs  map[int]string           // sse index -> accumulated partial_json
	stopReason  string
	inputTokens int
}

// NewAggregatingWriter creates an empty aggregator.
func NewAggregatingWriter() *AggregatingWriter {
	return &AggregatingWriter{
		blockIndex: map[int]int{},
		toolInputs: map[int]string{},
		stopReason: "end_turn",
	}
}

func (a *AggregatingWriter) WriteSSE(event string, data map[string]interface{}) error {
	switch event {
	case "message_start":
		if msg, ok := data["message"].(map[string]interface{}); ok {
			if usage, ok := msg["usage"].(map[string]interface{}); ok {
				if it, ok := usage["input_tokens"].(int); ok {
					a.inputTokens = it
				}
			}
		}
	case "content_block_start":
		idx := toInt(data["index"])
		cb, _ := data["content_block"].(map[string]interface{})
		blk := map[string]interface{}{}
		for k, v := range cb {
			blk[k] = v
		}
		a.blockIndex[idx] = len(a.blocks)
		a.blocks = append(a.blocks, blk)
	case "content_block_delta":
		idx := toInt(data["index"])
		pos, ok := a.blockIndex[idx]
		if !ok {
			return nil
		}
		d, _ := data["delta"].(map[string]interface{})
		switch d["type"] {
		case "text_delta":
			if t, ok := d["text"].(string); ok {
				prev, _ := a.blocks[pos]["text"].(string)
				a.blocks[pos]["text"] = prev + t
			}
		case "thinking_delta":
			if t, ok := d["thinking"].(string); ok {
				prev, _ := a.blocks[pos]["thinking"].(string)
				a.blocks[pos]["thinking"] = prev + t
			}
		case "input_json_delta":
			if pj, ok := d["partial_json"].(string); ok {
				a.toolInputs[idx] += pj
			}
		}
	case "content_block_stop":
		idx := toInt(data["index"])
		pos, ok := a.blockIndex[idx]
		if !ok {
			return nil
		}
		// finalize tool_use input JSON
		if a.blocks[pos]["type"] == "tool_use" {
			raw := a.toolInputs[idx]
			var input interface{}
			if raw == "" {
				input = map[string]interface{}{}
			} else if json.Unmarshal([]byte(raw), &input) != nil {
				input = map[string]interface{}{}
			}
			a.blocks[pos]["input"] = input
		}
	case "message_delta":
		if d, ok := data["delta"].(map[string]interface{}); ok {
			if sr, ok := d["stop_reason"].(string); ok && sr != "" {
				a.stopReason = sr
			}
		}
	}
	return nil
}

// BuildResponse assembles the final non-streaming Anthropic message JSON.
func (a *AggregatingWriter) BuildResponse(messageID, model string, outputTokens int) map[string]interface{} {
	content := a.blocks
	if content == nil {
		content = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   a.stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  a.inputTokens,
			"output_tokens": outputTokens,
		},
	}
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
