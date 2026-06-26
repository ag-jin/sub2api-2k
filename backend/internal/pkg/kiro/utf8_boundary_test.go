package kiro

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// chunkConcatWriter collects text_delta + thinking_delta text from SSE events.
type utf8CaptureWriter struct{ b strings.Builder }

func (w *utf8CaptureWriter) WriteSSE(event string, data map[string]interface{}) error {
	if event != "content_block_delta" {
		return nil
	}
	delta, _ := data["delta"].(map[string]interface{})
	if delta == nil {
		return nil
	}
	switch delta["type"] {
	case "text_delta":
		if t, ok := delta["text"].(string); ok {
			w.b.WriteString(t)
		}
	case "thinking_delta":
		if t, ok := delta["thinking"].(string); ok {
			w.b.WriteString(t)
		}
	}
	return nil
}

// feedFramesByteSplit pushes content into the converter via assistant frames whose
// payloads are cut at arbitrary byte offsets — reproducing how Kiro streams text
// where the thinking buffer re-slices across frame boundaries. The converter must
// never emit a half rune (U+FFFD) regardless of where the byte cuts land.
func runChunked(t *testing.T, thinking bool, chunks []string) string {
	t.Helper()
	var stream bytes.Buffer
	for _, ch := range chunks {
		stream.Write(assistantFrame(ch))
	}
	w := &utf8CaptureWriter{}
	sc := NewStreamConverter(w, "claude-sonnet-4-5", "msg_t", thinking)
	if err := sc.Run(&stream); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return w.b.String()
}

// TestThinking_NoSwallowedChars reproduces the swallowed-character / fragmented
// English bug: with thinking enabled but NO <thinking> tag in the stream (a plain
// answer), content flows through the no-open-tag flush path. The previous code
// truncated the buffer UNCONDITIONALLY before the whitespace check, so any
// whitespace-only safe-flush was dropped from the buffer entirely — corrupting
// text like "with naming" -> "with aming" and splitting words across lines.
// The output must reconstruct the input byte-for-byte.
func TestThinking_NoSwallowedChars(t *testing.T) {
	full := "Replace CASCADE_RPT_FIELDS with naming's exact value. " +
		"The escaping and adgroup request only need _tb_token_ and csrfid."
	// Stream in small word/space chunks so the 10-byte holdback tail repeatedly
	// lands on whitespace — the exact condition that dropped characters.
	var chunks []string
	for _, w := range splitKeepSpaces(full) {
		chunks = append(chunks, w)
	}
	got := runChunked(t, true, chunks)
	if got != full {
		t.Errorf("text corrupted/swallowed:\n got=%q\nwant=%q", got, full)
	}
}

// splitKeepSpaces splits s into word and single-space tokens, preserving spaces
// as their own chunks (so whitespace-only frames are exercised).
func splitKeepSpaces(s string) []string {
	var out []string
	cur := strings.Builder{}
	for _, r := range s {
		if r == ' ' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			out = append(out, " ")
		} else {
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// TestThinking_NoGarbledUTF8 reproduces the production bug: with thinking enabled,
// the thinkingBuffer is sliced at byte offsets (holding back a fixed-size tail of
// len("<thinking>")=10 bytes), which split multi-byte Chinese characters and
// produced U+FFFD. The fix clamps every slice to a rune boundary. Each frame here
// carries VALID UTF-8 (as real Kiro frames always do — verified against the live
// upstream); the corruption came purely from the converter's internal re-slicing,
// not from the frames themselves.
func TestThinking_NoGarbledUTF8(t *testing.T) {
	full := "杭州西湖,这颗镶嵌在江南大地上的明珠,四季更迭之间各展风姿,美不胜收。春日桃红柳绿,夏日荷香四溢,秋日桂子飘香,冬日断桥残雪。"
	// Feed one rune per frame (always valid UTF-8). The buffer's fixed 10-byte
	// tail hold-back then lands mid-rune on nearly every flush, which is exactly
	// the condition that produced 乱码 before the fix.
	var chunks []string
	for _, r := range full {
		chunks = append(chunks, string(r))
	}

	got := runChunked(t, true, chunks)
	if strings.ContainsRune(got, '�') {
		t.Errorf("thinking output contains U+FFFD (乱码): %q", got)
	}
	if got != full {
		t.Errorf("thinking output mismatch:\n got=%q\nwant=%q", got, full)
	}
	if !utf8.ValidString(got) {
		t.Errorf("thinking output is not valid UTF-8")
	}
}

// TestNonThinking_NoGarbledUTF8 is the control: non-thinking path emits each
// frame's content whole, so it must also stay clean under the same chunking.
func TestNonThinking_NoGarbledUTF8(t *testing.T) {
	full := "你好世界,今天天气很好,我们一起学习编程。中文流式不应出现乱码。"
	var chunks []string
	for _, r := range full {
		chunks = append(chunks, string(r))
	}
	got := runChunked(t, false, chunks)
	if strings.ContainsRune(got, '�') {
		t.Errorf("non-thinking output contains U+FFFD: %q", got)
	}
	if got != full {
		t.Errorf("non-thinking mismatch:\n got=%q\nwant=%q", got, full)
	}
}

// TestThinkingInsideBlock_NoGarbledUTF8 exercises the OTHER clamped slice point:
// content streamed INSIDE a <thinking>...</thinking> block goes through the
// thinkingSafeEmitLen emit path (which holds back a partial close-tag prefix).
// Feeding Chinese rune-by-rune makes that hold-back land mid-rune; the output
// thinking_delta + trailing text must reconstruct cleanly with no U+FFFD.
func TestThinkingInsideBlock_NoGarbledUTF8(t *testing.T) {
	thinkingText := "让我想想这个问题:用户需要中文推理,我应该仔细分析每个细节,确保万无一失。"
	answerText := "好的,答案是:中文推理与回复都不会乱码。"
	full := "<thinking>" + thinkingText + "</thinking>\n\n" + answerText

	var chunks []string
	for _, r := range full {
		chunks = append(chunks, string(r))
	}
	got := runChunked(t, true, chunks)
	if strings.ContainsRune(got, '�') {
		t.Errorf("in-block output contains U+FFFD (乱码): %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("in-block output is not valid UTF-8")
	}
	// The visible stream must contain both the thinking text and the answer text
	// intact (a separator newline between them is fine). The critical property is
	// that every Chinese character survives — no rune is split/dropped.
	if !strings.Contains(got, thinkingText) {
		t.Errorf("thinking text corrupted/missing:\n got=%q\nwant substring=%q", got, thinkingText)
	}
	if !strings.Contains(got, answerText) {
		t.Errorf("answer text corrupted/missing:\n got=%q\nwant substring=%q", got, answerText)
	}
}
