package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/google/uuid"
)

// OpenCodeGatewayService forwards requests to the OpenCode Go upstream.
//
// OpenCode Go serves two protocols over the same base URL, routed per model:
//   - Anthropic Messages (/v1/messages) — minimax & qwen models. The Anthropic
//     request body is forwarded as-is and the SSE/JSON response is relayed
//     unchanged (lightweight passthrough).
//   - OpenAI Chat Completions (/v1/chat/completions) — everything else. The
//     Anthropic request is converted via the shared apicompat chain and the
//     Chat Completions response is converted back to Anthropic.
//
// Authentication uses a static bearer API key (no token refresh): the chat path
// sends `Authorization: Bearer <key>`, the messages path sends `x-api-key:
// <key>` (matching opencode console's messages.ts parseApiKey).
type OpenCodeGatewayService struct {
	accountRepo AccountRepository
	settingRepo SettingRepository
	serverAddr  string // "host:port" for internal gateway calls
}

// NewOpenCodeGatewayService constructs the service. nil-safe accountRepo for tests;
// nil settingRepo disables the vision-assist feature.
func NewOpenCodeGatewayService(accountRepo AccountRepository, settingRepo SettingRepository, serverAddr string) *OpenCodeGatewayService {
	return &OpenCodeGatewayService{accountRepo: accountRepo, settingRepo: settingRepo, serverAddr: serverAddr}
}

// opencodeCredentialFromAccount decodes the account credentials into an opencode
// credential (api_key + base_url + optional web session).
func opencodeCredentialFromAccount(account *Account) (*opencode.Credential, error) {
	if account == nil {
		return nil, fmt.Errorf("nil account")
	}
	raw, err := json.Marshal(account.Credentials)
	if err != nil {
		return nil, fmt.Errorf("marshal credentials: %w", err)
	}
	var cred opencode.Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("decode opencode credential: %w", err)
	}
	if cred.APIKey == "" {
		return nil, fmt.Errorf("opencode credential missing api_key")
	}
	return &cred, nil
}

// opencodeSSEWriter adapts gin.Context to the opencode.SSEWriter interface,
// writing raw SSE strings with lazy header + flush.
type opencodeSSEWriter struct {
	c           *gin.Context
	flusher     http.Flusher
	wroteHeader bool
	mu          sync.Mutex
}

func (w *opencodeSSEWriter) ensureHeader() {
	if w.wroteHeader {
		return
	}
	w.c.Writer.Header().Set("Content-Type", "text/event-stream")
	w.c.Writer.Header().Set("Cache-Control", "no-cache")
	w.c.Writer.Header().Set("Connection", "keep-alive")
	w.c.Writer.WriteHeader(http.StatusOK)
	w.wroteHeader = true
}

func (w *opencodeSSEWriter) WriteString(s string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureHeader()
	return io.WriteString(w.c.Writer, s)
}

func (w *opencodeSSEWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// dumpSSEWriter 同时写到 inner(发给客户端) 和 dump(写文件供排查)。
type dumpSSEWriter struct {
	inner *opencodeSSEWriter
	dump  *os.File
}

func (d *dumpSSEWriter) WriteString(s string) (int, error) {
	if d.dump != nil {
		_, _ = d.dump.WriteString(s)
	}
	return d.inner.WriteString(s)
}

func (d *dumpSSEWriter) Flush() {
	if d.dump != nil {
		_ = d.dump.Sync()
	}
	d.inner.Flush()
}

// Forward routes the request to the OpenCode Go upstream, choosing the protocol
// by model. It returns a ForwardResult with usage where available.
func (s *OpenCodeGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	start := time.Now()
	reqLog := logger.FromContext(ctx)

	cred, err := opencodeCredentialFromAccount(account)
	if err != nil {
		return nil, fmt.Errorf("opencode credential: %w", err)
	}

	// Peek model + stream flag for logging and protocol routing. Model mapping
	// (credentials.model_mapping) applies before routing so a mapped model's
	// protocol wins.
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	requestedModel := probe.Model
	if mapped := account.GetMappedModel(requestedModel); mapped != "" && mapped != requestedModel {
		reqLog.Info("opencode.model_mapped", zap.String("from", requestedModel), zap.String("to", mapped))
		patched, perr := patchRequestModel(body, mapped)
		if perr == nil {
			body = patched
			probe.Model = mapped
		}
	}

	// Vision assist: describe images for GLM text-only models before forwarding.
	patched, verr := s.maybeProcessVisionAssist(ctx, body, probe.Model, true, reqLog)
	if verr != nil {
		reqLog.Warn("opencode.vision_assist_error", zap.Error(verr))
	} else {
		body = patched
	}

	if opencode.UsesMessagesProtocol(probe.Model) {
		return s.forwardMessages(ctx, c, account, cred, body, requestedModel, probe.Stream, start, reqLog)
	}
	return s.forwardChatCompletions(ctx, c, account, cred, body, requestedModel, probe.Stream, start, reqLog)
}

// forwardMessages sends the Anthropic Messages body to /messages unchanged and
// relays the upstream SSE/JSON response back. Usage is parsed from the SSE
// message_delta / message_stop events; for non-streaming responses the JSON
// body's top-level usage is read.
func (s *OpenCodeGatewayService) forwardMessages(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	cred *opencode.Credential,
	body []byte,
	requestedModel string,
	stream bool,
	start time.Time,
	reqLog *zap.Logger,
) (*ForwardResult, error) {
	url := cred.EffectiveBaseURL() + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("opencode messages new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", cred.APIKey)
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("anthropic-version", c.GetHeader("anthropic-version"))
	if httpReq.Header.Get("anthropic-version") == "" {
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	reqLog.Info("opencode.messages_request_start",
		zap.Int64("account_id", account.ID),
		zap.String("model", requestedModel),
		zap.Bool("stream", stream),
		zap.Int("body_size", len(body)),
		zap.String("base_url", cred.EffectiveBaseURL()),
	)

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, opencode.DefaultRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("opencode http client: %w", err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		reqLog.Warn("opencode.messages_upstream_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, fmt.Errorf("opencode messages upstream request: %w", err)
	}
	defer resp.Body.Close()

	reqLog.Info("opencode.messages_upstream_response",
		zap.Int64("account_id", account.ID),
		zap.Int("status", resp.StatusCode),
		zap.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		reqLog.Warn("opencode.messages_upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", opencode.ParseUpstreamError(errBody)),
		)
		return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: errBody, ResponseHeaders: resp.Header}
	}

	messageID := "msg_" + uuid.NewString()
	result := &ForwardResult{
		RequestID: messageID,
		Model:     requestedModel,
		Stream:    stream,
		Duration:  time.Since(start),
	}

	if stream {
		usage, relayErr := relayAnthropicSSE(resp.Body, c)
		if relayErr != nil {
			reqLog.Warn("opencode.messages_stream_error", zap.Error(relayErr))
		}
		result.Usage = ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
		return result, nil
	}

	// Non-streaming: read the full JSON, relay it, and parse usage.
	upstream, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, fmt.Errorf("opencode messages read response: %w", rerr)
	}
	c.Data(http.StatusOK, "application/json", upstream)
	result.Usage = parseAnthropicUsage(upstream)
	return result, nil
}

