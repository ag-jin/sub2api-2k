package kiro

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// utf8SafeEmitLen clamps n (a byte length within buf that we intend to emit) so
// that it never falls in the middle of a multi-byte UTF-8 character. Kiro splits
// assistant text across event-stream frames on arbitrary byte boundaries, so a
// multi-byte rune can straddle two frames. Emitting half a rune yields U+FFFD
// ("锟斤拷"-style 乱码) once json.Marshal re-encodes it. Holding the trailing
// incomplete bytes until the next frame completes the rune fixes this.
//
// Returns the largest m <= n such that buf[:m] ends on a rune boundary.
func utf8SafeEmitLen(buf string, n int) int {
	if n <= 0 || n >= len(buf) {
		// n>=len means we'd emit everything up to n; still must ensure the slice
		// end is a rune boundary. For n>=len(buf) the caller emits the whole buf,
		// which we trim below.
		if n > len(buf) {
			n = len(buf)
		}
		if n == len(buf) {
			return trimIncompleteTrailingRune(buf)
		}
		return n
	}
	// Walk back from n to the start of the rune that byte n-1 belongs to.
	for n > 0 && !utf8.RuneStart(buf[n]) {
		n--
	}
	return n
}

// trimIncompleteTrailingRune returns len(buf) minus any trailing bytes that form
// an incomplete UTF-8 sequence, so buf[:result] is always valid UTF-8 and the
// held-back tail can be completed by the next frame.
func trimIncompleteTrailingRune(buf string) int {
	if buf == "" {
		return 0
	}
	r, size := utf8.DecodeLastRuneInString(buf)
	if r == utf8.RuneError && size <= 1 {
		// Trailing bytes are an incomplete sequence. Find where it starts.
		i := len(buf) - 1
		for i >= 0 && !utf8.RuneStart(buf[i]) {
			i--
		}
		if i >= 0 {
			return i
		}
		return 0
	}
	return len(buf)
}

// SSEWriter receives Anthropic SSE events (event name + JSON data).
type SSEWriter interface {
	WriteSSE(event string, data map[string]interface{}) error
}

// estimateTokens approximates output token count (Chinese ~1.5 chars/token, others ~4).
func estimateTokens(text string) int {
	var chinese, other int
	for _, c := range text {
		if c >= 0x4E00 && c <= 0x9FFF {
			chinese++
		} else {
			other++
		}
	}
	chineseTokens := (chinese*2 + 2) / 3
	otherTokens := (other + 3) / 4
	t := chineseTokens + otherTokens
	if t < 1 && (chinese+other) > 0 {
		t = 1
	}
	return t
}

// StreamConverter turns a Kiro event-stream into Anthropic SSE events.
type StreamConverter struct {
	w            SSEWriter
	model        string
	displayModel string // client-requested model (echoed in message_start); "" means use model

	messageID       string
	thinkingEnabled bool

	started        bool
	nextIndex      int
	textBlockIndex int  // -1 if not open
	textOpen       bool
	toolBlocks     map[string]int // toolUseId -> block index
	toolNameMap    map[string]string // short tool name -> original (reverse map)
	toolOpen       map[int]bool

	inputTokens  int
	outputTokens int
	stopReason   string
	deltaSent    bool

	// thinking state
	thinkingBuffer     string
	inThinkingBlock    bool
	thinkingExtracted  bool
	thinkingBlockIndex int
}

// NewStreamConverter creates a converter that writes Anthropic SSE to w.
func NewStreamConverter(w SSEWriter, model, messageID string, thinkingEnabled bool) *StreamConverter {
	return &StreamConverter{
		w:                  w,
		model:              model,
		messageID:          messageID,
		thinkingEnabled:    thinkingEnabled,
		textBlockIndex:     -1,
		thinkingBlockIndex: -1,
		toolBlocks:         map[string]int{},
		toolOpen:           map[int]bool{},
	}
}

// SetToolNameMap installs the short->original tool-name map so tool_use events
// in the response restore their original names (mirrors kiro.rs tool_name_map).
func (c *StreamConverter) SetToolNameMap(m map[string]string) {
	c.toolNameMap = m
}

// SetDisplayModel sets the client-requested model name echoed in message_start,
// so the client sees its original model ID (e.g. "claude-sonnet-4-5-20250929")
// rather than the upstream Kiro model ID (e.g. "claude-sonnet-4.5").
func (c *StreamConverter) SetDisplayModel(m string) {
	c.displayModel = m
}

func (c *StreamConverter) effectiveDisplayModel() string {
	if c.displayModel != "" {
		return c.displayModel
	}
	return c.model
}

