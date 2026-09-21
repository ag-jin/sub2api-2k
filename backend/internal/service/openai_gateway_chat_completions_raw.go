package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// openaiCCRawAllowedHeaders 是 CC 直转路径专用的客户端 header 透传白名单。
//
// **关键**：不能复用 openaiAllowedHeaders——后者含 Codex 客户端专属 header
// （originator / session_id / x-codex-turn-state / x-codex-turn-metadata / conversation_id），
// 这些在 ChatGPT OAuth 上游是必需的，但透传给 DeepSeek/Kimi/GLM 等第三方
// OpenAI 兼容上游会造成：
//   - 完全忽略（多数友好厂商）——隐性污染上游统计
//   - 400 "unknown parameter"（严格上游）——可见错误
//
// 这里仅放行通用 HTTP header；content-type / authorization / accept 由上下文
// 显式设置，不依赖透传。
//
// 参见决策记录：
// pensieve/short-term/maxims/dont-reuse-shared-headers-whitelist-across-different-upstream-trust-domains
var openaiCCRawAllowedHeaders = map[string]bool{
	"accept-language": true,
	"user-agent":      true,
}

// forwardAsRawChatCompletions 直转客户端的 Chat Completions 请求到上游
// `{base_url}/v1/chat/completions`，**不**做 CC↔Responses 协议转换。
//
// 适用场景：account.platform=openai && account.type=apikey && 上游已被探测确认
// 不支持 /v1/responses 端点（如 GLM/Qwen 等第三方 OpenAI 兼容上游）；CN 供应商
// 固定 chat_completions 协议也走此路径。
//
// 与 ForwardAsChatCompletions 的关键差异：
//
//   - 不调用 apicompat.ChatCompletionsToResponses，body 仅做模型 ID 改写
//   - 上游 URL 拼到 /v1/chat/completions 而非 /v1/responses
//   - 流式响应 SSE 直接透传给客户端（上游 chunk 已是 CC 格式）
//   - 非流式响应 JSON 直接透传，仅按需提取 usage
//   - 不应用 codex OAuth transform（APIKey 路径无 OAuth）
//   - 不注入 prompt_cache_key（OAuth 专属机制）
//
// 调用入口：openai_gateway_chat_completions.go::ForwardAsChatCompletions
// 在函数顶部按 openai_compat.ShouldUseResponsesAPI 分流。
func (s *OpenAIGatewayService) forwardAsRawChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse minimal fields needed for routing/billing
	originalModel := gjson.GetBytes(body, "model").String()
	if originalModel == "" {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := gjson.GetBytes(body, "stream").Bool()
	// CodeBuddy 上游仅支持流式（11101 实证）：sendCCUpstreamRequest 一律
	// 强制 stream=true 上传；客户端要非流式响应时由网关聚合（见下方分支）。
	isCodeBuddy := account.IsCodeBuddy()

	// 2. Resolve model mapping (same as ForwardAsChatCompletions)
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	SetOpsUpstreamModel(c, upstreamModel)
	grokCacheIdentity := ""
	if account.Platform == PlatformGrok {
		// Resolve before image bridging or other body rewrites so the fallback is
		// anchored to the client's stable conversation prefix.
		grokCacheIdentity = resolveGrokCacheIdentity(c, body, "", upstreamModel)
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	// 国产模型默认 effort 补充：需要 mappedModel 判定，推迟到 billingModel 算出之后。
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, billingModel)

	// 3. Rewrite model in body (no protocol conversion)
	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}
	if normalizedBody, normalized := NormalizeGLMOpenAIReasoningEffort(upstreamBody, upstreamModel); normalized {
		upstreamBody = normalizedBody
	}

	// 4. Apply OpenAI fast policy on the CC body
	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, upstreamBody)
	if policyErr != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(policyErr, &blocked) {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
			writeChatCompletionsError(c, http.StatusForbidden, "permission_error", blocked.Message)
		}
		return nil, policyErr
	}
	upstreamBody = updatedBody
	// Keep the final outbound tier separate from the observed response tier so
	// usage recording can apply the selected credential's response contract.
	serviceTier := extractOpenAIServiceTierFromBody(upstreamBody)
	if account.Platform == PlatformGrok {
		strippedBody, stripErr := stripRedundantGrokChatViewImageTool(upstreamBody)
		if stripErr != nil {
			return nil, fmt.Errorf("strip redundant Grok Chat view_image tool: %w", stripErr)
		}
		upstreamBody = strippedBody
	}

	// Grok Composer does not accept image_url parts directly, but Grok Build
	// can describe the images first. Bridge only this exact failure mode.
	token, tokenKind, err := s.getRequestCredential(ctx, c, account)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("account %d missing %s credential", account.ID, tokenKind)
	}

	var bridgeUsage OpenAIUsage
	if account.Platform == PlatformGrok {
		bridgedBody, usage, bridged, bridgeErr := s.bridgeGrokComposerImageInputs(ctx, c, account, upstreamBody, token)
		if bridgeErr != nil {
			var failoverErr *UpstreamFailoverError
			if !errors.As(bridgeErr, &failoverErr) && c != nil && c.Writer != nil && !c.Writer.Written() {
				writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", bridgeErr.Error())
			}
			return nil, bridgeErr
		}
		if bridged {
			upstreamBody = bridgedBody
			addOpenAIUsage(&bridgeUsage, usage)
		}
	}

	if clientStream || isCodeBuddy {
		var usageErr error
		if isCodeBuddy {
			// codebuddy：developer→system + 强制流式 + include_usage。
			upstreamBody, usageErr = transformCodeBuddyRequestBody(upstreamBody, account)
		} else {
			upstreamBody, usageErr = ensureOpenAIChatStreamUsage(upstreamBody)
		}
		if usageErr != nil {
			return nil, fmt.Errorf("enable stream usage: %w", usageErr)
		}
	}
	if account.Platform == PlatformGrok {
		upstreamBody, err = stripGrokChatPromptCacheKey(upstreamBody)
		if err != nil {
			return nil, fmt.Errorf("remove Responses-only Grok prompt cache key: %w", err)
		}
		upstreamBody, err = normalizeGrokChatReasoningEffort(upstreamBody, upstreamModel)
		if err != nil {
			return nil, fmt.Errorf("normalize Grok chat reasoning effort: %w", err)
		}
		upstreamBody, err = sanitizeGrokUnsupportedFields(upstreamBody)
		if err != nil {
			return nil, fmt.Errorf("sanitize Grok unsupported fields: %w", err)
		}
	}
	upstreamBody = applyOllamaCloudRawChatCompletionsRequest(account, upstreamBody)
	upstreamBody = clampOllamaCloudUpstreamMaxTokens(account, upstreamBody)

	logger.L().Debug("openai chat_completions raw: forwarding without protocol conversion",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", clientStream),
	)

	// 5. Build and send upstream request via the shared CC pipeline
	targetURL, err := s.rawChatCompletionsURL(account)
	if err != nil {
		return nil, err
	}
	upstreamBody, err = normalizeStrictChatDeveloperRoles(account, targetURL, upstreamBody)
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	SetActualOpenAIUpstreamEndpoint(c, grokChatRawEndpoint)
	customUA := account.GetOpenAIUserAgent()
	if customUA == "" && account.IsGrokOAuth() {
		customUA = defaultGrokUpstreamUserAgent()
	}
	firstTokenTimeout := time.Duration(0)
	if clientStream || isCodeBuddy {
		firstTokenTimeout = s.chatCompletionsFirstTokenTimeout()
	}

	// 流停顿守卫：raw 直转路径除了"首 token 截止"之外，原本**没有任何中流空闲
	// 判定**。上游在首帧之后挂住时（生产实证：buddy 链路出现 63–68 秒后客户端
	// 放弃的 499，以及 WordBuddy 侧 900 秒级静默），网关会一直读下去，既不报错
	// 也不收尾。这里按 stream_data_interval_timeout 监控上游空闲：超时即取消上游
	// 请求，让本请求按可重试错误收尾，而不是把挂起原样暴露给用户。
	//
	// 用独立的子 context（而非直接取消调用方的 ctx）：这样取消来源可归因——
	// 只有 stallGuard.Fired() 时才判定为上游停顿，客户端主动断开不误判。
	stallIdle := s.streamDataIntervalTimeout()
	stallCtx, stallCancel := context.WithCancel(ctx)
	defer stallCancel()
	stallGuard := newUpstreamStallGuard(stallCtx, stallIdle, stallCancel)
	defer stallGuard.stop()

	resp, firstTokenGuard, err := s.sendCCUpstreamRequest(stallCtx, c, account, targetURL, upstreamBody, clientStream || isCodeBuddy, token, customUA, grokCacheIdentity, firstTokenTimeout)
	if err != nil {
		// 首 token 截止前就挂起（连接/响应头阶段）：归类为超时 failover，而非传输
		// 错误。守卫已在 sendCCUpstreamRequest 内释放，这里只按 Fired() 归因。
		//
		// 注意（代码事实，勿想当然）：**stallGuard 并不覆盖响应头等待阶段**。
		// sendCCUpstreamRequestOnce 内部再次调用 detachUpstreamContext
		// （openai_gateway_cc_pipeline.go 的 context.WithoutCancel），stallCtx 的
		// cancel 到不了 transport；且此刻 onStall 钩子尚未注册。
		// 该阶段的保护完全来自 firstTokenGuard（默认 60s，仅流式或 CodeBuddy）。
		// 既有限制：非流式且非 CodeBuddy 的 raw 请求两者皆无（既有缺口，未恶化）。
		if firstTokenGuard != nil && firstTokenGuard.Fired() {
			return nil, s.newOpenAIChatFirstTokenTimeoutError(ctx, c, account, originalModel, "", time.Since(startTime))
		}
		return nil, err
	}
	if firstTokenGuard != nil && firstTokenGuard.Fired() {
		// 响应头在截止之后才到达：同样按超时 failover 处理（与 /v1/responses
		// 路径的 headerGuard 语义一致）。注意不能 stopHeaderWait——守卫还要
		// 继续覆盖流读取阶段，只有首个数据块到达才停表。
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		firstTokenGuard.close()
		return nil, s.newOpenAIChatFirstTokenTimeoutError(ctx, c, account, originalModel, resp.Header.Get("x-request-id"), time.Since(startTime))
	}
	defer func() { _ = resp.Body.Close() }()
	if firstTokenGuard != nil {
		// 守卫的停表/取消随响应体关闭兜底释放；首个数据块到达后由流循环停表。
		resp.Body = &openAIRequestContextReadCloser{ReadCloser: resp.Body, cleanup: firstTokenGuard.close}
	}
	// 响应头到达视为一次真实产出，重置空闲计时后再进入读取阶段。
	resp.Body = stallGuard.wrap(resp.Body)

	// **关键**：本路径的上游请求经 detachUpstreamContext 用 context.WithoutCancel
	// 剥离了取消链（设计意图：客户端断开后仍继续 drain 上游以完成计费），因此
	// stallGuard 的 cancel() **到不了上游请求**，无法解除阻塞中的 Read。
	// 故改为在守卫触发时关闭响应体来解阻塞。
	//
	// 关于依据（避免后人误引）：net/http **没有**文档化"Close 会中断挂起的 Read"
	// 这一保证。实测机制是——响应体外层为 bodyEOFSignal，其 Close 在未见过 EOF 时
	// 走 earlyCloseFn（net/http/transport.go），通知 persistConn.readLoop 由 transport
	// **带外关闭连接**，使阻塞中的 socket 读报错返回（HTTP/2 为 pipe close）。
	// 该行为经本仓实证：HTTP/1.1 与 HTTP/2 下 Close 均毫秒级解除阻塞、Close 返回 nil。
	// 属实现细节，跨 Go 版本理论上可变；若失效，
	// TestUpstreamStallGuard_OnStallHookUnblocksDetachedRead 会失败。
	// 副作用已核：连接被丢弃不复用（非污染）、trackedBody 的 inFlight 正确释放、
	// 与客户端断开的 drain 计费路径不冲突（守卫只在"上游也无产出"时介入）。
	// 仅当守卫判定停顿（Fired）才生效，不影响正常读取。
	if stallGuard != nil {
		body := resp.Body
		stallGuard.onStall(func() { _ = body.Close() })
	}

	// 7. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if account.Platform == PlatformGrok {
			kind := "http_error"
			if s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody) {
				kind = "failover"
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
				Kind:               kind,
				Message:            upstreamMsg,
			})
			s.handleGrokAccountUpstreamError(withGrokTeamRateLimitModel(ctx, upstreamModel), account, resp.StatusCode, resp.Header, respBody)
			if s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody) {
				retryable, retryDelay, retryDeadline, retryMax := grokSameAccountRetryMetadata(account, resp.StatusCode, respBody)
				return nil, &UpstreamFailoverError{
					StatusCode:               resp.StatusCode,
					ResponseBody:             respBody,
					ResponseHeaders:          resp.Header.Clone(),
					RetryableOnSameAccount:   retryable,
					RequestScopedTransient:   retryable && resp.StatusCode == http.StatusTooManyRequests,
					SameAccountRetryDelay:    retryDelay,
					SameAccountRetryDeadline: retryDeadline,
					SameAccountRetryMax:      retryMax,
				}
			}
			return s.handleChatCompletionsErrorResponse(resp, c, account, billingModel)
		}
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account, billingModel)
	}

	if account.Platform == PlatformGrok {
		s.updateGrokUsageFromResponse(withGrokTeamRateLimitModel(ctx, upstreamModel), account, resp.Header, resp.StatusCode)
	}

	// 8. Forward response
	var result *OpenAIForwardResult
	var forwardErr error
	if isCodeBuddy && !clientStream {
		// CodeBuddy 非流式本地聚合：消费上游 SSE，拼接为单个 chat.completion JSON。
		aggBody, aggUsage, upstreamEcho, aggErr := aggregateCodeBuddyCCResponse(resp.Body)
		if aggErr != nil {
			writeChatCompletionsError(c, http.StatusBadGateway, "api_error", "Failed to read upstream response")
			return nil, fmt.Errorf("aggregate codebuddy stream: %w", aggErr)
		}
		if aggUsage.InputTokens == 0 && aggUsage.OutputTokens == 0 {
			// usage 全 0 兜底：不阻塞主链路（上游 usage 偶发缺失，design 已知限制）。
			logger.L().Debug("codebuddy chat_completions aggregate: upstream usage missing",
				zap.Int64("account_id", account.ID),
				zap.String("model", originalModel),
			)
		}
		observer := upstreamResponseModelObserverFromContext(c)
		if observer == nil {
			observer = beginUpstreamResponseModelObservation(c)
		}
		observer.ObserveOpenAI(aggBody, "chat.completion")
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		}
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
			c.Writer.Header().Set("Content-Type", ct)
		} else {
			c.Writer.Header().Set("Content-Type", "application/json")
		}
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write(aggBody)
		return &OpenAIForwardResult{
			RequestID:             resp.Header.Get("x-request-id"),
			Usage:                 aggUsage,
			Model:                 originalModel,
			BillingModel:          billingModel,
			UpstreamModel:         upstreamModel,
			UpstreamResponseModel: upstreamEcho, // 上游回显实值 model（auto → deepseek-v4.1-flash）
			ReasoningEffort:       reasoningEffort,
			ServiceTier:           serviceTier,
			Stream:                false,
			Duration:              time.Since(startTime),
		}, nil
	}
	if clientStream {
		result, forwardErr = s.streamRawChatCompletions(c, resp, account, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime, len(body), firstTokenGuard, stallGuard)
	} else {
		result, forwardErr = s.bufferRawChatCompletions(c, resp, account, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
	if result != nil {
		addOpenAIUsage(&result.Usage, bridgeUsage)
		result.UpstreamEndpoint = grokChatRawEndpoint
	}
	return result, forwardErr
}

func (s *OpenAIGatewayService) rawChatCompletionsURL(account *Account) (string, error) {
	if account.Platform == PlatformGrok {
		targetURL, err := buildGrokChatCompletionsURL(account, s.cfg, s.settingService)
		if err != nil {
			return "", fmt.Errorf("invalid grok base_url: %w", err)
		}
		return targetURL, nil
	}

	return s.openAIChatCompletionsTargetURL(account)
}

// streamRawChatCompletions 透传上游 CC SSE 流到客户端，并提取 usage（包括
// 末尾 [DONE] 之前的 chunk 中的 usage 字段，按 OpenAI CC 协议）。
//
// usage 字段仅在客户端请求 stream_options.include_usage=true 时出现于上游响应中。
// 网关会对上游强制打开 include_usage 以保证计费完整，并原样向下游透传 usage，
// 让级联代理或下游计费系统也能拿到完整用量。
func (s *OpenAIGatewayService) streamRawChatCompletions(
	c *gin.Context,
	resp *http.Response,
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
	requestBodyLen int,
	firstTokenGuard *openAIFirstOutputHeaderGuard,
	stallGuard *upstreamStallGuard,
) (*OpenAIForwardResult, error) {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	requestID := resp.Header.Get("x-request-id")
	writeStreamHeaders := s.newStreamHeaderWriter(c, resp.Header)
	scanner := s.newUpstreamSSEScanner(resp.Body)

	var usage OpenAIUsage
	var firstTokenMs *int
	clientDisconnected := false
	clientOutputStarted := false
	pendingLines := make([]string, 0, 8)
	refusalDetector := newOpenAIChatSilentRefusalDetector(requestBodyLen)
	var terminal openAIRawStreamTerminalState

	// sawTerminal 记录上游是否给出过协议终止信号（[DONE] 或非空 finish_reason）。
	// 旧实现从不校验该信号，导致上游半途断开时客户端收到"半截正文后流自然结束"，
	// 无法区分"回答完整"与"被截断"——这正是下游把断流误当正常完成的原因。
	sawTerminal := false

	// writeMu 串行化所有对 c.Writer 的写入：keepalive goroutine 与读循环并发写，
	// 不加锁会让 SSE 帧交错、破坏客户端解析。
	//
	// 并发状态约定（避免数据竞争）：clientDisconnected / clientOutputStarted /
	// pendingLines 仅由本函数所在 goroutine 读写；keepalive goroutine 只读写下面
	// 的 atomic 变量（outputStarted / keepaliveWriteFailed / lastWriteAt），
	// 不触碰读循环的普通变量。
	var writeMu sync.Mutex
	var lastWriteAt int64
	var outputStarted atomic.Bool
	var keepaliveWriteFailed atomic.Bool
	atomic.StoreInt64(&lastWriteAt, time.Now().UnixNano())

	writeRaw := func(str string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := c.Writer.WriteString(str)
		return err
	}
	flushRaw := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		c.Writer.Flush()
	}

	// 下游空闲保活：raw 直转路径原先完全没有 keepalive，上游在长思考/生成阶段
	// 可以数十秒不发一个字节（实测单流曾出现 21.7s 静默窗口）。中间任何一跳
	// （nginx / 隧道 / 客户端）都可能因零字节静默掐断连接，对用户表现为"无征兆断流"。
	// 这里按 stream_keepalive_interval 补发 SSE 注释帧（eventsource 层直接忽略，
	// 不进入客户端事件流）。
	//
	// **刻意只在正文开始输出之后保活**：正文之前提交响应头会固化 200 状态码，
	// 使上层丧失"换号重试"能力（见 openAIForwardMayFailover——它要求响应未被写出，
	// 或显式标记 SafeToFailoverAfterWrite）。首 token 前的等待因此交给首 token 超时
	// 守卫处理，与另一条 Chat Completions 路径（openai_gateway_chat_completions.go
	// 的 keepalive 分支同样在未开始输出时 continue）保持一致的取舍。
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	keepaliveStop := make(chan struct{})
	keepaliveDone := make(chan struct{})
	if keepaliveInterval > 0 {
		go func() {
			defer close(keepaliveDone)
			ticker := time.NewTicker(keepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-keepaliveStop:
					return
				case <-ticker.C:
				}
				if !outputStarted.Load() || keepaliveWriteFailed.Load() {
					continue
				}
				if time.Since(time.Unix(0, atomic.LoadInt64(&lastWriteAt))) < keepaliveInterval {
					continue
				}
				writeMu.Lock()
				_, err := c.Writer.WriteString(":\n\n")
				if err == nil {
					atomic.StoreInt64(&lastWriteAt, time.Now().UnixNano())
					c.Writer.Flush()
				}
				writeMu.Unlock()
				if err != nil {
					keepaliveWriteFailed.Store(true)
					return
				}
			}
		}()
	} else {
		close(keepaliveDone)
	}
	defer func() {
		close(keepaliveStop)
		<-keepaliveDone
	}()

	writeLine := func(line string) {
		if clientDisconnected || keepaliveWriteFailed.Load() {
			return
		}
		if !clientOutputStarted && !refusalDetector.ShouldReleaseClientOutput() {
			pendingLines = append(pendingLines, line)
			return
		}
		if !clientOutputStarted {
			writeStreamHeaders()
			for _, pending := range pendingLines {
				if werr := writeRaw(pending + "\n"); werr != nil {
					clientDisconnected = true
					logger.L().Debug("openai chat_completions raw: client disconnected, continuing to drain upstream for billing",
						zap.Error(werr),
						zap.String("request_id", requestID),
					)
					return
				}
			}
			pendingLines = pendingLines[:0]
			clientOutputStarted = true
			// 响应头已提交、正文已开始：此后保活是安全的（响应身份已固化）。
			outputStarted.Store(true)
			atomic.StoreInt64(&lastWriteAt, time.Now().UnixNano())
		}
		if werr := writeRaw(line + "\n"); werr != nil {
			clientDisconnected = true
			logger.L().Debug("openai chat_completions raw: client disconnected, continuing to drain upstream for billing",
				zap.Error(werr),
				zap.String("request_id", requestID),
			)
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		refusalDetector.ObserveSSELine(line)
		if payload, ok := extractOpenAISSEDataLine(line); ok {
			trimmedPayload := strings.TrimSpace(payload)
			// 语义进展锚点：解析出真实 data 帧即刷新停顿守卫。
			// 绝不能用"读到任意字节"当锚点——上游会滴流 SSE 注释心跳
			// （生产实测帧 `: heartbeat`），那样守卫会被无限续命，
			// 而客户端在该时段收不到任何语义输出（本次事故形态）。
			stallGuard.touch()
			terminal.ObserveDataLine(trimmedPayload)
			if trimmedPayload != "[DONE]" {
				observer.ObserveOpenAI([]byte(payload), strings.TrimSpace(gjson.Get(payload, "type").String()))
				usageOnlyChunk := isOpenAIChatUsageOnlyStreamChunk(payload)
				if u := extractCCStreamUsage(payload); u != nil {
					usage = *u
				}
				if strings.TrimSpace(gjson.Get(payload, "choices.0.finish_reason").String()) != "" {
					sawTerminal = true
				}
				if firstTokenMs == nil && !usageOnlyChunk {
					elapsed := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &elapsed
					// 首个数据块已到，解除首 token 截止守卫（仅停表，不取消上游）。
					if firstTokenGuard != nil {
						firstTokenGuard.stopHeaderWait()
					}
				}
			}
		}
		line = applyOllamaCloudRawChatCompletionsSSELine(account, line)
		line = stripEmptyChatToolCallIdentityFromSSELine(line)

		// CodeBuddy 上游帧是本家"全量 delta"形状（空串 content/refusal、空数组
		// tool_calls、空壳 function_call/extra_fields、空串 finish_reason），原样
		// 透传会让下游客户端误判帧边界（详见 codebuddy_stream_normalize.go）。
		// 归一化放在 usage/首 token/静默拒绝观测之后，只影响写出给客户端的内容。
		if account.IsCodeBuddy() {
			if normalized, ok := normalizeCodeBuddyChatStreamLine(line); ok {
				line = normalized
			}
		}

		writeLine(line)
		atomic.StoreInt64(&lastWriteAt, time.Now().UnixNano())
		if line == "" {
			if !clientDisconnected && clientOutputStarted {
				flushRaw()
			}
			continue
		}
		if !clientDisconnected && clientOutputStarted {
			flushRaw()
		}
	}

	// 首 token 截止守卫已触发且始终未收到数据块：上游在限时内挂起（排队/静默），
	// 守卫取消上游请求导致的 context.Canceled 属于上游故障而非客户端断开，
	// 判为可切换的 failover 错误。
	if firstTokenGuard != nil && firstTokenGuard.Fired() && firstTokenMs == nil {
		return nil, s.newOpenAIChatFirstTokenTimeoutError(c.Request.Context(), c, account, originalModel, requestID, time.Since(startTime))
	}

	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:                     requestID,
			UpstreamHeaders:               resp.Header,
			Usage:                         usage,
			Model:                         originalModel,
			BillingModel:                  billingModel,
			UpstreamModel:                 upstreamModel,
			UpstreamResponseModel:         observedUpstreamResponseModel(c),
			UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
			UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
			ReasoningEffort:               reasoningEffort,
			ServiceTier:                   resolvedOpenAIUpstreamServiceTier(c, serviceTier),
			Stream:                        true,
			Duration:                      time.Since(startTime),
			FirstTokenMs:                  firstTokenMs,
		}
	}

	scanErr := scanner.Err()
	if scanErr != nil && !errors.Is(scanErr, context.Canceled) && !errors.Is(scanErr, context.DeadlineExceeded) {
		logger.L().Warn("openai chat_completions raw: stream read error",
			zap.Error(scanErr),
			zap.String("request_id", requestID),
		)
	}

	// 客户端取消/断开后上游读失败与上游截断不可区分（取消会连带取消上游请求），
	// 沿用既有语义：按已收到的用量正常收尾计费，不判为上游故障。
	clientAborted := clientDisconnected ||
		errors.Is(scanErr, context.Canceled) ||
		errors.Is(scanErr, context.DeadlineExceeded)

	// **停顿归因必须先于截断判定**：守卫解阻塞的方式是关闭响应体，Close 之后
	// scanner 既可能返回读错误、也可能直接干净 EOF。若只在错误分支归因，
	// clean-EOF 形态会掉进下方"截断"分支被记成 stream_truncated，
	// 错误语义与用户提示都失真。
	// （本仓测试 TestForwardAsRawChatCompletions_MidStreamStallDoesNotHangForever 抓的就是这点。）
	if stallGuard.Fired() {
		logger.L().Warn("openai chat_completions raw: upstream stream stalled",
			zap.String("request_id", requestID),
			zap.Duration("idle", stallGuard.Idle()),
			zap.Int64("account_id", account.ID),
			zap.String("model", originalModel),
		)
		if !clientDisconnected && clientOutputStarted && !sawTerminal {
			writeTerminalError(c, writeRaw, flushRaw, "upstream_stream_stalled",
				fmt.Sprintf("Upstream produced no data for %s", stallGuard.Idle().Round(time.Second)))
		}
		return nil, upstreamStallError("chat.completions", stallGuard.Idle())
	}

	// 上游在任何终止信号之前结束：连接被 reset（scanErr != nil）或干净 EOF。
	// 两者都不能再记成功——此前统一返回 nil error，把上游截断伪装成
	// `HTTP 200 + usage 0/0`，客户端收到半截回答且 Ops 侧完全无感。
	if !clientAborted && terminal.IsTruncated(clientOutputStarted) {
		cause := scanErr
		if cause == nil {
			cause = ErrOpenAIUpstreamStreamTruncated
		}
		logger.L().Warn("openai chat_completions raw: upstream stream truncated before terminal chunk",
			zap.Error(cause),
			zap.String("request_id", requestID),
			zap.Int64("account_id", account.ID),
			zap.String("upstream_model", upstreamModel),
			zap.Bool("saw_sse_data", terminal.sawDataLine),
			zap.Bool("client_output_started", clientOutputStarted),
		)
		if !clientOutputStarted {
			// 响应头尚未提交：可以透明换号重试，客户端不会看到半截流。
			return nil, newOpenAIRawStreamTruncatedFailoverError(c, account, requestID, cause)
		}
		// 已写出语义字节：无法再 failover，改为带类型的上游错误。handler 会据此
		// 补发 SSE error 帧并把本次请求计入 SLA 失败。
		recordOpenAIRawStreamTruncation(c, account, requestID, cause, "http_error")
		return resultWithUsage(), newOpenAIUpstreamStreamReadError(cause)
	}

	if scanErr == nil && !clientDisconnected && !clientOutputStarted {
		if refusalDetector.IsSilentRefusal() {
			return nil, newOpenAISilentRefusalFailoverError(c, account, requestID)
		}
		if len(pendingLines) > 0 {
			writeStreamHeaders()
			for _, pending := range pendingLines {
				if werr := writeRaw(pending + "\n"); werr != nil {
					clientDisconnected = true
					logger.L().Debug("openai chat_completions raw: client disconnected during final flush",
						zap.Error(werr),
						zap.String("request_id", requestID),
					)
					break
				}
			}
			if !clientDisconnected {
				flushRaw()
				clientOutputStarted = true
			}
		}
	}

	return resultWithUsage(), nil
}