// forwardChatCompletions converts the Anthropic request to Chat Completions,
// sends it, and streams the converted Anthropic SSE response back (or a single
// JSON body for non-streaming requests). Mirrors the deepseek path minus the
// non-standard `thinking` object.
func (s *OpenCodeGatewayService) forwardChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	cred *opencode.Credential,
	body []byte,
	requestedModel string,
	_ bool,
	start time.Time,
	reqLog *zap.Logger,
) (*ForwardResult, error) {
	upstreamBody, upstreamModel, stream, err := opencode.BuildUpstreamRequest(body)
	if err != nil {
		return nil, fmt.Errorf("opencode build request: %w", err)
	}

	reqLog.Info("opencode.request_start",
		zap.Int64("account_id", account.ID),
		zap.String("model", requestedModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", stream),
		zap.Int("body_size", len(body)),
		zap.String("base_url", cred.EffectiveBaseURL()),
	)

	url := cred.EffectiveBaseURL() + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("opencode new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, opencode.DefaultRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("opencode http client: %w", err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		reqLog.Warn("opencode.upstream_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, fmt.Errorf("opencode upstream request: %w", err)
	}
	defer resp.Body.Close()

	reqLog.Info("opencode.upstream_response",
		zap.Int64("account_id", account.ID),
		zap.Int("status", resp.StatusCode),
		zap.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		reqLog.Warn("opencode.upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", opencode.ParseUpstreamError(errBody)),
		)
		return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: errBody, ResponseHeaders: resp.Header}
	}

	messageID := "msg_" + uuid.NewString()

	if !stream {
		upstream, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, fmt.Errorf("opencode read response: %w", rerr)
		}
		anthJSON, usage, cerr := opencode.ConvertNonStream(upstream, upstreamModel)
		if cerr != nil {
			return nil, fmt.Errorf("opencode convert response: %w", cerr)
		}
		c.Data(http.StatusOK, "application/json", anthJSON)
		return &ForwardResult{
			RequestID: messageID,
			Model:     requestedModel,
			Stream:    false,
			Duration:  time.Since(start),
			Usage:     ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
		}, nil
	}

	flusher, _ := c.Writer.(http.Flusher)
	sw := &opencodeSSEWriter{c: c, flusher: flusher}
	// DIAG: dump 转换后的 Anthropic SSE 到文件,用于定位 "Content block not found" 根因。
	// 用 messageID 作文件名,只对 opencode CC 转换路径生效。排查完移除。
	var dumpWriter opencode.SSEWriter = sw
	if f, derr := os.OpenFile("/tmp/sse_dump_opencode_"+messageID+".log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); derr == nil {
		defer f.Close()
		dumpWriter = &dumpSSEWriter{inner: sw, dump: f}
	}
	usage, cerr := opencode.ConvertStream(resp.Body, dumpWriter, upstreamModel)
	if cerr != nil {
		reqLog.Warn("opencode.stream_error", zap.Error(cerr))
		if !sw.wroteHeader {
			return nil, fmt.Errorf("opencode stream convert: %w", cerr)
		}
	}
	result := &ForwardResult{
		RequestID: messageID,
		Model:     requestedModel,
		Stream:    true,
		Duration:  time.Since(start),
		Usage:     ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}
	if upstreamModel != requestedModel {
		result.UpstreamModel = upstreamModel
	}
	return result, nil
}

// relayAnthropicSSE copies an Anthropic SSE stream from r to the gin client,
// parsing usage from message_delta/message_start events along the way. Used for
// the Messages-protocol passthrough path where the upstream already speaks
// Anthropic SSE.
func relayAnthropicSSE(r io.Reader, c *gin.Context) (anthropicSSEUsage, error) {
	flusher, _ := c.Writer.(http.Flusher)
	headerWritten := false
	var usage anthropicSSEUsage

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return usage, err
		}
		if !headerWritten {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.WriteHeader(http.StatusOK)
			headerWritten = true
		}
		if _, werr := io.WriteString(c.Writer, line); werr != nil {
			return usage, werr
		}
		if flusher != nil && strings.HasPrefix(line, "data:") {
			flusher.Flush()
		}
		captureAnthropicSSEUsage(line, &usage)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return usage, nil
}

// anthropicSSEUsage is the usage extracted from a relayed Anthropic SSE stream.
type anthropicSSEUsage struct {
	InputTokens  int
	OutputTokens int
}

