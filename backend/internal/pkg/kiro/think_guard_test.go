package kiro

import (
	"strings"
	"testing"
)

func TestFindRealThinkingEnd_Genuine(t *testing.T) {
	// genuine close tag: not quoted, followed by \n\n
	buf := "some reasoning here</thinking>\n\nfinal"
	idx := findRealThinkingEnd(buf)
	if idx != len("some reasoning here") {
		t.Errorf("expected genuine close at %d, got %d", len("some reasoning here"), idx)
	}
}

func TestFindRealThinkingEnd_QuotedNotEnd(t *testing.T) {
	// tag wrapped in backticks => not a real close; rest is more content + real end
	buf := "I should explain the `</thinking>` tag here</thinking>\n\n"
	idx := findRealThinkingEnd(buf)
	// must skip the backtick-wrapped one and find the genuine one
	want := strings.Index(buf, "tag here") + len("tag here")
	if idx != want {
		t.Errorf("expected genuine close at %d, got %d (buf=%q)", want, idx, buf)
	}
}

func TestFindRealThinkingEnd_NoTrailingNewlines(t *testing.T) {
	// close tag not followed by \n\n (mid-sentence reference) => not genuine
	buf := "talking about </thinking> in the middle of a sentence and more text follows here"
	if idx := findRealThinkingEnd(buf); idx >= 0 {
		t.Errorf("should not treat mid-sentence </thinking> as close, got %d", idx)
	}
}

func TestFindRealThinkingEnd_NeedMoreData(t *testing.T) {
	// candidate at tail, can't tell if \n\n follows
	buf := "reasoning</thinking>"
	if idx := findRealThinkingEnd(buf); idx != -2 {
		t.Errorf("expected -2 (need more data), got %d", idx)
	}
}

func TestFindRealThinkingEndAtBufferEnd_WhitespaceOnly(t *testing.T) {
	// at stream end, close tag followed by only whitespace is genuine
	buf := "final thought</thinking>  \n"
	if idx := findRealThinkingEndAtBufferEnd(buf); idx != len("final thought") {
		t.Errorf("expected close at %d, got %d", len("final thought"), idx)
	}
	// but a quoted one is not
	buf2 := "mentioning `</thinking>` only"
	if idx := findRealThinkingEndAtBufferEnd(buf2); idx >= 0 {
		t.Errorf("quoted tag should not be treated as close, got %d", idx)
	}
}
