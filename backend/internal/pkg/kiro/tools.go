package kiro

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const toolNameMaxLen = 63

const writeToolDescSuffix = "- IMPORTANT: If the content to write exceeds 150 lines, you MUST only write the first 50 lines using this tool, then use `Edit` tool to append the remaining content in chunks of no more than 50 lines each. If needed, leave a unique placeholder to help append content. Do NOT attempt to write all content at once."

const editToolDescSuffix = "- IMPORTANT: If the `new_string` content exceeds 50 lines, you MUST split it into multiple Edit calls, each replacing no more than 50 lines at a time. If used to append content, leave a unique placeholder to help append content. On the final chunk, do NOT include the placeholder."

const toolDescMaxLen = 10000

// shortenToolName shortens an over-long tool name to <=63 chars using a sha256
// suffix, mirroring kiro.rs converter.rs::shorten_tool_name.
func shortenToolName(name string) string {
	if len(name) <= toolNameMaxLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hashSuffix := fmt.Sprintf("%x", sum)[:8]
	prefixMax := toolNameMaxLen - 1 - 8 // 54
	// byte-truncate at a rune boundary
	prefix := name
	if len(name) > prefixMax {
		// find a valid rune boundary at or before prefixMax
		end := prefixMax
		for end > 0 && !utf8RuneStart(name[end]) {
			end--
		}
		prefix = name[:end]
	}
	return prefix + "_" + hashSuffix
}

func utf8RuneStart(b byte) bool {
	// continuation bytes are 0b10xxxxxx
	return b&0xC0 != 0x80
}

// mapToolName returns the (possibly shortened) tool name, recording the
// short->original mapping when shortening occurs.
func mapToolName(name string, toolNameMap map[string]string) string {
	if len(name) <= toolNameMaxLen {
		return name
	}
	short := shortenToolName(name)
	toolNameMap[short] = name
	return short
}

// truncateRunes truncates s to at most n runes.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// normalizeJSONSchema ensures a tool input schema has the required shape,
// mirroring kiro.rs converter.rs::normalize_json_schema.
func normalizeJSONSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{},
			"required":             []interface{}{},
			"additionalProperties": true,
		}
	}
	// type must be a non-empty string
	if t, ok := schema["type"].(string); !ok || t == "" {
		schema["type"] = "object"
	}
	// properties must be an object
	if _, ok := schema["properties"].(map[string]interface{}); !ok {
		schema["properties"] = map[string]interface{}{}
	}
	// required must be an array of strings
	switch req := schema["required"].(type) {
	case []interface{}:
		filtered := make([]interface{}, 0, len(req))
		for _, v := range req {
			if s, ok := v.(string); ok {
				filtered = append(filtered, s)
			}
		}
		schema["required"] = filtered
	default:
		schema["required"] = []interface{}{}
	}
	// additionalProperties: allow bool or object, else true
	switch schema["additionalProperties"].(type) {
	case bool, map[string]interface{}:
		// keep
	default:
		schema["additionalProperties"] = true
	}
	return schema
}

// convertToolsV2 converts Anthropic tool defs to Kiro toolSpecification format,
// applying name shortening, description suffixes, truncation, and schema
// normalization. It records short->original name mappings in toolNameMap.
func convertToolsV2(tools []AnthropicTool, toolNameMap map[string]string) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		desc := t.Description
		switch t.Name {
		case "Write":
			desc = desc + "\n" + writeToolDescSuffix
		case "Edit":
			desc = desc + "\n" + editToolDescSuffix
		}
		desc = truncateRunes(desc, toolDescMaxLen)

		schema := normalizeJSONSchema(t.InputSchema)

		out = append(out, map[string]interface{}{
			"toolSpecification": map[string]interface{}{
				"name":        mapToolName(t.Name, toolNameMap),
				"description": desc,
				"inputSchema": map[string]interface{}{
					"json": schema,
				},
			},
		})
	}
	return out
}

// placeholderTool builds a placeholder tool definition for a tool name that
// appears in history but is missing from the current tools list.
func placeholderTool(name string) map[string]interface{} {
	return map[string]interface{}{
		"toolSpecification": map[string]interface{}{
			"name":        name,
			"description": "Tool used in conversation history",
			"inputSchema": map[string]interface{}{
				"json": map[string]interface{}{
					"$schema":              "http://json-schema.org/draft-07/schema#",
					"type":                 "object",
					"properties":           map[string]interface{}{},
					"required":             []interface{}{},
					"additionalProperties": true,
				},
			},
		},
	}
}

var _ = json.Marshal
