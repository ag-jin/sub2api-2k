package kiro

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShortenToolName_LongName(t *testing.T) {
	long := strings.Repeat("a", 80) // > 63
	short := shortenToolName(long)
	if len(short) > toolNameMaxLen {
		t.Errorf("shortened name length %d > %d", len(short), toolNameMaxLen)
	}
	// deterministic
	if shortenToolName(long) != short {
		t.Error("shortenToolName not deterministic")
	}
}

func TestShortenToolName_ShortNameUnchanged(t *testing.T) {
	if shortenToolName("Write") != "Write" {
		t.Error("short name should be unchanged")
	}
}

func TestMapToolName_Roundtrip(t *testing.T) {
	m := map[string]string{}
	long := "mcp__some_very_long_server_name__some_very_long_tool_name_exceeding_limit"
	short := mapToolName(long, m)
	if short == long {
		t.Fatal("expected shortening")
	}
	if m[short] != long {
		t.Errorf("map should record short->original: %v", m)
	}
	// reverse via StreamConverter
	sc := NewStreamConverter(&wsCapture{}, "claude-opus-4.8", "msg", false)
	sc.SetToolNameMap(m)
	if got := sc.restoreToolName(short); got != long {
		t.Errorf("restoreToolName = %q, want %q", got, long)
	}
}

func TestConvertRequestV2_ToolNameShortening(t *testing.T) {
	long := "mcp__github_server__create_pull_request_with_a_really_long_descriptive_name"
	req := &AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []AnthropicTool{
			{Name: long, Description: "x", InputSchema: map[string]interface{}{"type": "object"}},
		},
	}
	body, _, tnm, err := ConvertRequestV2(req)
	if err != nil {
		t.Fatalf("ConvertRequestV2: %v", err)
	}
	if len(tnm) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(tnm))
	}
	// The serialized tool name must be the shortened one
	if strings.Contains(string(body), long) {
		t.Error("body should contain shortened name, not original long name")
	}
}

func TestConvertToolsV2_WriteSuffixAndSchema(t *testing.T) {
	m := map[string]string{}
	tools := convertToolsV2([]AnthropicTool{
		{Name: "Write", Description: "Writes a file.", InputSchema: nil},
	}, m)
	if len(tools) != 1 {
		t.Fatal("expected 1 tool")
	}
	spec := tools[0]["toolSpecification"].(map[string]interface{})
	desc := spec["description"].(string)
	if !strings.Contains(desc, "150 lines") {
		t.Error("Write tool should get chunked-write suffix")
	}
	schema := spec["inputSchema"].(map[string]interface{})["json"].(map[string]interface{})
	if schema["type"] != "object" {
		t.Errorf("schema type should default to object, got %v", schema["type"])
	}
	if _, ok := schema["additionalProperties"]; !ok {
		t.Error("schema should have additionalProperties")
	}
}

func TestPlaceholderTool(t *testing.T) {
	p := placeholderTool("SomeHistoryTool")
	spec := p["toolSpecification"].(map[string]interface{})
	if spec["name"] != "SomeHistoryTool" {
		t.Errorf("name = %v", spec["name"])
	}
	if spec["description"] != "Tool used in conversation history" {
		t.Errorf("desc = %v", spec["description"])
	}
}