// Run reads frames from r until EOF, converting each Kiro event to SSE.
func (c *StreamConverter) Run(r io.Reader) error {
	if err := c.emitMessageStart(); err != nil {
		return err
	}

	for {
		frame, err := ParseFrame(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Unexpected EOF mid-stream is treated as end of stream.
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return fmt.Errorf("parse frame: %w", err)
		}
		if err := c.handleFrame(frame); err != nil {
			return err
		}
	}

	return c.finalize()
}

func (c *StreamConverter) emitMessageStart() error {
	if c.started {
		return nil
	}
	c.started = true
	if err := c.w.WriteSSE("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            c.messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         c.effectiveDisplayModel(),
			"stop_reason":   nil,
			"stop_sequence": nil,
			"stop_details": nil,
			"usage": map[string]interface{}{
				"input_tokens":               c.inputTokens,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"output_tokens":              1,
			},
		},
	}); err != nil {
		return err
	}
	// Do NOT eagerly open a text block — let the first text/tool delta open
	// it lazily. This prevents an empty text block at index 0 when Kiro sends
	// a tool_use event before any text, which caused Claude desktop clients to
	// render the conversation vertically (降级渲染).
	if c.thinkingEnabled {
		// In thinking mode we still defer to the first thinking/text delta
		// (thinking block is opened by processWithThinking when the opening
		// tag arrives, or by the first text delta if tags never appear).
		return nil
	}
	return nil
}

func (c *StreamConverter) openTextBlock() error {
	if c.textOpen {
		return nil
	}
	idx := c.nextIndex
	c.nextIndex++
	c.textBlockIndex = idx
	c.textOpen = true
	return c.w.WriteSSE("content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	})
}

func (c *StreamConverter) closeTextBlock() error {
	if !c.textOpen {
		return nil
	}
	idx := c.textBlockIndex
	c.textOpen = false
	c.textBlockIndex = -1
	return c.w.WriteSSE("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (c *StreamConverter) emitTextDelta(text string) error {
	if text == "" {
		return nil
	}
	if !c.textOpen {
		if err := c.openTextBlock(); err != nil {
			return err
		}
	}
	c.outputTokens += estimateTokens(text)
	return c.w.WriteSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": c.textBlockIndex,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": text,
		},
	})
}

func (c *StreamConverter) handleFrame(frame *Frame) error {
	// Error / exception frames.
	switch frame.MessageType() {
	case "exception", "error":
		// Mark stop reason for known exceptions; ignore payload otherwise.
		if strings.Contains(string(frame.Payload), "ContentLengthExceededException") {
			c.stopReason = "max_tokens"
		}
		return nil
	}

	switch frame.EventType() {
	case "assistantResponseEvent":
		var ev AssistantResponseEvent
		if err := json.Unmarshal(frame.Payload, &ev); err != nil {
			return nil // tolerate malformed event
		}
		return c.handleAssistantText(ev.Content)

	case "toolUseEvent":
		var ev ToolUseEvent
		if err := json.Unmarshal(frame.Payload, &ev); err != nil {
			return nil
		}
		return c.handleToolUse(&ev)

	case "contextUsageEvent":
		var ev struct {
			ContextUsagePercentage float64 `json:"contextUsagePercentage"`
			PercentageUsed         float64 `json:"percentageUsed"`
		}
		if err := json.Unmarshal(frame.Payload, &ev); err != nil {
			return nil
		}
		pct := ev.ContextUsagePercentage
		if pct == 0 {
			pct = ev.PercentageUsed
		}
		window := ContextWindowSize(c.model)
		c.inputTokens = int(pct * float64(window) / 100.0)
		if pct >= 100.0 {
			c.stopReason = "model_context_window_exceeded"
		}
		return nil

	default:
		return nil
	}
}

func (c *StreamConverter) handleToolUse(ev *ToolUseEvent) error {
	// Close any open text block before starting a tool block.
	if c.thinkingEnabled && c.inThinkingBlock {
		if err := c.flushThinkingEnd(); err != nil {
			return err
		}
	}
	if c.textOpen {
		if err := c.closeTextBlock(); err != nil {
			return err
		}
	}

	idx, exists := c.toolBlocks[ev.ToolUseID]
	if !exists {
		idx = c.nextIndex
		c.nextIndex++
		c.toolBlocks[ev.ToolUseID] = idx
		c.toolOpen[idx] = true
		if err := c.w.WriteSSE("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    ev.ToolUseID,
				"name":  c.restoreToolName(ev.Name),
				"input": map[string]interface{}{},
			},
		}); err != nil {
			return err
		}
	}

	if ev.Input != "" {
		if err := c.w.WriteSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": ev.Input,
			},
		}); err != nil {
			return err
		}
	}

	if ev.Stop {
		if c.toolOpen[idx] {
			c.toolOpen[idx] = false
			c.stopReason = "tool_use"
			return c.w.WriteSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
		}
	}

	return nil
}

