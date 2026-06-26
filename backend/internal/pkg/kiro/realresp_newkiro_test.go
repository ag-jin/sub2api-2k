package kiro

import (
	"os"
	"strings"
	"testing"
)

// TestParseRealNewKiroResponse feeds REAL captured Kiro 0.12.301 event-stream
// responses (runtime.kiro.dev) through the stream parser to prove it handles
// the new framing (incl. modelId field + meteringEvent) end-to-end.
func TestParseRealNewKiroResponse(t *testing.T) {
	cases := []struct {
		path      string
		wantTool  string // expected tool name if any
		wantInBody string // substring expected in aggregated text
	}{
		{"/tmp/real_chat_text.bin", "", "三件事"},
		{"/tmp/real_chat_tool.bin", "remote_web_search", ""},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(tc.path)
		if err != nil {
			t.Skipf("missing capture %s (run capture first): %v", tc.path, err)
			continue
		}
		agg := NewAggregatingWriter()
		conv := NewStreamConverter(agg, "claude-opus-4-6", "msg_test", false)
		if err := conv.Run(strings.NewReader(string(data))); err != nil {
			t.Fatalf("%s: parser Run failed: %v", tc.path, err)
		}
		resp := agg.BuildResponse("msg_test", "claude-opus-4-6", conv.OutputTokens())
		blocks, _ := resp["content"].([]map[string]interface{})
		var text strings.Builder
		var tools []string
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				if s, ok := b["text"].(string); ok {
					text.WriteString(s)
				}
			case "tool_use":
				if n, ok := b["name"].(string); ok {
					tools = append(tools, n)
				}
			}
		}
		body := text.String()
		t.Logf("%s -> blocks=%d text_len=%d tools=%v inputTokens=%d stop=%v",
			tc.path, len(blocks), len(body), tools, conv.InputTokens(), resp["stop_reason"])
		if tc.wantInBody != "" && !strings.Contains(body, tc.wantInBody) {
			t.Errorf("%s: expected text to contain %q, got: %.120s", tc.path, tc.wantInBody, body)
		}
		if tc.wantTool != "" {
			found := false
			for _, n := range tools {
				if n == tc.wantTool {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: expected tool %q, got %v", tc.path, tc.wantTool, tools)
			}
		}
	}
}