// captureAnthropicSSEUsage scans a single SSE line for the usage object carried
// by message_start (input_tokens) and message_delta (output_tokens) events.
func captureAnthropicSSEUsage(line string, usage *anthropicSSEUsage) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(line[len("data:"):])
	if data == "" || data == "[DONE]" {
		return
	}
	// message_start carries {message:{usage:{input_tokens,...}}}
	if strings.Contains(data, `"message_start"`) {
		if v := sseUsageInt(data, []string{"message", "usage", "input_tokens"}); v > 0 {
			usage.InputTokens = v
		}
	}
	// message_delta carries {usage:{output_tokens}}
	if strings.Contains(data, `"message_delta"`) {
		if v := sseUsageInt(data, []string{"usage", "output_tokens"}); v > 0 {
			usage.OutputTokens = v
		}
	}
}

// sseUsageInt extracts a nested integer from a JSON data line given a key path.
// Uses gjson-free json.Unmarshal into map[string]any for zero new deps.
func sseUsageInt(data string, path []string) int {
	var node any
	if json.Unmarshal([]byte(data), &node) != nil {
		return 0
	}
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return 0
		}
		node = m[key]
	}
	switch v := node.(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// parseAnthropicUsage reads top-level usage from a non-streaming Anthropic JSON
// response body.
func parseAnthropicUsage(body []byte) ClaudeUsage {
	var resp struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &resp)
	if resp.Usage == nil {
		return ClaudeUsage{}
	}
	return ClaudeUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
}

// TestConnection sends a minimal non-streaming request to the OpenCode upstream
// and emits account-test SSE events (test_start / content / test_complete /
// error), mirroring the deepseek test path. Routes by model protocol.
func (s *OpenCodeGatewayService) TestConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()

	cred, err := opencodeCredentialFromAccount(account)
	if err != nil {
		return s.sendOpenCodeTestError(c, "Invalid OpenCode credential: "+err.Error(), false)
	}
	if modelID == "" {
		modelID = "glm-5.2"
	}
	modelID = opencode.MapModel(modelID)
	if mapped := account.GetMappedModel(modelID); mapped != "" && mapped != modelID {
		modelID = mapped
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	w := &testEventWriter{c: c}
	w.emit(map[string]interface{}{"type": "test_start", "model": modelID})

	if opencode.UsesMessagesProtocol(modelID) {
		return s.testMessages(ctx, c, cred, modelID, w)
	}
	return s.testChatCompletions(ctx, c, cred, modelID, w)
}

// testChatCompletions runs the test request over /chat/completions.
func (s *OpenCodeGatewayService) testChatCompletions(ctx context.Context, c *gin.Context, cred *opencode.Credential, modelID string, w *testEventWriter) error {
	reqBody := map[string]interface{}{
		"model":      modelID,
		"max_tokens": 50,
		"stream":     false,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
	}
	raw, _ := json.Marshal(reqBody)

	url := cred.EffectiveBaseURL() + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return s.sendOpenCodeTestError(c, err.Error(), true)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return s.sendOpenCodeTestError(c, "http client: "+err.Error(), true)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return s.sendOpenCodeTestError(c, "upstream request failed: "+err.Error(), true)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.sendOpenCodeTestError(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, opencode.ParseUpstreamError(bodyBytes)), true)
	}

	var ccResp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bodyBytes, &ccResp); err == nil {
		for _, ch := range ccResp.Choices {
			display := ch.Message.Content
			if display == "" {
				display = ch.Message.ReasoningContent
			}
			if display == "" {
				display = "(empty response)"
			}
			if len(display) > 300 {
				display = display[:300] + "..."
			}
			w.emit(map[string]interface{}{"type": "content", "text": display})
			break
		}
	}
	w.emit(map[string]interface{}{"type": "test_complete", "success": true})
	return nil
}

// testMessages runs the test request over /messages (Anthropic passthrough).
func (s *OpenCodeGatewayService) testMessages(ctx context.Context, c *gin.Context, cred *opencode.Credential, modelID string, w *testEventWriter) error {
	reqBody := map[string]interface{}{
		"model":      modelID,
		"max_tokens": 50,
		"stream":     false,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
	}
	raw, _ := json.Marshal(reqBody)

	url := cred.EffectiveBaseURL() + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return s.sendOpenCodeTestError(c, err.Error(), true)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", cred.APIKey)
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return s.sendOpenCodeTestError(c, "http client: "+err.Error(), true)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return s.sendOpenCodeTestError(c, "upstream request failed: "+err.Error(), true)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.sendOpenCodeTestError(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, opencode.ParseUpstreamError(bodyBytes)), true)
	}

	// Anthropic response: content[].text
	var anthResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	display := "(empty response)"
	if err := json.Unmarshal(bodyBytes, &anthResp); err == nil {
		for _, blk := range anthResp.Content {
			if blk.Type == "text" && blk.Text != "" {
				display = blk.Text
				break
			}
		}
	}
	if len(display) > 300 {
		display = display[:300] + "..."
	}
	w.emit(map[string]interface{}{"type": "content", "text": display})
	w.emit(map[string]interface{}{"type": "test_complete", "success": true})
	return nil
}

func (s *OpenCodeGatewayService) sendOpenCodeTestError(c *gin.Context, msg string, headerWritten bool) error {
	if !headerWritten {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Flush()
	}
	w := &testEventWriter{c: c}
	w.emit(map[string]interface{}{"type": "error", "error": msg})
	return nil
}