const thinkingOpenTag = "<thinking>"
const thinkingCloseTag = "</thinking>"

// handleAssistantText routes text content through the thinking state machine
// (if enabled) or directly as text deltas.
func (c *StreamConverter) handleAssistantText(content string) error {
	if content == "" {
		return nil
	}
	if !c.thinkingEnabled {
		return c.emitTextDelta(content)
	}
	return c.processWithThinking(content)
}

func (c *StreamConverter) openThinkingBlock() error {
	if c.thinkingBlockIndex >= 0 {
		return nil
	}
	idx := c.nextIndex
	c.nextIndex++
	c.thinkingBlockIndex = idx
	c.inThinkingBlock = true
	return c.w.WriteSSE("content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]interface{}{
			"type":      "thinking",
			"thinking":  "",
			"signature": "",
		},
	})
}

func (c *StreamConverter) emitThinkingDelta(text string) error {
	if c.thinkingBlockIndex < 0 {
		return nil
	}
	return c.w.WriteSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": c.thinkingBlockIndex,
		"delta": map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": text,
		},
	})
}

func (c *StreamConverter) closeThinkingBlock() error {
	if c.thinkingBlockIndex < 0 {
		return nil
	}
	idx := c.thinkingBlockIndex
	c.inThinkingBlock = false
	c.thinkingExtracted = true
	c.thinkingBlockIndex = -1
	// Emit a placeholder signature_delta before content_block_stop. The
	// Anthropic standard requires a signature_delta (even a placeholder
	// one) for the client to render thinking blocks correctly. Real
	// Anthropic SSE carries a ~2KB base64 signature; we use a short
	// valid base64 placeholder so the client format check passes.
	if err := c.w.WriteSSE("content_block_delta", map[string]interface{}{
		"type": "content_block_delta",
		"index": idx,
		"delta": map[string]interface{}{
			"type":      "signature_delta",
			"signature": "EoY=", // valid base64 placeholder
		},
	}); err != nil {
		return err
	}
	return c.w.WriteSSE("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": idx,
	})
}

// flushThinkingEnd closes a thinking block when a close tag sits at buffer end
// or when tool_use interrupts an open thinking block.
func (c *StreamConverter) flushThinkingEnd() error {
	idx := findRealThinkingEndAtBufferEnd(c.thinkingBuffer)
	if idx < 0 {
		// Stream is ending / tool_use boundary: no whitespace-terminated close
		// tag. Fall back to a plain (non-quoted) close-tag search so trailing
		// text after </thinking> is still split off as normal text.
		searchStart := 0
		for {
			rel := strings.Index(c.thinkingBuffer[searchStart:], thinkingCloseTag)
			if rel < 0 {
				break
			}
			pos := searchStart + rel
			if pos > 0 && isThinkingQuoteByte(c.thinkingBuffer[pos-1]) {
				searchStart = pos + 1
				continue
			}
			afterPos := pos + len(thinkingCloseTag)
			if afterPos < len(c.thinkingBuffer) && isThinkingQuoteByte(c.thinkingBuffer[afterPos]) {
				searchStart = pos + 1
				continue
			}
			idx = pos
			break
		}
	}
	if idx >= 0 {
		thinkingContent := c.thinkingBuffer[:idx]
		if thinkingContent != "" {
			if err := c.emitThinkingDelta(thinkingContent); err != nil {
				return err
			}
		}
		remaining := strings.TrimLeft(c.thinkingBuffer[idx+len(thinkingCloseTag):], " \t\r\n")
		c.thinkingBuffer = ""
		if err := c.closeThinkingBlock(); err != nil {
			return err
		}
		if remaining != "" {
			return c.emitTextDelta(remaining)
		}
		return nil
	}
	// No close tag: flush remaining as thinking content.
	if c.thinkingBuffer != "" {
		if err := c.emitThinkingDelta(c.thinkingBuffer); err != nil {
			return err
		}
		c.thinkingBuffer = ""
	}
	return c.closeThinkingBlock()
}