// writeTerminalError 在响应头已提交、正文已开始输出之后，向下游补发一个可见的
// 终止事件（标准 error 帧 + [DONE]），使客户端能够区分"回答完整"与"上游截断"。
//
// 为什么需要它：raw 直转路径此前在上游异常中断时既不写 error 帧也不写 [DONE]，
// 只是让响应体自然结束。按 SSE 语义，客户端会把这种结束当作正常完成，于是
// "断流"被静默吞掉（用户看到的是回答突然停住但界面显示成功）。
//
// [DONE] 必须跟在 error 帧之后：多数 OpenAI 兼容客户端只在收到 [DONE] 或
// finish_reason 才结束读取循环，缺少它会导致客户端挂到自身读超时。
func writeTerminalError(
	c *gin.Context,
	writeRaw func(string) error,
	flushRaw func(),
	code string,
	message string,
) {
	if c == nil || c.Writer == nil {
		return
	}
	payload, err := json.Marshal(gin.H{"error": gin.H{
		"type":    "upstream_error",
		"code":    code,
		"message": message,
	}})
	if err != nil {
		payload = []byte(`{"error":{"type":"upstream_error","message":"Upstream stream terminated unexpectedly"}}`)
	}
	if werr := writeRaw("data: " + string(payload) + "\n\n"); werr != nil {
		return
	}
	if werr := writeRaw("data: [DONE]\n\n"); werr != nil {
		return
	}
	flushRaw()
}