// ForwardAsChatCompletions serves an OpenAI Chat Completions (/v1/chat/completions)
// request against an OpenCode Go upstream, returning a Chat Completions response
// so that standard OpenAI clients can use opencode models without adaptation.
//
// Routing mirrors Forward but keeps the client-facing wire format as OpenAI:
//   - chat/completions-protocol models (glm, kimi, deepseek, mimo, hy3): the CC
//     request is forwarded to the upstream /chat/completions AS-IS and the CC
//     SSE/JSON response is relayed unchanged (OpenCode's /chat/completions is
//     already standard OpenAI, so no conversion is needed).
//   - messages-protocol models (minimax, qwen): the CC request is converted to
//     Anthropic (via the shared apicompat chain) and sent to /messages; the
//     Anthropic response is converted back to Chat Completions for the client.
func (s *OpenCodeGatewayService) ForwardAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*ForwardResult, error) {
	start := time.Now()
	reqLog := logger.FromContext(ctx)

	cred, err := opencodeCredentialFromAccount(account)
	if err != nil {
		return nil, fmt.Errorf("opencode credential: %w", err)
	}

	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	requestedModel := probe.Model
	if mapped := account.GetMappedModel(requestedModel); mapped != "" && mapped != requestedModel {
		reqLog.Info("opencode.cc.model_mapped", zap.String("from", requestedModel), zap.String("to", mapped))
		patched, perr := patchRequestModel(body, mapped)
		if perr == nil {
			body = patched
			probe.Model = mapped
		}
	}

	// Vision assist: describe images for GLM text-only models before forwarding.
	patched, verr := s.maybeProcessVisionAssist(ctx, body, probe.Model, false, reqLog)
	if verr != nil {
		reqLog.Warn("opencode.vision_assist_error", zap.Error(verr))
	} else {
		body = patched
	}

	if opencode.UsesMessagesProtocol(probe.Model) {
		return s.forwardCCAsMessages(ctx, c, account, cred, body, requestedModel, probe.Stream, start, reqLog)
	}
	return s.forwardCCPassthrough(ctx, c, account, cred, body, requestedModel, probe.Stream, start, reqLog)
}

// forwardCCPassthrough sends a Chat Completions request to /chat/completions
// unchanged and relays the upstream OpenAI SSE/JSON response back. The upstream
// already speaks standard OpenAI Chat Completions, so no protocol conversion is
// needed — only auth (Bearer) and usage extraction.
func (s *OpenCodeGatewayService) forwardCCPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	cred *opencode.Credential,
	body []byte,
	requestedModel string,
	stream bool,
	start time.Time,
	reqLog *zap.Logger,
) (*ForwardResult, error) {
	url := cred.EffectiveBaseURL() + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("opencode cc new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	reqLog.Info("opencode.cc.request_start",
		zap.Int64("account_id", account.ID),
		zap.String("model", requestedModel),
		zap.Bool("stream", stream),
		zap.Int("body_size", len(body)),
	)

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, opencode.DefaultRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("opencode http client: %w", err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		reqLog.Warn("opencode.cc.upstream_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, fmt.Errorf("opencode cc upstream request: %w", err)
	}
	defer resp.Body.Close()

	reqLog.Info("opencode.cc.upstream_response",
		zap.Int64("account_id", account.ID),
		zap.Int("status", resp.StatusCode),
		zap.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		reqLog.Warn("opencode.cc.upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", opencode.ParseUpstreamError(errBody)),
		)
		return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: errBody, ResponseHeaders: resp.Header}
	}

	result := &ForwardResult{
		RequestID: "chatcmpl_" + uuid.NewString(),
		Model:     requestedModel,
		Stream:    stream,
		Duration:  time.Since(start),
	}

	if stream {
		usage, relayErr := relayChatCompletionsSSE(resp.Body, c)
		if relayErr != nil {
			reqLog.Warn("opencode.cc.stream_error", zap.Error(relayErr))
		}
		result.Usage = ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
		return result, nil
	}

	upstream, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, fmt.Errorf("opencode cc read response: %w", rerr)
	}
	c.Data(http.StatusOK, "application/json", upstream)
	result.Usage = parseChatCompletionsUsage(upstream)
	return result, nil
}

// forwardCCAsMessages converts a Chat Completions request to Anthropic, sends it
// to /messages (the messages-protocol upstream), and converts the Anthropic
// response back to Chat Completions for the client. Used for minimax/qwen models
// which OpenCode serves only over the Anthropic /messages endpoint.
func (s *OpenCodeGatewayService) forwardCCAsMessages(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	cred *opencode.Credential,
	body []byte,
	requestedModel string,
	stream bool,
	start time.Time,
	reqLog *zap.Logger,
) (*ForwardResult, error) {
	// 1. Chat Completions → Responses → Anthropic request
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, fmt.Errorf("opencode cc parse request: %w", err)
	}
	responsesReq, err := apicompat.ChatCompletionsToResponses(&ccReq)
	if err != nil {
		return nil, fmt.Errorf("opencode cc→responses: %w", err)
	}
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("opencode responses→anthropic: %w", err)
	}
	anthropicReq.Stream = stream
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("opencode marshal anthropic: %w", err)
	}

	// 2. Send to /messages (x-api-key auth, matching opencode messages.ts).
	url := cred.EffectiveBaseURL() + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(anthropicBody))
	if err != nil {
		return nil, fmt.Errorf("opencode cc-messages new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", cred.APIKey)
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	reqLog.Info("opencode.cc-messages.request_start",
		zap.Int64("account_id", account.ID),
		zap.String("model", requestedModel),
		zap.Bool("stream", stream),
	)

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, opencode.DefaultRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("opencode http client: %w", err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		reqLog.Warn("opencode.cc-messages.upstream_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, fmt.Errorf("opencode cc-messages upstream request: %w", err)
	}
	defer resp.Body.Close()

	reqLog.Info("opencode.cc-messages.upstream_response",
		zap.Int64("account_id", account.ID),
		zap.Int("status", resp.StatusCode),
		zap.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		reqLog.Warn("opencode.cc-messages.upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", opencode.ParseUpstreamError(errBody)),
		)
		return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: errBody, ResponseHeaders: resp.Header}
	}

	result := &ForwardResult{
		RequestID: "chatcmpl_" + uuid.NewString(),
		Model:     requestedModel,
		Stream:    stream,
		Duration:  time.Since(start),
	}

	// 3. Convert Anthropic response → Responses → Chat Completions and relay.
	if stream {
		usage, relayErr := relayAnthropicSSEAsCC(resp.Body, c, requestedModel)
		if relayErr != nil {
			reqLog.Warn("opencode.cc-messages.stream_error", zap.Error(relayErr))
		}
		result.Usage = ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
		return result, nil
	}

	anthUpstream, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, fmt.Errorf("opencode cc-messages read response: %w", rerr)
	}
	ccOut, usage := convertAnthropicResponseToCC(anthUpstream, requestedModel)
	c.Data(http.StatusOK, "application/json", ccOut)
	result.Usage = usage
	return result, nil
}

