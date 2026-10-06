package service

// 国产供应商（kimi/zhipu/deepseek）原生 Anthropic 端点直通路径。
//
// 当账号 credentials["api_protocol"] = "anthropic" 时，入站 /v1/messages 请求
// 不再做 Anthropic→CC→Anthropic 双重转换，而是零转换直通供应商的官方
// Anthropic 兼容端点（如 https://open.bigmodel.cn/api/anthropic/v1/messages），
// 适配 Claude Code 等原生 Anthropic 客户端。转发骨架以
// gateway_anthropic_passthrough.go 的 APIKey 透传为模板（字节级 SSE 中继 +
// usage 解析），错误/failover 语义对齐 OpenAI 网关其他路径
// （failoverOpenAIUpstreamHTTPError / handleAnthropicErrorResponse）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// forwardAnthropicViaNativeAnthropicEndpoint 将 Anthropic Messages 请求零转换
// 直通到国产供应商的原生 Anthropic 端点。仅做模型名映射与少量 body 清洗
// （空文本块 / web-search 历史块），协议本身不转换。
func (s *OpenAIGatewayService) forwardAnthropicViaNativeAnthropicEndpoint(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	originalModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if originalModel == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := gjson.GetBytes(body, "stream").Bool()

	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	if upstreamModel != originalModel {
		rewritten, err := sjson.SetBytes(body, "model", upstreamModel)
		if err != nil {
			return nil, fmt.Errorf("rewrite model: %w", err)
		}
		body = rewritten
	}
	if normalized, changed := NormalizeGLM53AnthropicThinking(body, upstreamModel); changed {
		body = normalized
	}

	// 记录客户端请求的推理强度：优先 Claude 协议的 output_config.effort；
	// 缺失且 thinking 已启用时，按国产 passback-required 模型兜底为 high
	// （对齐 Anthropic 网关 gateway_handler 的记录语义，避免该路径长期落 NULL）。
	requestedReasoningEffort := NormalizeClaudeOutputEffort(gjson.GetBytes(body, "output_config.effort").String())
	reasoningEffort := ApplyThinkingEnabledFallback(
		requestedReasoningEffort,
		body,
		billingModel,
	)

	// 与 Anthropic 平台 passthrough 相同的 pre-filter：剥离空文本块与上游
	// 无法接受的 web-search 历史块（GLM/Kimi/DeepSeek 对 server_tool_use 400）。
	body = StripEmptyTextBlocks(body)
	body = FilterWebSearchHistoryBlocks(body, upstreamModel)

	logger.LegacyPrintf("service.gateway", "[CN Anthropic 直通] account=%d(%s) platform=%s model=%s upstream=%s stream=%v",
		account.ID, account.Name, account.Platform, originalModel, upstreamModel, clientStream)

	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	targetURL, err := s.nativeAnthropicTargetURL(account)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.sendNativeAnthropicUpstreamRequest(ctx, c, account, body, apiKey, targetURL, proxyURL, clientStream)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		// 非 failover 错误：经共享 compat handler 以 Anthropic 格式回写
		// （透传规则、ops 记录、cyber_policy 与 CC 回退路径一致）。
		return s.handleAnthropicErrorResponse(resp, c, account, billingModel)
	}

	if clientStream {
		return s.handleNativeAnthropicStreamingResponse(ctx, resp, c, account, originalModel, billingModel, upstreamModel, reasoningEffort, startTime)
	}
	return s.handleNativeAnthropicBufferedResponse(ctx, resp, c, account, originalModel, billingModel, upstreamModel, reasoningEffort, startTime)
}

