package kiro

import (
	"bytes"
	"testing"
)

// captureWriter records SSE events for assertions.
type captureWriter struct {
	events []captured
}

type captured struct {
	name string
	data map[string]interface{}
}

func (w *captureWriter) WriteSSE(event string, data map[string]interface{}) error {
	w.events = append(w.events, captured{name: event, data: data})
	return nil
}

func (w *captureWriter) names() []string {
	out := make([]string, len(w.events))
	for i, e := range w.events {
		out[i] = e.name
	}
	return out
}

func assistantFrame(content string) []byte {
	h := map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}
	payload := []byte(`{"content":` + jsonString(content) + `}`)
	return buildTestFrame(h, payload)
}

func toolFrame(name, id, input string, stop bool) []byte {
	h := map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}
	stopStr := "false"
	if stop {
		stopStr = "true"
	}
	payload := []byte(`{"name":` + jsonString(name) + `,"toolUseId":` + jsonString(id) +
		`,"input":` + jsonString(input) + `,"stop":` + stopStr + `}`)
	return buildTestFrame(h, payload)
}

func jsonString(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestStream_SimpleText(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(assistantFrame("Hello"))
	buf.Write(assistantFrame(" world"))

	w := &captureWriter{}
	c := NewStreamConverter(w, "claude-opus-4.8", "msg_1", false)
	if err := c.Run(&buf); err != nil {
		t.Fatalf("Run: %v", err)
	}

	names := w.names()
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(names) != len(want) {
		t.Fatalf("event sequence = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("event[%d] = %s, want %s", i, names[i], want[i])
		}
	}
}

func TestStream_ToolUse(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(assistantFrame("Let me check"))
	buf.Write(toolFrame("get_weather", "tool_1", `{"city":`, false))
	buf.Write(toolFrame("get_weather", "tool_1", `"NYC"}`, true))

	w := &captureWriter{}
	c := NewStreamConverter(w, "claude-opus-4.8", "msg_1", false)
	if err := c.Run(&buf); err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundToolStart := false
	foundInputDelta := false
	for _, e := range w.events {
		if e.name == "content_block_start" {
			if cb, ok := e.data["content_block"].(map[string]interface{}); ok {
				if cb["type"] == "tool_use" && cb["name"] == "get_weather" {
					foundToolStart = true
				}
			}
		}
		if e.name == "content_block_delta" {
			if d, ok := e.data["delta"].(map[string]interface{}); ok {
				if d["type"] == "input_json_delta" {
					foundInputDelta = true
				}
			}
		}
	}
	if !foundToolStart {
		t.Error("missing tool_use content_block_start")
	}
	if !foundInputDelta {
		t.Error("missing input_json_delta")
	}
	if c.stopReason != "tool_use" {
		t.Errorf("stopReason = %q, want tool_use", c.stopReason)
	}
}

func TestStream_Thinking(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(assistantFrame("<thinking>Let me reason</thinking>"))
	buf.Write(assistantFrame("Final answer"))

	w := &captureWriter{}
	c := NewStreamConverter(w, "claude-opus-4.8", "msg_1", true)
	if err := c.Run(&buf); err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundThinkingStart := false
	foundThinkingDelta := false
	foundTextDelta := false
	for _, e := range w.events {
		if e.name == "content_block_start" {
			if cb, ok := e.data["content_block"].(map[string]interface{}); ok {
				if cb["type"] == "thinking" {
					foundThinkingStart = true
				}
			}
		}
		if e.name == "content_block_delta" {
			if d, ok := e.data["delta"].(map[string]interface{}); ok {
				if d["type"] == "thinking_delta" {
					foundThinkingDelta = true
				}
				if d["type"] == "text_delta" {
					foundTextDelta = true
				}
			}
		}
	}
	if !foundThinkingStart {
		t.Error("missing thinking content_block_start")
	}
	if !foundThinkingDelta {
		t.Error("missing thinking_delta")
	}
	if !foundTextDelta {
		t.Error("missing text_delta after thinking")
	}
}

func TestStream_ContextUsage(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(assistantFrame("hi"))
	h := map[string]string{":message-type": "event", ":event-type": "contextUsageEvent"}
	buf.Write(buildTestFrame(h, []byte(`{"contextUsagePercentage":1.5}`)))

	w := &captureWriter{}
	c := NewStreamConverter(w, "claude-opus-4.8", "msg_1", false)
	if err := c.Run(&buf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.InputTokens() != 15000 {
		t.Errorf("InputTokens = %d, want 15000", c.InputTokens())
	}
}