// relayChatCompletionsSSE copies an OpenAI Chat Completions SSE stream from r to
// the gin client, parsing usage from the final chunk along the way.
func relayChatCompletionsSSE(r io.Reader, c *gin.Context) (anthropicSSEUsage, error) {
	flusher, _ := c.Writer.(http.Flusher)
	headerWritten := false
	var usage anthropicSSEUsage

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return usage, err
		}
		if !headerWritten {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.WriteHeader(http.StatusOK)
			headerWritten = true
		}
		if _, werr := io.WriteString(c.Writer, line); werr != nil {
			return usage, werr
		}
		if flusher != nil && strings.HasPrefix(line, "data:") {
			flusher.Flush()
		}
		if strings.HasPrefix(line, "data:") {
			captureChatCompletionsSSEUsage(line, &usage)
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	return usage, nil
}

// captureChatCompletionsSSEUsage extracts usage from a Chat Completions SSE data
// line (the final chunk carries usage when stream_options.include_usage is set).
func captureChatCompletionsSSEUsage(line string, usage *anthropicSSEUsage) {
	data := strings.TrimSpace(line[len("data:"):])
	if data == "" || data == "[DONE]" {
		return
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return
	}
	if chunk.Usage != nil {
		usage.InputTokens = chunk.Usage.PromptTokens
		usage.OutputTokens = chunk.Usage.CompletionTokens
	}
}

// parseChatCompletionsUsage reads top-level usage from a non-streaming Chat
// Completions JSON response body.
func parseChatCompletionsUsage(body []byte) ClaudeUsage {
	var resp struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &resp)
	if resp.Usage == nil {
		return ClaudeUsage{}
	}
	return ClaudeUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
}

// convertAnthropicResponseToCC converts a non-streaming Anthropic Messages JSON
// response into a Chat Completions JSON response via the shared apicompat chain
// (Anthropic → Responses → ChatCompletions).
func convertAnthropicResponseToCC(anthBody []byte, model string) ([]byte, ClaudeUsage) {
	var anthResp apicompat.AnthropicResponse
	if err := json.Unmarshal(anthBody, &anthResp); err != nil {
		// Fallback: relay the raw Anthropic body rather than failing hard.
		return anthBody, ClaudeUsage{}
	}
	respResp := apicompat.AnthropicToResponsesResponse(&anthResp)
	ccResp := apicompat.ResponsesToChatCompletions(respResp, model)
	out, err := json.Marshal(ccResp)
	if err != nil {
		return anthBody, ClaudeUsage{}
	}
	var usage ClaudeUsage
	usage.InputTokens = anthResp.Usage.InputTokens
	usage.OutputTokens = anthResp.Usage.OutputTokens
	return out, usage
}