// sendNativeAnthropicUpstreamRequest 构建（含签名注入）并发送原生 Anthropic 上游
// 请求，并在响应命中 VERIFY_* 时执行签名自愈（design M3.1(a) / 票 23）：作废私钥 →
// 重新握手（singleflight 与每 key 退避由票 20 的 Signer 保证）→ 用同一请求体重放
// **恰好一次**（重放走同一构建闭包，重新签名；单次重放由结构保证，不循环）。
//
// 三条原生直通入口（/v1/messages、/v1/chat/completions、/v1/responses）共用本函数：
// 签名挂点只有一个（buildNativeAnthropicUpstreamRequest），自愈也必须同层，否则
// 未改的入口会在 VERIFY_* 后带着作废私钥一直失败到私钥 TTL 到期。
//
// 错误语义与既有内联实现一致：构建失败原样返回（尚未发出请求，含票 24 的 closed
// 策略可 failover 错误）；传输失败（含重放阶段的传输失败）经
// handleOpenAIUpstreamTransportError 归一为既有 failover 语义；重放仍失败且策略为
// closed 时交回自愈编排给出的可 failover 错误（换账号/渠道，不下发该响应）。
func (s *OpenAIGatewayService) sendNativeAnthropicUpstreamRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	apiKey string,
	targetURL string,
	proxyURL string,
	stream bool,
	sessionBodies ...[]byte,
) (*http.Response, error) {
	build := func() (*http.Request, bool, error) {
		upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, stream)
		upstreamReq, _, signed, err := s.buildNativeAnthropicUpstreamRequest(upstreamCtx, c, account, body, apiKey, targetURL, sessionBodies...)
		releaseUpstreamCtx()
		return upstreamReq, signed, err
	}

	upstreamReq, signed, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
	}
	if resp == nil || resp.StatusCode < 400 || !signed {
		// signed=false：本次没带签名（门控关闭或 fail-open），没有可作废的私钥。
		return resp, nil
	}

	// 检测点唯一：复用错误体接缝（body 读后回卷，调用方仍可重读）。
	errorBody, _ := s.readOpenAIUpstreamError(resp)
	outcome := s.zhipuSignSelfHealVerifyFailure(ctx, account, signed, resp, errorBody, func() (*http.Response, error) {
		replayReq, _, replayErr := build()
		if replayErr != nil {
			return nil, replayErr
		}
		return s.doOpenAIUpstream(replayReq, proxyURL, account)
	})
	if !outcome.Replayed {
		return resp, nil
	}
	if outcome.FailoverErr != nil {
		// fail 策略 closed（票 24）：重放仍失败，不下发该响应，换账号/渠道重试。
		if outcome.Resp != nil && outcome.Resp.Body != nil {
			_ = outcome.Resp.Body.Close()
		}
		return nil, outcome.FailoverErr
	}
	if outcome.ReplayErr != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, outcome.ReplayErr, true)
	}
	return outcome.Resp, nil
}

// nativeAnthropicTargetURL 组装国产供应商原生 Anthropic messages 端点。
// 第三方端点保持朴素路径，不附加 ?beta=true。
func (s *OpenAIGatewayService) nativeAnthropicTargetURL(account *Account) (string, error) {
	baseURL := strings.TrimSpace(account.GetAnthropicProtocolBaseURL())
	if baseURL == "" {
		return "", fmt.Errorf("account %d has no anthropic protocol base url", account.ID)
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	if account.IsOpenCodeGo() {
		// OpenCode Go 的 Chat Completions base 带 /v1；用版本感知拼接避免 /v1/v1/messages。
		return buildOpenAIEndpointURL(validatedURL, "/v1/messages"), nil
	}
	return strings.TrimRight(validatedURL, "/") + "/v1/messages", nil
}

func resolveOpenCodeGoMappedModel(account *Account, body []byte, defaultMappedModel string) string {
	original := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	billing := resolveOpenAIForwardModel(account, original, defaultMappedModel)
	return normalizeOpenAIModelForUpstream(account, billing)
}

// buildNativeAnthropicUpstreamRequest 构建原生 Anthropic 上游请求；signed 表示本次
// 请求确实带了智谱签名（自愈状态机据此判定 VERIFY_* 是否与自己的私钥有关，design
// M3.1(a) / 票 23）。
func (s *OpenAIGatewayService) buildNativeAnthropicUpstreamRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	apiKey string,
	targetURL string,
	sessionBodies ...[]byte,
) (*http.Request, []byte, bool, error) {
	// 能力维度 body sanitize：与 Anthropic 平台 passthrough 相同，按 beta
	// header 决定是否保留 body 中的 beta 能力字段，避免客户端"body 带字段但
	// header 忘带 token"的 bug 让第三方上游 400。
	clientBeta := ""
	if c != nil && c.Request != nil {
		clientBeta = getHeaderRaw(c.Request.Header, "anthropic-beta")
	}
	if beta, ok := account.HeaderOverrideValue("anthropic-beta"); ok {
		clientBeta = beta
	}
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, clientBeta); changed {
		body = sanitized
	}

	// Ollama Cloud DeepSeek 出站 max_tokens clamp：判定与 nativeAnthropicTargetURL
	// 的 base 取值同源（GetAnthropicProtocolBaseURL，adaptive 时是 Anthropic 协议
	// 地址而非 CC/Responses 地址），详见 helper 注释。
	body = clampOllamaCloudAnthropicMessagesMaxTokens(account, account.GetAnthropicProtocolBaseURL(), body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, false, err
	}

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if !allowedHeaders[lowerKey] {
				continue
			}
			wireKey := resolveWireCasing(key)
			for _, v := range values {
				addHeaderRaw(req.Header, wireKey, v)
			}
		}
	}

	// 覆盖入站鉴权残留，注入上游认证（默认 x-api-key；可经 extra
	// anthropic_apikey_auth_scheme 切换 Authorization: Bearer；Ollama Cloud
	// 上游按实际 base_url 强制 Bearer，与 nativeAnthropicTargetURL 同源）。
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	req.Header.Del("cookie")
	setAnthropicAPIKeyAuthHeader(req.Header, account, apiKey, account.GetAnthropicProtocolBaseURL())

	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	if getHeaderRaw(req.Header, "anthropic-version") == "" {
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}

	// 账号级请求头覆写（最终生效，覆盖上面所有来源的同名头）
	account.ApplyHeaderOverrides(req.Header)
	payloads := append([][]byte{body}, sessionBodies...)
	applyOpenCodeSessionHeader(c, account, targetURL, req.Header, payloads...)

	// 智谱签名 V4 必须最后应用（design M3；票 22 冻结的调用位置约定）：ts/nonce/sig
	// 与 session 必须同源一致。zhipu 数据面挂点仅此两处，另一处是 sendCCUpstreamRequest。
	//
	// 返回的 signErr 只在 fail 策略 closed（票 24）时非 nil：请求不发出，
	// 交回可 failover 错误给调度器换账号/渠道。
	signed, signErr := s.applyZhipuClientSign(req.Context(), c, account, body, req.Header)
	if signErr != nil {
		return nil, nil, false, signErr
	}

	return req, body, signed, nil
}