// processWithThinking buffers content and splits out <thinking>...</thinking> blocks.
func (c *StreamConverter) processWithThinking(content string) error {
	c.thinkingBuffer += content

	for {
		if !c.inThinkingBlock && !c.thinkingExtracted {
			start := strings.Index(c.thinkingBuffer, thinkingOpenTag)
			if start < 0 {
				// No open tag yet. Keep a small tail in case a tag is split
				// across chunks; flush the rest as text. The cut point is clamped
				// to a UTF-8 rune boundary so a multi-byte char split across frames
				// is never emitted half-formed (would become U+FFFD 乱码).
				if len(c.thinkingBuffer) > len(thinkingOpenTag) {
					cut := len(c.thinkingBuffer) - len(thinkingOpenTag)
					cut = utf8SafeEmitLen(c.thinkingBuffer, cut)
					if cut > 0 {
						flush := c.thinkingBuffer[:cut]
						// IMPORTANT: only consume from the buffer when we actually
						// emit. Whitespace-only flushes are kept in the buffer to be
						// joined with following content — truncating here would DROP
						// the whitespace and corrupt the text (e.g. "with naming" ->
						// "with aming"). Mirrors kiro.rs which truncates inside the
						// non-empty branch only.
						if strings.TrimSpace(flush) != "" {
							c.thinkingBuffer = c.thinkingBuffer[cut:]
							if err := c.emitTextDelta(flush); err != nil {
								return err
							}
						}
					}
				}
				return nil
			}
			before := c.thinkingBuffer[:start]
			if strings.TrimSpace(before) != "" {
				if err := c.emitTextDelta(before); err != nil {
					return err
				}
			}
			c.thinkingBuffer = c.thinkingBuffer[start+len(thinkingOpenTag):]
			if err := c.openThinkingBlock(); err != nil {
				return err
			}
			continue
		}

		if c.inThinkingBlock {
			end := findRealThinkingEnd(c.thinkingBuffer)
			if end == -2 {
				// A candidate close tag is at the tail but we cannot yet tell if
				// "\n\n" follows; wait for more data without emitting the tail.
				return nil
			}
			if end < 0 {
				// No genuine close tag yet. Emit thinking content but hold back a
				// tail that could be (or grow into) a close tag, so we never split
				// or prematurely emit it. The tail starts at the last index that
				// is either a full "</thinking>" occurrence or a partial prefix of
				// it at the buffer end; only that minimal region is held, so inline
				// '<' characters in normal thinking text still stream out.
				safe := thinkingSafeEmitLen(c.thinkingBuffer)
				// Clamp to a UTF-8 rune boundary: hold back any trailing incomplete
				// multi-byte sequence so the next frame can complete it (avoids 乱码).
				safe = utf8SafeEmitLen(c.thinkingBuffer, safe)
				if safe > 0 {
					emit := c.thinkingBuffer[:safe]
					c.thinkingBuffer = c.thinkingBuffer[safe:]
					if err := c.emitThinkingDelta(emit); err != nil {
						return err
					}
				}
				return nil
			}
			thinkingContent := c.thinkingBuffer[:end]
			if thinkingContent != "" {
				if err := c.emitThinkingDelta(thinkingContent); err != nil {
					return err
				}
			}
			c.thinkingBuffer = c.thinkingBuffer[end+len(thinkingCloseTag):]
			if err := c.closeThinkingBlock(); err != nil {
				return err
			}
			continue
		}

		// thinking already extracted: rest is plain text.
		if c.thinkingBuffer != "" {
			text := c.thinkingBuffer
			c.thinkingBuffer = ""
			if err := c.emitTextDelta(text); err != nil {
				return err
			}
		}
		return nil
	}
}

func (c *StreamConverter) finalize() error {
	// Flush any buffered thinking content.
	if c.thinkingEnabled {
		if c.inThinkingBlock {
			if err := c.flushThinkingEnd(); err != nil {
				return err
			}
		} else if !c.thinkingExtracted && c.thinkingBuffer != "" {
			buffered := c.thinkingBuffer
			c.thinkingBuffer = ""
			if err := c.emitTextDelta(buffered); err != nil {
				return err
			}
		}
	}

	// Close any open text block.
	if c.textOpen {
		if err := c.closeTextBlock(); err != nil {
			return err
		}
	}
	// Close any still-open tool blocks.
	for idx, open := range c.toolOpen {
		if open {
			c.toolOpen[idx] = false
			if err := c.w.WriteSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			}); err != nil {
				return err
			}
		}
	}

	if !c.deltaSent {
		c.deltaSent = true
		if c.stopReason == "" {
			c.stopReason = "end_turn"
		}
		if err := c.w.WriteSSE("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   c.stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]interface{}{
				// input_tokens here is the accurate value computed from the
				// upstream contextUsageEvent (the message_start value was an
				// early estimate). Clients reconcile usage from message_delta.
				"input_tokens":  c.inputTokens,
				"output_tokens": c.outputTokens,
			},
		}); err != nil {
			return err
		}
	}

	return c.w.WriteSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// InputTokens returns the computed input token count (from contextUsageEvent).
