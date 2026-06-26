package kiro

import "testing"

// TestStripThinkingSuffix verifies the "-thinking" gateway suffix is removed so
// the model echoed to the client is a valid Anthropic model name.
func TestStripThinkingSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-sonnet-4-5-thinking", "claude-sonnet-4-5"},
		{"claude-opus-4-8-thinking", "claude-opus-4-8"},
		{"claude-sonnet-4-5-20250929-thinking", "claude-sonnet-4-5-20250929"},
		{"claude-sonnet-4-5", "claude-sonnet-4-5"}, // no suffix: unchanged
		{"claude-opus-4.6", "claude-opus-4.6"},     // dot format, no suffix
		{"CLAUDE-SONNET-4-5-THINKING", "CLAUDE-SONNET-4-5"}, // case-insensitive cut, prefix case preserved
		{"", ""},
	}
	for _, c := range cases {
		if got := StripThinkingSuffix(c.in); got != c.want {
			t.Errorf("StripThinkingSuffix(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
