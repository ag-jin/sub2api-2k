package deepseek

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

type captureSSEWriter struct {
	strings.Builder
	flushes int
}

func (w *captureSSEWriter) Flush() {
	w.flushes++
}

func convertStreamEvents(t *testing.T, chunks ...string) ([]apicompat.AnthropicStreamEvent, StreamUsage) {
	t.Helper()
	var input strings.Builder
	for _, chunk := range chunks {
		input.WriteString("data: ")
		input.WriteString(chunk)
		input.WriteString("\n\n")
	}
	input.WriteString("data: [DONE]\n\n")

	var out captureSSEWriter
	usage, err := ConvertStream(strings.NewReader(input.String()), &out, "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	return parseAnthropicSSEEvents(t, out.String()), usage
}

func parseAnthropicSSEEvents(t *testing.T, sse string) []apicompat.AnthropicStreamEvent {
	t.Helper()
	var events []apicompat.AnthropicStreamEvent
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatalf("unmarshal SSE data %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func requireSingleTerminalEvents(t *testing.T, events []apicompat.AnthropicStreamEvent) apicompat.AnthropicStreamEvent {
	t.Helper()
	var deltas []apicompat.AnthropicStreamEvent
	var stops int
	for _, event := range events {
		switch event.Type {
		case "message_delta":
			deltas = append(deltas, event)
		case "message_stop":
			stops++
		}
	}
	if len(deltas) != 1 {
		t.Fatalf("message_delta count=%d want 1; events=%+v", len(deltas), events)
	}
	if stops != 1 {
		t.Fatalf("message_stop count=%d want 1; events=%+v", stops, events)
	}
	return deltas[0]
}

func TestConvertStreamIncludesUsageFromFinalChatCompletionsEvent(t *testing.T) {
	events, usage := convertStreamEvents(t,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":18,"completion_tokens":80,"total_tokens":98}}`,
	)

	if usage.InputTokens != 18 || usage.OutputTokens != 80 {
		t.Fatalf("returned usage=%+v want input=18 output=80", usage)
	}
	delta := requireSingleTerminalEvents(t, events)
	if delta.Usage == nil {
		t.Fatalf("message_delta usage is nil")
	}
	if delta.Usage.InputTokens != 18 || delta.Usage.OutputTokens != 80 {
		t.Fatalf("message_delta usage=%+v want input=18 output=80", delta.Usage)
	}
}

func TestConvertStreamTextTerminalEventsAreNotDuplicated(t *testing.T) {
	events, _ := convertStreamEvents(t,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	)

	delta := requireSingleTerminalEvents(t, events)
	if delta.Delta == nil || delta.Delta.StopReason != "end_turn" {
		t.Fatalf("message_delta stop_reason=%+v want end_turn", delta.Delta)
	}
}

func TestConvertStreamToolTerminalEventsAreNotDuplicated(t *testing.T) {
	events, _ := convertStreamEvents(t,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
	)

	delta := requireSingleTerminalEvents(t, events)
	if delta.Delta == nil || delta.Delta.StopReason != "tool_use" {
		t.Fatalf("message_delta stop_reason=%+v want tool_use", delta.Delta)
	}
}
