package deepseek

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// SSEWriter is the minimal sink for Anthropic SSE output (satisfied by the
// gateway's gin-based writer).
type SSEWriter interface {
	WriteString(s string) (int, error)
	Flush()
}

// StreamUsage carries token counts surfaced after a stream completes.
type StreamUsage struct {
	InputTokens  int
	OutputTokens int
}

// ConvertStream reads a DeepSeek Chat Completions SSE stream from r and writes
// the equivalent Anthropic Messages SSE to w. Chain per chunk:
//
//	DeepSeek SSE chunk (choices[].delta.content / .reasoning_content / .tool_calls)
//	  → apicompat.ChatCompletionsChunkToResponsesEvents
//	  → apicompat.ResponsesEventToAnthropicEvents
//	  → Anthropic SSE
//
// reasoning_content flows through as Anthropic thinking blocks automatically
// (the apicompat chain maps it to reasoning_summary_text.delta → thinking_delta).
func ConvertStream(r io.Reader, w SSEWriter, model string) (StreamUsage, error) {
	ccState := apicompat.NewChatCompletionsToResponsesStreamState(model)
	anthState := apicompat.NewResponsesEventToAnthropicState()
	var usage StreamUsage

	emit := func(evts []apicompat.AnthropicStreamEvent) error {
		for _, e := range evts {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(e)
			if err != nil {
				return err
			}
			if _, err := w.WriteString(sse); err != nil {
				return err
			}
		}
		if len(evts) > 0 {
			w.Flush()
		}
		return nil
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip malformed keep-alive/comment lines rather than aborting.
			continue
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		respEvents := apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, ccState)
		for i := range respEvents {
			if err := emit(apicompat.ResponsesEventToAnthropicEvents(&respEvents[i], anthState)); err != nil {
				return usage, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return usage, fmt.Errorf("read upstream stream: %w", err)
	}

	for _, e := range apicompat.FinalizeChatCompletionsResponsesStream(ccState) {
		if err := emit(apicompat.ResponsesEventToAnthropicEvents(&e, anthState)); err != nil {
			return usage, err
		}
	}

	// Emit synthetic termination if the compatibility chain still did not
	// produce a completion event.
	if err := emit(apicompat.FinalizeResponsesAnthropicStream(anthState)); err != nil {
		return usage, err
	}
	return usage, nil
}

// ConvertNonStream converts a complete DeepSeek Chat Completions JSON response
// into an Anthropic Messages JSON response. Chain:
//
//	ChatCompletionsResponse → apicompat.ChatCompletionsResponseToResponses
//	  → apicompat.ResponsesToAnthropic
//
// reasoning_content (message-level) becomes an Anthropic thinking block.
func ConvertNonStream(upstreamBody []byte, model string) ([]byte, StreamUsage, error) {
	var ccResp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(upstreamBody, &ccResp); err != nil {
		return nil, StreamUsage{}, fmt.Errorf("parse chatcompletions response: %w", err)
	}
	respResp := apicompat.ChatCompletionsResponseToResponses(&ccResp, model)
	anthResp := apicompat.ResponsesToAnthropic(respResp, model)

	var usage StreamUsage
	if ccResp.Usage != nil {
		usage.InputTokens = ccResp.Usage.PromptTokens
		usage.OutputTokens = ccResp.Usage.CompletionTokens
	}

	out, err := json.Marshal(anthResp)
	if err != nil {
		return nil, usage, fmt.Errorf("marshal anthropic response: %w", err)
	}
	return out, usage, nil
}

// ParseUpstreamError extracts a concise message from a non-2xx upstream body for
// logging, without leaking the full payload.
func ParseUpstreamError(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 400 {
		trimmed = trimmed[:400]
	}
	return string(trimmed)
}
