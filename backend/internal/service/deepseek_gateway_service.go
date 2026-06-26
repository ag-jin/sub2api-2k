package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/deepseek"
	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

// DeepseekGatewayService forwards Anthropic Messages requests to DeepSeek
// (OpenAI Chat Completions) upstreams — DeepSeek official and OpenCode Go. It
// mirrors KiroGatewayService but uses a static bearer API key (no token
// refresh) and reuses the apicompat conversion chain via the deepseek package.
type DeepseekGatewayService struct {
	accountRepo AccountRepository
}

// NewDeepseekGatewayService constructs the service. nil-safe accountRepo for tests.
func NewDeepseekGatewayService(accountRepo AccountRepository) *DeepseekGatewayService {
	return &DeepseekGatewayService{accountRepo: accountRepo}
}

// deepseekCredentialFromAccount decodes the account credentials into a deepseek
// credential (api_key + base_url).
func deepseekCredentialFromAccount(account *Account) (*deepseek.Credential, error) {
	if account == nil {
		return nil, fmt.Errorf("nil account")
	}
	raw, err := json.Marshal(account.Credentials)
	if err != nil {
		return nil, fmt.Errorf("marshal credentials: %w", err)
	}
	var cred deepseek.Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("decode deepseek credential: %w", err)
	}
	if cred.APIKey == "" {
		return nil, fmt.Errorf("deepseek credential missing api_key")
	}
	return &cred, nil
}

// deepseekSSEWriter adapts gin.Context to the deepseek.SSEWriter interface,
// writing raw Anthropic SSE strings with lazy header + flush.
type deepseekSSEWriter struct {
	c           *gin.Context
	flusher     http.Flusher
	wroteHeader bool
	mu          sync.Mutex
}

func (w *deepseekSSEWriter) ensureHeader() {
	if w.wroteHeader {
		return
	}
	w.c.Writer.Header().Set("Content-Type", "text/event-stream")
	w.c.Writer.Header().Set("Cache-Control", "no-cache")
	w.c.Writer.Header().Set("Connection", "keep-alive")
	w.c.Writer.WriteHeader(http.StatusOK)
	w.wroteHeader = true
}

func (w *deepseekSSEWriter) WriteString(s string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureHeader()
	return io.WriteString(w.c.Writer, s)
}