// ensureOpenAIChatStreamUsage 确保 raw Chat Completions 流式请求会让上游返回 usage。
// usage 也会继续向下游透传，支持级联代理和下游计费系统。
func ensureOpenAIChatStreamUsage(body []byte) ([]byte, error) {
	updated, err := sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return body, err
	}
	return updated, nil
}

func isOpenAIChatUsageOnlyStreamChunk(payload string) bool {
	if strings.TrimSpace(payload) == "" {
		return false
	}
	if !gjson.Get(payload, "usage").Exists() {
		return false
	}
	choices := gjson.Get(payload, "choices")
	return choices.Exists() && choices.IsArray() && len(choices.Array()) == 0
}

// extractCCStreamUsage 从单个 CC 流式 chunk 的 payload 中提取 usage 字段。
// CC 协议中 usage 仅出现在末尾 chunk（且仅当 include_usage 生效时），
// 但上游可能在多个 chunk 中重复——总是用最新值。
func extractCCStreamUsage(payload string) *OpenAIUsage {
	usageResult := gjson.Get(payload, "usage")
	if !usageResult.Exists() || !usageResult.IsObject() {
		return nil
	}
	u, ok := openAIUsageFromGJSON(usageResult)
	if !ok {
		return nil
	}
	return &u
}

// bufferRawChatCompletions 透传上游 CC 非流式 JSON 响应。
func (s *OpenAIGatewayService) bufferRawChatCompletions(
	c *gin.Context,
	resp *http.Response,
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if !errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			writeChatCompletionsError(c, http.StatusBadGateway, "api_error", "Failed to read upstream response")
		}
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveOpenAI(respBody, strings.TrimSpace(gjson.GetBytes(respBody, "type").String()))

	var usage OpenAIUsage
	if parsedUsage, ok := extractOpenAIUsageFromJSONBytes(respBody); ok {
		usage = parsedUsage
	}
	responseModel := gjson.GetBytes(respBody, "model").String()
	if requiresBillableGrokChatUsage(account, billingModel, upstreamModel, responseModel) && !hasBillableGrokChatUsage(usage) {
		upstreamRequestID := firstNonEmpty(requestID, resp.Header.Get("xai-request-id"))
		return nil, newGrokMissingUsageFailoverError(c, account, upstreamRequestID)
	}
	respBody = applyOllamaCloudRawChatCompletionsResponse(account, respBody)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Writer.Header().Set("Content-Type", ct)
	} else {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(respBody)

	return &OpenAIForwardResult{
		RequestID:                     requestID,
		UpstreamHeaders:               resp.Header,
		Usage:                         usage,
		Model:                         originalModel,
		BillingModel:                  billingModel,
		UpstreamModel:                 upstreamModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
		ReasoningEffort:               reasoningEffort,
		ServiceTier:                   resolvedOpenAIUpstreamServiceTier(c, serviceTier),
		Stream:                        false,
		Duration:                      time.Since(startTime),
	}, nil
}

// buildOpenAIChatCompletionsURL 拼接上游 Chat Completions 端点 URL。
//
//   - base 已是 /chat/completions：原样返回
//   - base 以 /v1 结尾：追加 /chat/completions
//   - base 以其他版本段结尾（如 /v4）：追加 /chat/completions
//   - 其他情况：追加 /v1/chat/completions
//
// 与 buildOpenAIResponsesURL 是姐妹函数。
func buildOpenAIChatCompletionsURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/chat/completions")
}