// handleNativeAnthropicBufferedResponse 处理非流式原生 Anthropic 响应：
// 校验 JSON、解析 usage、透传响应头后原样回写（仅工具名反向还原）。
func (s *OpenAIGatewayService) handleNativeAnthropicBufferedResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, anthropicTooLargeError)
	if err != nil {
		return nil, err
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveAnthropic(body)

	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, invalidNonStreamingJSONFailoverError(ctx, s.rateLimitService, resp, account, body, err, billingModel)
	}

	usage := parseClaudeUsageFromResponseBody(body)
	if IsForceCacheBilling(ctx) && usage.InputTokens > 0 {
		body, err = classifyAnthropicResponseInputAsCacheRead(body, usage)
		if err != nil {
			return nil, err
		}
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	body = reverseToolNamesIfPresent(c, body)
	c.Data(resp.StatusCode, contentType, body)

	return &OpenAIForwardResult{
		RequestID:        resp.Header.Get("x-request-id"),
		UpstreamHeaders:  resp.Header,
		Usage:            claudeUsageToOpenAIUsage(usage),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: "/v1/messages",
		Stream:           false,
		ReasoningEffort:  reasoningEffort,
		Duration:         time.Since(startTime),
	}, nil
}

// handleNativeAnthropicStreamingResponse 处理流式原生 Anthropic 响应：
// 字节级 SSE 中继（逐行透传、按事件边界 flush），同时解析 usage。
// 骨架与 handleStreamingResponseAnthropicAPIKeyPassthrough 一致。
func (s *OpenAIGatewayService) handleNativeAnthropicStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	if c.Writer.Header().Get("Cache-Control") == "" {
		c.Header("Cache-Control", "no-cache")
	}
	if c.Writer.Header().Get("Connection") == "" {
		c.Header("Connection", "keep-alive")
	}
	c.Header("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	usage := &ClaudeUsage{}
	var firstTokenMs *int
	clientDisconnected := false
	sawTerminalEvent := false

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)

	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}(scanBuf)
	defer close(done)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTimer *time.Timer
	if keepaliveInterval > 0 {
		keepaliveTimer = time.NewTimer(keepaliveInterval)
		defer keepaliveTimer.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTimer != nil {
		keepaliveCh = keepaliveTimer.C
	}
	lastDataAt := time.Now()
	resetKeepaliveTimer := func() {
		if keepaliveTimer == nil {
			return
		}
		if !keepaliveTimer.Stop() {
			select {
			case <-keepaliveTimer.C:
			default:
			}
		}
		keepaliveTimer.Reset(keepaliveInterval)
	}
	inPartialEvent := false

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if !clientDisconnected {
					flusher.Flush()
				}
				if !sawTerminalEvent {
					return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime),
						fmt.Errorf("stream usage incomplete: missing terminal event")
				}
				return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime), nil
			}
			if ev.err != nil {
				if sawTerminalEvent {
					return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime), nil
				}
				if clientDisconnected {
					return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime),
						fmt.Errorf("stream usage incomplete after disconnect: %w", ev.err)
				}
				if errors.Is(ev.err, context.Canceled) || errors.Is(ev.err, context.DeadlineExceeded) {
					return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime),
						fmt.Errorf("stream usage incomplete: %w", ev.err)
				}
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.gateway", "[CN Anthropic 直通] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, ev.err)
					return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime), ev.err
				}
				return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime),
					fmt.Errorf("stream read error: %w", ev.err)
			}

			line := ev.line
			if data, ok := extractAnthropicSSEDataLine(line); ok {
				trimmed := strings.TrimSpace(data)
				observer.ObserveAnthropic([]byte(trimmed))
				if anthropicStreamEventIsTerminal("", trimmed) {
					sawTerminalEvent = true
				}
				if firstTokenMs == nil && trimmed != "" && trimmed != "[DONE]" {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}
				parseSSEUsagePassthrough(data, usage)
			} else {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "event:") && anthropicStreamEventIsTerminal(strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")), "") {
					sawTerminalEvent = true
				}
			}

			if !clientDisconnected {
				restored := string(reverseToolNamesIfPresent(c, []byte(line)))
				if _, err := io.WriteString(w, restored); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.gateway", "[CN Anthropic 直通] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
				} else if _, err := io.WriteString(w, "\n"); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.gateway", "[CN Anthropic 直通] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
				} else if line == "" {
					// 按 SSE 事件边界刷出，减少每行 flush 带来的 syscall 开销。
					flusher.Flush()
					lastDataAt = time.Now()
					resetKeepaliveTimer()
					inPartialEvent = false
				} else {
					inPartialEvent = true
				}
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if clientDisconnected {
				return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime),
					fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.LegacyPrintf("service.gateway", "[CN Anthropic 直通] Stream data interval timeout: account=%d model=%s interval=%s", account.ID, upstreamModel, streamInterval)
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, upstreamModel)
			}
			return s.nativeAnthropicStreamResult(c, resp, usage, firstTokenMs, clientDisconnected, originalModel, billingModel, upstreamModel, reasoningEffort, startTime),
				fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if clientDisconnected {
				continue
			}
			if inPartialEvent {
				resetKeepaliveTimer()
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				resetKeepaliveTimer()
				continue
			}
			if _, err := fmt.Fprint(w, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.gateway", "[CN Anthropic 直通] Client disconnected during keepalive ping, continue draining upstream for usage: account=%d", account.ID)
				continue
			}
			flusher.Flush()
			lastDataAt = time.Now()
			resetKeepaliveTimer()
		}
	}
}