func (c *StreamConverter) InputTokens() int { return c.inputTokens }

// OutputTokens returns the estimated output token count.
func (c *StreamConverter) OutputTokens() int { return c.outputTokens }


// restoreToolName maps a (possibly shortened) tool name back to its original.
func (c *StreamConverter) restoreToolName(name string) string {
	if c.toolNameMap != nil {
		if orig, ok := c.toolNameMap[name]; ok {
			return orig
		}
	}
	return name
}

// --- thinking close-tag guard (mirrors kiro.rs find_real_thinking_end_tag) ---

// thinkingQuoteChars are characters that, when adjacent to a </thinking> tag,
// indicate the tag is being quoted/referenced (e.g. inline code, punctuation)
// rather than a real closing tag. Mirrors kiro.rs QUOTE_CHARS.
const thinkingQuoteChars = "`\"'\\#!@$%^&*()-_=+[]{};:<>,.?/"

func isThinkingQuoteByte(b byte) bool {
	for i := 0; i < len(thinkingQuoteChars); i++ {
		if thinkingQuoteChars[i] == b {
			return true
		}
	}
	return false
}

// findRealThinkingEnd returns the index of a genuine </thinking> close tag in
// buf, or -1. A tag is genuine only if it is NOT wrapped by quote chars AND is
// followed by "\n\n". Returns -2 to signal "need more data" (a candidate tag is
// at the buffer tail and we can't yet tell if "\n\n" follows). Mirrors kiro.rs
// find_real_thinking_end_tag.
func findRealThinkingEnd(buf string) int {
	const tag = thinkingCloseTag
	searchStart := 0
	for {
		rel := strings.Index(buf[searchStart:], tag)
		if rel < 0 {
			return -1
		}
		pos := searchStart + rel
		quoteBefore := pos > 0 && isThinkingQuoteByte(buf[pos-1])
		afterPos := pos + len(tag)
		quoteAfter := afterPos < len(buf) && isThinkingQuoteByte(buf[afterPos])
		if quoteBefore || quoteAfter {
			searchStart = pos + 1
			continue
		}
		after := buf[afterPos:]
		if len(after) < 2 {
			// not enough to decide whether "\n\n" follows; wait for more data
			return -2
		}
		if strings.HasPrefix(after, "\n\n") {
			return pos
		}
		searchStart = pos + 1
	}
}

// findRealThinkingEndAtBufferEnd returns the index of a </thinking> close tag
// when everything after it is whitespace (used at stream end / before tool_use,
// where the trailing "\n\n" may be absent). Mirrors kiro.rs
// find_real_thinking_end_tag_at_buffer_end.
func findRealThinkingEndAtBufferEnd(buf string) int {
	const tag = thinkingCloseTag
	searchStart := 0
	for {
		rel := strings.Index(buf[searchStart:], tag)
		if rel < 0 {
			return -1
		}
		pos := searchStart + rel
		quoteBefore := pos > 0 && isThinkingQuoteByte(buf[pos-1])
		if quoteBefore {
			searchStart = pos + 1
			continue
		}
		after := buf[pos+len(tag):]
		if strings.TrimSpace(after) == "" {
			return pos
		}
		searchStart = pos + 1
	}
}


// thinkingSafeEmitLen returns how many leading bytes of buf are safe to emit as
// thinking content without risking splitting a close tag. It holds back:
//   - any full "</thinking>" occurrence (its trailing "\n\n" may still arrive),
//   - or a partial prefix of "</thinking>" sitting at the very end of buf.
// Inline '<' that cannot be a close-tag prefix does not block emission.
func thinkingSafeEmitLen(buf string) int {
	// Hold from the last full close tag (could be a real close awaiting "\n\n").
	if i := strings.LastIndex(buf, thinkingCloseTag); i >= 0 {
		return i
	}
	// Hold a trailing partial prefix of the close tag, e.g. "</thi" at the end.
	maxP := len(thinkingCloseTag) - 1
	if maxP > len(buf) {
		maxP = len(buf)
	}
	for p := maxP; p > 0; p-- {
		if strings.HasSuffix(buf, thinkingCloseTag[:p]) {
			return len(buf) - p
		}
	}
	return len(buf)
}