// relayAnthropicSSEAsCC reads an Anthropic Messages SSE stream from r, converts
// each event to Chat Completions SSE via the apicompat chain, and writes it to
// the gin client. Returns usage parsed from the stream.
func relayAnthropicSSEAsCC(r io.Reader, c *gin.Context, model string) (anthropicSSEUsage, error) {
	flusher, _ := c.Writer.(http.Flusher)
	headerWritten := false
	var usage anthropicSSEUsage

	// Anthropic SSE → Responses events → (finalize) → ChatCompletions chunks.
	// We buffer Responses events and convert them at finalize, since the
	// apicompat chain does not expose a per-event Responses→CC stream converter.
	anthState := apicompat.NewAnthropicEventToResponsesState()
	var respEvents []apicompat.ResponsesStreamEvent

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return usage, err
		}
		if !headerWritten {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.WriteHeader(http.StatusOK)
			headerWritten = true
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var evt apicompat.AnthropicStreamEvent
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		if evt.Type == "message_start" {
			if m := evt.Message; m != nil {
				usage.InputTokens = m.Usage.InputTokens
			}
		}
		if evt.Type == "message_delta" {
			if u := evt.Usage; u != nil {
				usage.OutputTokens = u.OutputTokens
			}
		}
		evs := apicompat.AnthropicEventToResponsesEvents(&evt, anthState)
		respEvents = append(respEvents, evs...)
	}
	respEvents = append(respEvents, apicompat.FinalizeAnthropicResponsesStream(anthState)...)

	// Convert collected Responses events into Chat Completions SSE chunks via
	// the stateful ResponsesEventToChatChunks converter, then finalize.
	ccState := apicompat.NewResponsesEventToChatState()
	for _, ev := range respEvents {
		chunks := apicompat.ResponsesEventToChatChunks(&ev, ccState)
		for _, ch := range chunks {
			raw, _ := json.Marshal(ch)
			if _, werr := io.WriteString(c.Writer, "data: "+string(raw)+"\n\n"); werr != nil {
				return usage, werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	for _, ch := range apicompat.FinalizeResponsesChatStream(ccState) {
		raw, _ := json.Marshal(ch)
		if _, werr := io.WriteString(c.Writer, "data: "+string(raw)+"\n\n"); werr != nil {
			return usage, werr
		}
	}
	if _, werr := io.WriteString(c.Writer, "data: [DONE]\n\n"); werr != nil {
		return usage, werr
	}
	if flusher != nil {
		flusher.Flush()
	}
	return usage, nil
}

// isGLMTextOnlyModel reports whether a model is a GLM text-only model that
// needs vision assistance (glm-5.2, glm-5.1, glm-5).
func isGLMTextOnlyModel(model string) bool {
	m := opencode.NormalizeModelID(model)
	switch m {
	case "glm-5.2", "glm-5.1", "glm-5":
		return true
	}
	return false
}

// MaybeProcessVisionAssistPublic exposes vision assist for the standard
// Anthropic passthrough path (gateway_handler uses this for GLM text-only
// accounts that don't go through the opencode gateway).
func (s *OpenCodeGatewayService) MaybeProcessVisionAssistPublic(
	ctx context.Context, body []byte, model string, isAnthropicFormat bool, reqLog *zap.Logger,
) ([]byte, error) {
	return s.maybeProcessVisionAssist(ctx, body, model, isAnthropicFormat, reqLog)
}

// maybeProcessVisionAssist checks whether the request targets a GLM text-only
// model with image content, and if vision assist is enabled in settings,
// replaces all image blocks with text descriptions obtained via an internal
// OpenAI gateway call.
//
// When the feature is disabled or the model is not a GLM text-only model, the
// original body is returned unchanged. On errors (e.g. vision call fails), the
// original body is also returned so the upstream can surface its own error.
func (s *OpenCodeGatewayService) maybeProcessVisionAssist(
	ctx context.Context,
	body []byte,
	model string,
	isAnthropicFormat bool,
	reqLog *zap.Logger,
) ([]byte, error) {
	if s.settingRepo == nil {
		reqLog.Warn("vision_assist.skip", zap.String("reason", "settingRepo_nil"))
		return body, nil
	}
	if !isGLMTextOnlyModel(model) {
		reqLog.Warn("vision_assist.skip", zap.String("reason", "not_glm_text_only"), zap.String("model", model))
		return body, nil
	}

	enabled, _ := s.settingRepo.GetValue(ctx, SettingKeyOpenCodeVisionAssistEnabled)
	if enabled != "true" {
		reqLog.Warn("vision_assist.skip", zap.String("reason", "disabled"), zap.String("enabled", enabled))
		return body, nil
	}

	visionModel, _ := s.settingRepo.GetValue(ctx, SettingKeyOpenCodeVisionModel)
	if visionModel == "" {
		visionModel = "gpt-5.4"
	}

	reqLog.Info("vision_assist.enter",
		zap.String("model", model),
		zap.Bool("is_anthropic", isAnthropicFormat),
		zap.Int("body_size", len(body)),
		zap.String("vision_model", visionModel),
	)

	if isAnthropicFormat {
		return s.replaceAnthropicImages(ctx, body, visionModel, reqLog)
	}
	return s.replaceChatCompletionsImages(ctx, body, visionModel, reqLog)
}

// visionImageBlock represents a detected image in a message body, along with
// the message index and block index for replacement.
// inToolResult=true 时, blockIdx 指向一个 tool_result block, nestedIdx 指向该
// tool_result.content 数组里 image 的位置 — 替换时只动 nestedIdx,保留 tool_result 外壳。
type visionImageBlock struct {
	msgIdx       int
	blockIdx     int
	inToolResult bool
	nestedIdx    int
	base64       string
	mediaType    string
}

// visionEncoderPrompt instructs the vision model to output structured visual
// tokens that mimic a vision encoder's output, optimized for text-only LLM
// consumption. Follows Zhipu's <|image|> placeholder token approach.
const visionEncoderPrompt = `You are a vision encoder. Convert this image into structured visual tokens that a text-only LLM can process. Output ONLY the following format:

<|image_start|>
[TYPE] describe the image type (photo/screenshot/chart/document/diagram/ui/illustration/other)
[OCR]
- list every visible text character-by-character with position
- format: "text" @ position (top-left/center/bottom-right/etc)
[OBJECTS]
- list each distinct object/region with: name | position | size | key visual attributes (color, shape, state)
[COLOR]
- dominant colors with hex codes if identifiable
[LAYOUT]
- spatial arrangement: what is at top/middle/bottom, left/center/right
[SUMMARY]
- one sentence summarizing the whole image
<|image_end|>

CRITICAL: Output ONLY the structured tokens above. Do NOT add any other text, explanations, or markdown.`

// visionDescCache 缓存图片描述结果,避免同一张图(相同 base64)在多轮请求中重复描述。
// Claude Code 每轮请求带完整历史,历史里的图片每次都会重新触发 vision assist,
// 不缓存的话 N 轮请求要重复描述 N 次同一张图。
//
// 淘汰策略(按真实使用场景优化):
//   - 30 小时无访问清理:覆盖 24H 内重启会话的场景,会话压缩后图片变孤儿,
//     30 小时后自动清理。
//   - 容量上限 20000 条(~140MB)LRU 兜底:缓存增长快时,满容量优先淘汰
//     最久没访问的(孤儿项),不用等满 30 小时。
//   - 后台 goroutine 每 30 分钟扫描清理过期项。
var visionDescCache = struct {
	sync.RWMutex
	m map[string]descEntry
}{m: make(map[string]descEntry)}

// visionInflight 单飞:同一张图(相同 base64)并行 describe 时只调一次视觉模型,
// 其他请求等结果。避免历史里重复出现的同一张图被并行描述多次。
var visionInflight sync.Map // cacheKeyStr -> *sync.Mutex

type descEntry struct {
	desc       string
	lastAccess time.Time // 每次命中刷新,用于空闲清理
}

const (
	visionDescCacheIdleTTL      = 30 * time.Hour   // 30 小时无访问清理
	visionDescCacheMaxEntries   = 20000            // 容量上限,满了 LRU 淘汰
	visionDescCacheScanInterval = 30 * time.Minute // 后台扫描间隔
)

// visionCacheOnce 控制后台清理 goroutine 只启动一次。
var visionCacheOnce sync.Once

// startVisionCacheCleaner 启动后台 goroutine 定期清理过期缓存项。
func startVisionCacheCleaner() {
	visionCacheOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(visionDescCacheScanInterval)
			defer ticker.Stop()
			for range ticker.C {
				now := time.Now()
				visionDescCache.Lock()
				for k, v := range visionDescCache.m {
					if now.Sub(v.lastAccess) > visionDescCacheIdleTTL {
						delete(visionDescCache.m, k)
					}
				}
				visionDescCache.Unlock()
			}
		}()
	})
}