func (w *deepseekSSEWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// Forward converts the Anthropic request to DeepSeek Chat Completions, sends it,
// and streams the converted Anthropic SSE response back (or a single JSON body
// for non-streaming requests).
func (s *DeepseekGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	start := time.Now()
	reqLog := logger.FromContext(ctx)

	cred, err := deepseekCredentialFromAccount(account)
	if err != nil {
		return nil, fmt.Errorf("deepseek credential: %w", err)
	}

	// Account-level model mapping (credentials.model_mapping) — lets the edit-page
	// model restriction apply before conversion.
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	if mapped := account.GetMappedModel(probe.Model); mapped != "" && mapped != probe.Model {
		reqLog.Info("deepseek.model_mapped", zap.String("from", probe.Model), zap.String("to", mapped))
		patched, perr := patchRequestModel(body, mapped)
		if perr == nil {
			body = patched
		}
	}

	upstreamBody, upstreamModel, stream, err := deepseek.BuildUpstreamRequest(body)
	if err != nil {
		return nil, fmt.Errorf("deepseek build request: %w", err)
	}

	reqLog.Info("deepseek.request_start",
		zap.Int64("account_id", account.ID),
		zap.String("model", probe.Model),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", stream),
		zap.Int("body_size", len(body)),
		zap.String("base_url", cred.EffectiveBaseURL()),
	)

	url := cred.EffectiveBaseURL() + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("deepseek new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	client, err := kiropkg.BuildHTTPClientExported(cred.ProxyURL, deepseek.DefaultRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("deepseek http client: %w", err)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		reqLog.Warn("deepseek.upstream_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, fmt.Errorf("deepseek upstream request: %w", err)
	}
	defer resp.Body.Close()

	reqLog.Info("deepseek.upstream_response",
		zap.Int64("account_id", account.ID),
		zap.Int("status", resp.StatusCode),
		zap.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		reqLog.Warn("deepseek.upstream_error",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("body", deepseek.ParseUpstreamError(errBody)),
		)
		return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: errBody}
	}

	messageID := "msg_" + uuid.NewString()

	if !stream {
		upstream, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, fmt.Errorf("deepseek read response: %w", rerr)
		}
		anthJSON, usage, cerr := deepseek.ConvertNonStream(upstream, upstreamModel)
		if cerr != nil {
			return nil, fmt.Errorf("deepseek convert response: %w", cerr)
		}
		c.Data(http.StatusOK, "application/json", anthJSON)
		return &ForwardResult{
			RequestID: messageID,
			Model:     probe.Model,
			Stream:    false,
			Duration:  time.Since(start),
			Usage:     ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
		}, nil
	}

	flusher, _ := c.Writer.(http.Flusher)
	sw := &deepseekSSEWriter{c: c, flusher: flusher}
	usage, cerr := deepseek.ConvertStream(resp.Body, sw, upstreamModel)
	if cerr != nil {
		reqLog.Warn("deepseek.stream_error", zap.Error(cerr))
		if !sw.wroteHeader {
			return nil, fmt.Errorf("deepseek stream convert: %w", cerr)
		}
	}
	result := &ForwardResult{
		RequestID: messageID,
		Model:     probe.Model,
		Stream:    true,
		Duration:  time.Since(start),
		Usage:     ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}
	if upstreamModel != probe.Model {
		result.UpstreamModel = upstreamModel
	}
	return result, nil
}

// patchRequestModel rewrites the top-level "model" field in an Anthropic request body.
func patchRequestModel(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	m, _ := json.Marshal(model)
	obj["model"] = m
	return json.Marshal(obj)
}

// TestConnection sends a minimal non-streaming /chat/completions request to the
// DeepSeek upstream and emits account-test SSE events (test_start / content /
// test_complete / error), mirroring KiroGatewayService.TestConnection.
func (s *DeepseekGatewayService) TestConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()

	cred, err := deepseekCredentialFromAccount(account)
	if err != nil {
		return s.sendDeepseekTestError(c, "Invalid DeepSeek credential: "+err.Error(), false)
	}
	if modelID == "" {
		modelID = "deepseek-v4-pro"
		if account != nil && account.Platform == PlatformOpenCode {
			modelID = "glm-5"
		}
	}
	modelID = deepseek.MapModel(modelID)

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	w := &testEventWriter{c: c}
	w.emit(map[string]interface{}{"type": "test_start", "model": modelID})

	reqBody := map[string]interface{}{
		"model":      modelID,
		"max_tokens": 50,
		"stream":     false,
		"thinking":   map[string]string{"type": "disabled"},
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
	}
	raw, _ := json.Marshal(reqBody)

	url := cred.EffectiveBaseURL() + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return s.sendDeepseekTestError(c, err.Error(), true)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)

	client, err := kiropkg.BuildHTTPClientExported(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return s.sendDeepseekTestError(c, "http client: "+err.Error(), true)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return s.sendDeepseekTestError(c, "upstream request failed: "+err.Error(), true)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.sendDeepseekTestError(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, deepseek.ParseUpstreamError(bodyBytes)), true)
	}

	// Emit the actual response text as a content event so the admin UI shows
	// what the model replied, not just "test complete". Try content first, then
	// reasoning_content (thinking models may put output there even with
	// thinking:disabled).
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
			// Truncate for display
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

func (s *DeepseekGatewayService) sendDeepseekTestError(c *gin.Context, msg string, headerWritten bool) error {
	if !headerWritten {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Flush()
	}
	w := &testEventWriter{c: c}
	w.emit(map[string]interface{}{"type": "error", "error": msg})
	return nil
}
