package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// isGLMTextOnlyAccount checks whether an account's model_mapping contains any
// GLM text-only model (glm-5.2 etc) as a target. Used to decide whether to
// apply stream-then-buffer for non-streaming requests.
func isGLMTextOnlyAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformAnthropic {
		return false
	}
	mapping := account.GetModelMapping()
	for _, v := range mapping {
		if isGLMTextOnlyModel(v) {
			return true
		}
	}
	return false
}

// forwardAnthropicStreamThenBuffer handles non-streaming requests to GLM
// text-only accounts by internally upgrading to streaming, collecting the
// full SSE response, and returning a standard Anthropic Messages JSON body.
//
// Why: astraflow GLM-5.2 takes 60s+ for large non-streaming requests (must
// generate full response before returning), causing 504 timeouts. Streaming
// gets first token in ~4s, avoiding timeouts entirely.
func (s *GatewayService) forwardAnthropicStreamThenBuffer(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	input anthropicPassthroughForwardInput,
) (*ForwardResult, error) {
	startTime := input.StartTime

	// 1. Patch body: stream:false → stream:true
	streamBody := patchStreamTrue(input.Body)

	logger.LegacyPrintf("service.gateway", "[StreamThenBuffer] account=%d name=%s model=%s body_size=%d",
		account.ID, account.Name, input.RequestModel, len(streamBody))

	// 2. Build upstream request (reuse existing builder)
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "apikey" {
		return nil, fmt.Errorf("stream-then-buffer requires apikey token, got: %s", tokenType)
	}

	upstreamReq, wireBody, err := s.buildUpstreamRequestAnthropicAPIKeyPassthrough(ctx, c, account, streamBody, token)
	if err != nil {
		return nil, err
	}
	_ = wireBody

	// Set streaming headers
	upstreamReq.Header.Set("Accept", "text/event-stream")

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("stream-then-buffer upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		logger.LegacyPrintf("service.gateway", "[StreamThenBuffer] Upstream error: Account=%d(%s) Status=%d Body=%s",
			account.ID, account.Name, resp.StatusCode, truncateString(string(errBody), 500))

		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           errBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		c.Data(resp.StatusCode, "application/json", errBody)
		return nil, fmt.Errorf("stream-then-buffer upstream error: %d", resp.StatusCode)
	}

	// 3. Collect SSE → JSON
	jsonResp, usage, err := collectAnthropicSSEToJSON(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("stream-then-buffer collect SSE: %w", err)
	}

	elapsed := time.Since(startTime)
	logger.LegacyPrintf("service.gateway", "[StreamThenBuffer] Success: account=%d elapsed=%dms json_size=%d",
		account.ID, elapsed.Milliseconds(), len(jsonResp))

	// 4. Return JSON to client (same as non-streaming)
	c.Data(http.StatusOK, "application/json", jsonResp)

	return &ForwardResult{
		RequestID:     resp.Header.Get("x-request-id"),
		Usage:         usage,
		Model:         input.OriginalModel,
		UpstreamModel: input.RequestModel,
		Stream:        false,
		Duration:      elapsed,
	}, nil
}

// patchStreamTrue replaces "stream":false with "stream":true in the JSON body.
func patchStreamTrue(body []byte) []byte {
	return bytes.Replace(body, []byte(`"stream":false`), []byte(`"stream":true`), 1)
}

// collectAnthropicSSEToJSON reads an Anthropic Messages SSE stream and
// reconstructs the equivalent non-streaming JSON response.
func collectAnthropicSSEToJSON(r io.Reader) ([]byte, ClaudeUsage, error) {
	var (
		msgID      string
		model      string
		stopReason string
		usage      ClaudeUsage
		blocks     []sseBlock
		curBlock   *sseBlock
	)

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}

		var evt sseEvent
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "message_start":
			if evt.Message != nil {
				msgID = evt.Message.ID
				model = evt.Message.Model
				if evt.Message.Usage != nil {
					usage.InputTokens = evt.Message.Usage.InputTokens
					usage.CacheReadInputTokens = evt.Message.Usage.CacheReadInputTokens
					usage.CacheCreationInputTokens = evt.Message.Usage.CacheCreationInputTokens
				}
			}

		case "content_block_start":
			blk := sseBlock{}
			if evt.ContentBlock != nil {
				blk.Type = evt.ContentBlock.Type
				if evt.ContentBlock.Type == "tool_use" {
					blk.ID = evt.ContentBlock.ID
					blk.Name = evt.ContentBlock.Name
				}
			}
			blocks = append(blocks, blk)
			curBlock = &blocks[len(blocks)-1]

		case "content_block_delta":
			if curBlock == nil {
				continue
			}
			if evt.Delta != nil {
				switch evt.Delta.Type {
				case "text_delta":
					curBlock.Text += evt.Delta.Text
				case "thinking_delta":
					curBlock.Thinking += evt.Delta.Thinking
				case "input_json_delta":
					curBlock.InputJSON += evt.Delta.PartialJSON
				}
			}

		case "content_block_stop":
			curBlock = nil

		case "message_delta":
			if evt.Delta != nil {
				stopReason = evt.Delta.StopReason
			}
			if evt.Usage != nil {
				usage.OutputTokens = evt.Usage.OutputTokens
			}

		case "message_stop":
			// done
		}
	}

	if err := sc.Err(); err != nil {
		return nil, usage, fmt.Errorf("read SSE: %w", err)
	}

	// Build JSON response
	content := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case "thinking":
			content = append(content, map[string]any{"type": "thinking", "thinking": b.Thinking})
		case "tool_use":
			var input any
			if b.InputJSON != "" {
				_ = json.Unmarshal([]byte(b.InputJSON), &input)
			}
			if input == nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    b.ID,
				"name":  b.Name,
				"input": input,
			})
		default:
			if b.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": b.Text})
			}
		}
	}

	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}

	resp := map[string]any{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       model,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":                usage.InputTokens,
			"output_tokens":               usage.OutputTokens,
			"cache_creation_input_tokens": usage.CacheCreationInputTokens,
			"cache_read_input_tokens":     usage.CacheReadInputTokens,
		},
	}

	jsonBytes, err := json.Marshal(resp)
	if err != nil {
		return nil, usage, fmt.Errorf("marshal response: %w", err)
	}
	return jsonBytes, usage, nil
}

// --- SSE parsing helpers ---

type sseEvent struct {
	Type         string         `json:"type"`
	Message      *sseMessage    `json:"message,omitempty"`
	ContentBlock *sseContentBlk `json:"content_block,omitempty"`
	Delta        *sseDelta      `json:"delta,omitempty"`
	Index        *int           `json:"index,omitempty"`
	Usage        *sseUsage      `json:"usage,omitempty"`
}

type sseMessage struct {
	ID    string    `json:"id"`
	Model string    `json:"model"`
	Usage *sseUsage `json:"usage,omitempty"`
}

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type sseContentBlk struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type sseDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type sseBlock struct {
	Type      string
	Text      string
	Thinking  string
	InputJSON string
	ID        string
	Name      string
}