// describeImageViaOpenAI sends a single image to the configured OpenAI group's
// gpt-5.4 (or configured vision model) and returns a text description.
func (s *OpenCodeGatewayService) describeImageViaOpenAI(
	ctx context.Context,
	visionModel string,
	base64Data string,
	mediaType string,
	reqLog *zap.Logger,
) (string, error) {
	// 启动后台清理 goroutine(只启动一次)。
	startVisionCacheCleaner()

	// 缓存命中检查:同一张图(base64 相同)在 30 小时内有访问过就直接返回。
	// 命中时刷新 lastAccess,活跃会话的图片永不清理。
	cacheKey := sha256.Sum256([]byte(base64Data))
	cacheKeyStr := hex.EncodeToString(cacheKey[:])
	now := time.Now()
	visionDescCache.RLock()
	if entry, ok := visionDescCache.m[cacheKeyStr]; ok && now.Sub(entry.lastAccess) < visionDescCacheIdleTTL {
		visionDescCache.RUnlock()
		// 刷新 lastAccess(用写锁,频率低不会成为瓶颈)。
		visionDescCache.Lock()
		if e, ok := visionDescCache.m[cacheKeyStr]; ok {
			e.lastAccess = time.Now()
			visionDescCache.m[cacheKeyStr] = e
		}
		visionDescCache.Unlock()
		reqLog.Info("vision_assist.cache_hit",
			zap.Int("desc_len", len(entry.desc)),
		)
		return entry.desc, nil
	}
	visionDescCache.RUnlock()

	// 单飞:同一张图并行 describe 时,只让第一个请求调视觉模型,其他等结果。
	// 否则同一请求里 5 个相同 image block 会并发调 5 次视觉模型。
	muIface, _ := visionInflight.LoadOrStore(cacheKeyStr, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	// 拿到锁后再次检查缓存:可能其他请求已经 describe 完写入了。
	visionDescCache.RLock()
	if entry, ok := visionDescCache.m[cacheKeyStr]; ok && time.Now().Sub(entry.lastAccess) < visionDescCacheIdleTTL {
		visionDescCache.RUnlock()
		visionDescCache.Lock()
		if e, ok := visionDescCache.m[cacheKeyStr]; ok {
			e.lastAccess = time.Now()
			visionDescCache.m[cacheKeyStr] = e
		}
		visionDescCache.Unlock()
		reqLog.Info("vision_assist.cache_hit_after_wait",
			zap.Int("desc_len", len(entry.desc)),
		)
		return entry.desc, nil
	}
	visionDescCache.RUnlock()

	visionAPIKey, _ := s.settingRepo.GetValue(ctx, SettingKeyOpenCodeVisionAPIKey)
	if visionAPIKey == "" {
		return "", fmt.Errorf("vision api key not configured")
	}

	reqBody := map[string]interface{}{
		"model":       visionModel,
		"max_tokens":  1024,
		"temperature": 0,
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": visionEncoderPrompt,
					},
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": "data:" + mediaType + ";base64," + base64Data,
						},
					},
				},
			},
		},
	}

	raw, _ := json.Marshal(reqBody)
	baseURL, _ := s.settingRepo.GetValue(ctx, SettingKeyOpenCodeVisionAPIBaseURL)
	if baseURL == "" {
		baseURL = "http://" + s.serverAddr
	}
	gatewayURL := baseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", gatewayURL, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("vision request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+visionAPIKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("vision upstream: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vision upstream returned %d: %s", resp.StatusCode, string(respBody))
	}

	var ccResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &ccResp); err != nil {
		return "", fmt.Errorf("vision parse response: %w", err)
	}
	if len(ccResp.Choices) == 0 {
		return "", fmt.Errorf("vision response empty")
	}
	desc := ccResp.Choices[0].Message.Content
	if desc == "" {
		return "", fmt.Errorf("vision returned empty description")
	}

	// 写入缓存:同一张图下次请求直接命中,不重复调用视觉模型。
	visionDescCache.Lock()
	visionDescCache.m[cacheKeyStr] = descEntry{desc: desc, lastAccess: time.Now()}
	// LRU 容量兜底:超过上限时淘汰最久没访问的(通常是会话压缩后的孤儿图片)。
	if len(visionDescCache.m) > visionDescCacheMaxEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range visionDescCache.m {
			if oldestKey == "" || v.lastAccess.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastAccess
			}
		}
		delete(visionDescCache.m, oldestKey)
	}
	visionDescCache.Unlock()

	reqLog.Info("vision_assist.image_described",
		zap.Int("desc_len", len(desc)),
	)
	return desc, nil
}