// nativeAnthropicStreamResult 组装流式直通结果；流中断时同样返回已观测到的
// usage 与错误一起带出，避免上游已计量的请求漏记漏计费（对齐 issue #5148 语义）。
func (s *OpenAIGatewayService) nativeAnthropicStreamResult(
	c *gin.Context,
	resp *http.Response,
	usage *ClaudeUsage,
	firstTokenMs *int,
	clientDisconnect bool,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
) *OpenAIForwardResult {
	if usage == nil {
		usage = &ClaudeUsage{}
	}
	return &OpenAIForwardResult{
		RequestID:        resp.Header.Get("x-request-id"),
		UpstreamHeaders:  resp.Header,
		Usage:            claudeUsageToOpenAIUsage(usage),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: "/v1/messages",
		Stream:           true,
		ReasoningEffort:  reasoningEffort,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}
}

// claudeUsageToOpenAIUsage 把 Anthropic 格式 usage 映射到 OpenAI 网关统一的
// 用量结构。Anthropic 的 input_tokens 不含缓存读写，而 OpenAI 网关内部
// 约定 InputTokens 是包含缓存明细的总输入；这里必须先合并，RecordUsage
// 才能准确拆回互斥的计费桶。
func claudeUsageToOpenAIUsage(u *ClaudeUsage) OpenAIUsage {
	if u == nil {
		return OpenAIUsage{}
	}
	return OpenAIUsage{
		InputTokens:              u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
}