// replaceAnthropicImages walks the messages array in an Anthropic-format body,
// detects image content blocks, describes them via the vision model, and
// replaces them with text blocks. Returns the modified body.
func (s *OpenCodeGatewayService) replaceAnthropicImages(
	ctx context.Context,
	body []byte,
	visionModel string,
	reqLog *zap.Logger,
) ([]byte, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return body, fmt.Errorf("parse anthropic body: %w", err)
	}

	var images []visionImageBlock
	userMsgCount := 0
	topLevelImages := 0
	toolResultImages := 0
	blockTypeCounts := map[string]int{}
	for mi, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		userMsgCount++
		var blocks []apicompat.AnthropicContentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			continue // content is a string, no images
		}
		for bi, blk := range blocks {
			blockTypeCounts[blk.Type]++
			if blk.Type == "image" && blk.Source != nil && blk.Source.Type == "base64" {
				topLevelImages++
				images = append(images, visionImageBlock{
					msgIdx: mi, blockIdx: bi,
					base64: blk.Source.Data, mediaType: blk.Source.MediaType,
				})
			}
			// tool_result.content 里可能嵌套 image block(Claude Code 工具返回截图等)
			if blk.Type == "tool_result" && len(blk.Content) > 0 {
				var nestedBlocks []apicompat.AnthropicContentBlock
				if err := json.Unmarshal(blk.Content, &nestedBlocks); err == nil {
					for ni, nb := range nestedBlocks {
						blockTypeCounts["tool_result."+nb.Type]++
						if nb.Type == "image" && nb.Source != nil && nb.Source.Type == "base64" {
							toolResultImages++
							images = append(images, visionImageBlock{
								msgIdx: mi, blockIdx: bi,
								inToolResult: true, nestedIdx: ni,
								base64: nb.Source.Data, mediaType: nb.Source.MediaType,
							})
						}
					}
				}
			}
		}
	}

	reqLog.Info("vision_assist.scan",
		zap.Int("user_msgs", userMsgCount),
		zap.Int("top_level_images", topLevelImages),
		zap.Int("tool_result_images", toolResultImages),
		zap.Any("block_types", blockTypeCounts),
	)

	if len(images) == 0 {
		return body, nil
	}

	return s.replaceImagesInBody(ctx, body, images, visionModel, reqLog)
}

// replaceImagesInBody replaces image blocks in the raw JSON body with text
// descriptions obtained from the vision model. It works with map[string]any
// for maximum flexibility across Anthropic and Chat Completions formats.
func (s *OpenCodeGatewayService) replaceImagesInBody(
	ctx context.Context,
	body []byte,
	images []visionImageBlock,
	visionModel string,
	reqLog *zap.Logger,
) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, fmt.Errorf("parse body: %w", err)
	}

	msgs, ok := root["messages"].([]any)
	if !ok {
		return body, nil
	}

	// 并行描述所有图片,但限制并发数:不限制时 9 张图同时打上游,
	// 上游排队导致单张从 10-20s 飙到 60-70s。限制到 3 并发,
	// 上游不超载,单张保持 10-20s,总时间 ~30-60s 而非 70s+。
	descs := make([]string, len(images))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3) // 最大并发数
	for i, img := range images {
		wg.Add(1)
		go func(idx int, im visionImageBlock) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			desc, err := s.describeImageViaOpenAI(ctx, visionModel, im.base64, im.mediaType, reqLog)
			if err != nil {
				reqLog.Warn("vision_assist.describe_failed",
					zap.Int("msg_idx", im.msgIdx),
					zap.Int("block_idx", im.blockIdx),
					zap.Error(err),
				)
				descs[idx] = "[Image description unavailable: vision assist failed — " + err.Error() + "]"
				return
			}
			descs[idx] = desc
		}(i, img)
	}
	wg.Wait()

	for i, img := range images {
		desc := descs[i]
		msg, ok := msgs[img.msgIdx].(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok || img.blockIdx >= len(blocks) {
			continue
		}
		descText := "[Image description: " + desc + "]"

		if img.inToolResult {
			// 只替换 tool_result.content 数组里的 image,保留 tool_result 外壳和 tool_use_id,
			// 不破坏 tool_use/tool_result 配对结构(否则 Claude Code 后续轮次会报错)。
			tr, ok := blocks[img.blockIdx].(map[string]any)
			if !ok {
				continue
			}
			nested, ok := tr["content"].([]any)
			if !ok || img.nestedIdx >= len(nested) {
				continue
			}
			nested[img.nestedIdx] = map[string]any{
				"type": "text",
				"text": descText,
			}
		} else {
			blocks[img.blockIdx] = map[string]any{
				"type": "text",
				"text": descText,
			}
		}
	}

	patched, err := json.Marshal(root)
	if err != nil {
		return body, fmt.Errorf("marshal patched body: %w", err)
	}

	reqLog.Info("vision_assist.images_replaced",
		zap.Int("image_count", len(images)),
		zap.String("vision_model", visionModel),
	)
	return patched, nil
}

// replaceChatCompletionsImages walks the messages array in an OpenAI Chat
// Completions format body, detects image_url content parts, and replaces them
// with text descriptions via the vision model.
func (s *OpenCodeGatewayService) replaceChatCompletionsImages(
	ctx context.Context,
	body []byte,
	visionModel string,
	reqLog *zap.Logger,
) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, fmt.Errorf("parse cc body: %w", err)
	}

	msgs, ok := root["messages"].([]any)
	if !ok {
		return body, nil
	}

	var images []visionImageBlock
	for mi, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		contentAny := msg["content"]
		switch content := contentAny.(type) {
		case []any:
			for bi, part := range content {
				partMap, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := partMap["type"].(string); t != "image_url" {
					continue
				}
				imgURL, ok := partMap["image_url"].(map[string]any)
				if !ok {
					continue
				}
				url, _ := imgURL["url"].(string)
				if url == "" {
					continue
				}
				base64, mediaType := parseDataURL(url)
				if base64 == "" {
					continue
				}
				images = append(images, visionImageBlock{
					msgIdx: mi, blockIdx: bi,
					base64: base64, mediaType: mediaType,
				})
			}
		case string:
			continue
		}
	}

	if len(images) == 0 {
		return body, nil
	}

	return s.replaceImagesInBody(ctx, body, images, visionModel, reqLog)
}

// parseDataURL extracts the base64 data and media type from a data: URL.
// Returns ("", "") if the input is not a valid data: URL.
func parseDataURL(url string) (base64, mediaType string) {
	if !strings.HasPrefix(url, "data:") {
		return "", ""
	}
	// data:image/png;base64,ABCD...
	rest := url[5:]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", ""
	}
	meta := rest[:comma]
	base64 = rest[comma+1:]
	semi := strings.Index(meta, ";")
	if semi >= 0 {
		mediaType = meta[:semi]
	} else {
		mediaType = meta
	}
	return
}
