package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Forward forwards request to OpenAI API
func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	beginUpstreamResponseModelObservation(c)
	ClearActualOpenAIUpstreamEndpoint(c)
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		SetActualOpenAIUpstreamEndpoint(c, "/v1/chat/completions")
	}
	filteredBody, filterErr := filterOpenAIResponsesNoneReasoningEffortForAccount(account, body)
	if filterErr != nil {
		return nil, filterErr
	}
	body = filteredBody
	clearGrokResponsesClientToolMapping(c)
	clearOpenAIResponsesClientToolMapping(c)
	clearOpenAIResponsesNamespaceNames(c)
	setCodexToolNameReverse(c, nil)
	if _, err := s.prepareCodexAccountIdentitySource(ctx, c, account); err != nil {
		return nil, err
	}
	startTime := time.Now()
	// 固定渠道映射后的请求级 canonical body；账号 normalize/strip 不得改写跨 failover hint。
	canonicalImageIntentBody := body

	restrictionResult := s.detectCodexClientRestriction(c, account, body)
	apiKeyID := getAPIKeyIDFromContext(c)
	// 执行作用域必须取自客户端原始身份：后面的账号 namespace 改写与指纹收敛会改掉
	// 请求体里的 client_metadata / prompt_cache_key，用改写后的值取键会让不同会话
	// 落到同一个键，也会与 WS 接入路径按原始报文算出的键对不上。
	wsExecutionScope, _ := resolveOpenAIWSExecutionScope(c, body, apiKeyID)
	logCodexCLIOnlyDetection(ctx, c, account, apiKeyID, restrictionResult, body)
	if restrictionResult.Enabled && !restrictionResult.Matched {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "forbidden_error",
				"message": CodexClientRestrictionMessage(restrictionResult),
			},
		})
		return nil, errors.New("codex_cli_only restriction: only codex official clients are allowed")
	}

	normalizedBody, normalized, err := normalizeOpenAICodexCompactReasoningEffortForAccount(c, account, body)
	if err != nil {
		return nil, err
	}
	if normalized {
		body = normalizedBody
	}
	legacyIngressBody, legacyIngressChanged, legacyIngressErr := normalizeOpenAIResponsesLegacyIngressWithConfig(s.cfg, body)
	if legacyIngressErr != nil {
		return nil, legacyIngressErr
	}
	if legacyIngressChanged {
		body = legacyIngressBody
	}
	// 在分流到 passthrough / Codex transform / 原生 ChatCompletions 之前统一修正
	// 显式为 null 的工具 Schema type，否则 upstream 的 400 会被归一成可重试的 502，
	// 同一份坏定义在账号池里反复重放。
	if sanitizedToolBody, toolSchemaSanitized, toolSchemaErr := sanitizeOpenAIResponsesToolSchemasForPlatform(body, account.Platform); toolSchemaErr != nil {
		return nil, toolSchemaErr
	} else if toolSchemaSanitized {
		body = sanitizedToolBody
	}
	if account.IsOpenAIOAuthLike() {
		reasoningBody, reasoningChanged, reasoningErr := normalizeOpenAIResponsesReasoningMode(body, account.GetMappedModel(gjson.GetBytes(body, "model").String()))
		if reasoningErr != nil {
			return nil, fmt.Errorf("normalize OpenAI Responses reasoning.mode: %w", reasoningErr)
		}
		if reasoningChanged {
			body = reasoningBody
		}
	}
	responsesLite := account.IsOpenAI() && isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader))
	if responsesLite {
		liteBody, changed, liteErr := normalizeOpenAIResponsesLitePayloadForAccount(body, account)
		if liteErr != nil {
			param := "tools"
			var validationErr *openAIResponsesLiteValidationError
			if errors.As(liteErr, &validationErr) {
				param = validationErr.param
			}
			setOpsUpstreamError(c, http.StatusBadRequest, liteErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"type": "invalid_request_error", "message": liteErr.Error(), "param": param,
			}})
			return nil, liteErr
		}
		if changed {
			body = liteBody
		}
	}
	wsDecision := s.getOpenAIWSProtocolResolver().Resolve(account)
	// 仅允许 WS 入站请求走 WS 上游，避免出现 HTTP -> WS 协议混用。
	wsDecision = resolveOpenAIWSDecisionByClientTransport(wsDecision, GetOpenAIClientTransport(c))
	passthroughEnabled := account.IsOpenAIPassthroughEnabled()
	compactPath := isOpenAIResponsesCompactPath(c)
	if shouldFlattenOpenAIResponsesNamespaces(account, wsDecision.Transport, passthroughEnabled, compactPath) {
		body, err = flattenOpenAIResponsesNamespaces(c, body)
		if err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"type": "invalid_request_error", "message": err.Error(), "param": "tools",
			}})
			return nil, err
		}
	}
	if shouldStripOpenAIResponsesInputNamespaces(account, wsDecision.Transport, passthroughEnabled) {
		keepToolCallNamespaces := shouldKeepOpenAIResponsesToolCallNamespaces(
			account, wsDecision.Transport, passthroughEnabled, compactPath, body,
		)
		body, err = stripOpenAIResponsesInputNamespaces(body, keepToolCallNamespaces)
		if err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"type": "invalid_request_error", "message": err.Error(), "param": "input",
			}})
			return nil, err
		}
	}

	nativeCNResponses := account.UsesNativeCNResponses()
	nativeDeepSeekResponses := account.Platform == PlatformDeepseek && nativeCNResponses
	if nativeDeepSeekResponses && account.Type == AccountTypeAPIKey && !compactPath &&
		needsOpenAIResponsesClientToolAdaptation(body) {
		adaptedBody, mapping, adaptErr := adaptOpenAIResponsesClientTools(body)
		if adaptErr != nil {
			return nil, fmt.Errorf("adapt DeepSeek Responses client tools: %w", adaptErr)
		}
		body = adaptedBody
		setOpenAIResponsesClientToolMapping(c, mapping)
	}

	originalBody := body
	rememberOpenCodeInboundBody(c, originalBody)
	requestView := newOpenAIRequestView(body)
	reqModel, reqStream, promptCacheKey := requestView.Model, requestView.Stream, requestView.PromptCacheKey
	originalModel := reqModel

	if account.Platform == PlatformGrok {
		return s.forwardGrokResponses(ctx, c, account, body, originalModel, reqStream, startTime)
	}

	if account.IsOpenAIApiKey() {
		if normalized, changed, normalizeErr := normalizeOpenAIParallelToolCallsWithoutTools(body, responsesLite); normalizeErr != nil {
			return nil, normalizeErr
		} else if changed {
			body = normalized
			originalBody = normalized
		}
		if normalized, changed, normalizeErr := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, isOpenAIResponsesCompactPath(c)); normalizeErr != nil {
			return nil, normalizeErr
		} else if changed {
			body = normalized
			originalBody = normalized
		}
		requestView = newOpenAIRequestView(body)
		reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
		originalModel = reqModel
	}

	// 上游协议统一由 resolveUpstreamProtocol 判定（按模型分流时带上游模型目录）。OpenAI API Key 账号只会落到
	// Responses / Chat Completions，上面的归一化对两条路径都生效。
	routingModel := upstreamRoutingModel(account, body, "")
	if account.IsOpenCodeGo() && IsOpenCodeUnsupportedModel(routingModel) {
		return nil, writeOpenCodeUnsupportedModelError(c, false, routingModel)
	}
	switch s.resolveUpstreamProtocolFor(ctx, account, APIProtocolResponses, routingModel) {
	case APIProtocolAnthropic:
		// Responses 客户端 × Anthropic 上游：转成 Anthropic 请求走原生端点。不能落到
		// raw-CC 分支——其 URL 构造会把 anthropic base 当 CC base 用。
		// 账号映射未命中时以去除首尾空白的请求模型兜底，计费名与上游模型名一致。
		return s.forwardResponsesViaNativeAnthropic(ctx, c, account, body, reqModel)
	case APIProtocolChatCompletions:
		return s.forwardResponsesViaRawChatCompletions(ctx, c, account, body, startTime)
	}
	SetActualOpenAIUpstreamEndpoint(c, openAIResponsesUpstreamEndpoint)
	if account.IsOpenAI() && (account.IsOpenAIApiKey() || account.IsOpenAIOAuthLike()) {
		normalizedReasoningBody, reasoningChanged, reasoningErr := normalizeOpenAIResponsesReasoningContentReplay(body)
		if reasoningErr != nil {
			return nil, fmt.Errorf("normalize OpenAI Responses reasoning content replay: %w", reasoningErr)
		}
		if reasoningChanged {
			body = normalizedReasoningBody
			originalBody = normalizedReasoningBody
			requestView = newOpenAIRequestView(normalizedReasoningBody)
			reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
			originalModel = reqModel
		}
		sanitizedBody, changed, sanitizeErr := sanitizeOpenAIResponsesInputItemIDs(body)
		if sanitizeErr != nil {
			return nil, fmt.Errorf("sanitize OpenAI Responses input item IDs: %w", sanitizeErr)
		}
		if changed {
			body = sanitizedBody
			originalBody = sanitizedBody
			requestView = newOpenAIRequestView(sanitizedBody)
			reqModel, reqStream, promptCacheKey = requestView.Model, requestView.Stream, requestView.PromptCacheKey
			originalModel = reqModel
		}
	}

	compatMessagesBridge := isOpenAICompatMessagesBridgeBody(body)
	setOpenAICompatMessagesBridgeContext(c, compatMessagesBridge)

	isCodexCLI := openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) || (s.cfg != nil && s.cfg.Gateway.ForceCodexCLI)
	codexImageGenerationExplicitToolPolicy := codexImageGenerationExplicitToolPolicyAllow
	if isCodexCLI {
		codexImageGenerationExplicitToolPolicy = account.CodexImageGenerationExplicitToolPolicy()
	}
	if c != nil {
		c.Set("openai_ws_transport_decision", string(wsDecision.Transport))
		c.Set("openai_ws_transport_reason", wsDecision.Reason)
	}
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		logOpenAIWSModeDebug(
			"selected account_id=%d account_type=%s transport=%s reason=%s model=%s stream=%v",
			account.ID,
			account.Type,
			normalizeOpenAIWSLogValue(string(wsDecision.Transport)),
			normalizeOpenAIWSLogValue(wsDecision.Reason),
			reqModel,
			reqStream,
		)
	}
	// 当前仅支持 WSv2；WSv1 命中时直接返回错误，避免出现“配置可开但行为不确定”。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocket {
		if c != nil {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": "OpenAI WSv1 is temporarily unsupported. Please enable responses_websockets_v2.",
				},
			})
		}
		return nil, errors.New("openai ws v1 is temporarily unsupported; use ws v2")
	}
	if passthroughEnabled {
		attemptImageIntentInvalidated := false
		if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
			strippedBody, changed, stripErr := stripOpenAIImageGenerationToolsFromRawPayload(body)
			if stripErr != nil {
				return nil, stripErr
			}
			if changed {
				body = strippedBody
				originalBody = strippedBody
				attemptImageIntentInvalidated = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
			}
		}
		// 透传分支只需要轻量提取字段，避免热路径全量 Unmarshal。
		mappedModel := account.GetMappedModel(reqModel)
		reasoningEffort := extractOpenAIReasoningEffortFromBody(body, mappedModel)
		// 国产模型默认 effort 补充：也要用 mappedModel 判定是否是 passback-required 上游。
		reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, mappedModel)
		return s.forwardOpenAIPassthrough(
			ctx,
			c,
			account,
			originalBody,
			canonicalImageIntentBody,
			reqModel,
			attemptImageIntentInvalidated,
			reasoningEffort,
			reqStream,
			startTime,
		)
	}

	bodyModified := false
	clientPromptCacheKey := promptCacheKey
	var reqBody map[string]any
	ensureReqBody := func() (map[string]any, error) {
		if requestView.HasPatches() {
			patchedBody, patchErr := requestView.ApplyPatches()
			if patchErr != nil {
				return nil, patchErr
			}
			body = patchedBody
			requestView = newOpenAIRequestView(body)
			reqBody = nil
			bodyModified = false
		}
		if reqBody != nil {
			return reqBody, nil
		}
		decoded, decodeErr := requestView.Decode(c)
		if decodeErr != nil {
			return nil, decodeErr
		}
		reqBody = decoded
		return reqBody, nil
	}
	markPatchSet := func(path string, value any) {
		bodyModified = true
		if requestView.patchesDisabled {
			if reqBody != nil {
				setOpenAIRequestMapPath(reqBody, path, value)
			}
			return
		}
		requestView.MarkPatchSet(path, value)
	}
	markPatchDelete := func(path string) {
		bodyModified = true
		if requestView.patchesDisabled {
			if reqBody != nil {
				deleteOpenAIRequestMapPath(reqBody, path)
			}
			return
		}
		requestView.MarkPatchDelete(path)
	}
	disablePatch := func() {
		requestView.DisablePatches()
	}
	markDecodedModified := func() {
		bodyModified = true
		disablePatch()
	}

	apiKey := getAPIKeyFromContext(c)
	imageGenerationAllowed := GroupAllowsImageGeneration(nil)
	if apiKey != nil {
		imageGenerationAllowed = GroupAllowsImageGeneration(apiKey.Group)
	}
	codexImageGenerationBridgeEnabled := isCodexCLI &&
		!isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader)) &&
		imageGenerationAllowed &&
		codexImageGenerationExplicitToolPolicy != codexImageGenerationExplicitToolPolicyStrip &&
		s.isCodexImageGenerationBridgeEnabled(ctx, account, apiKey)
	var imageIntent bool
	canonicalImageIntent := resolveOpenAIImageIntentHint(c, reqModel, canonicalImageIntentBody, IsImageGenerationIntent)
	if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stripOpenAIImageGenerationTools(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
		}
		imageIntent = IsImageGenerationIntentMap(openAIResponsesEndpoint, reqModel, decoded)
	} else {
		imageIntent = canonicalImageIntent
	}
	if imageIntent && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": ImageGenerationPermissionMessage()}})
		return nil, errors.New("image generation disabled for group")
	}

	isCompactRequest := compactPath
	requestedModel := reqModel
	billingModel, upstreamModel := resolveOpenAIForwardMappedModels(account, requestedModel, isCompactRequest)
	if isCompactRequest {
		if compactModel := s.resolveOpenAICompactFallbackModel(account, requestedModel); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	instructions := gjson.GetBytes(body, "instructions")
	instructionsEmpty := !instructions.Exists() || instructions.Type != gjson.String || strings.TrimSpace(instructions.String()) == ""
	if instructionsEmpty && account.UsesOpenAICodexProtocol() && !compatMessagesBridge && !nativeCNResponses {
		markPatchSet("instructions", defaultCodexSynthInstructions(upstreamModel))
	}
	if billingModel != requestedModel {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", requestedModel, billingModel, account.Name, isCodexCLI)
	}
	reqModel = billingModel
	if upstreamModel != requestedModel {
		markPatchSet("model", upstreamModel)
	}
	if upstreamModel != billingModel {
		if isCompactRequest {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Compact model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", requestedModel, upstreamModel, account.Name, isCodexCLI)
		} else {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Upstream model resolved: %s -> %s (account: %s, type: %s, isCodexCLI: %v)", billingModel, upstreamModel, account.Name, account.Type, isCodexCLI)
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()) == "minimal" {
		markPatchSet("reasoning.effort", "none")
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized reasoning.effort: minimal -> none (account: %s)", account.Name)
	}
	if strings.TrimSpace(gjson.GetBytes(body, "text.format.type").String()) == "json_schema" ||
		strings.TrimSpace(gjson.GetBytes(body, "response_format.type").String()) == "json_schema" {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if normalizeOpenAIResponseFormatSchemas(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized Responses JSON schema compatibility")
		}
	}

	imageIntent = imageIntent || IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, nil) || isOpenAIImageGenerationModel(upstreamModel)
	if imageIntent && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": ImageGenerationPermissionMessage()}})
		return nil, errors.New("image generation disabled for group")
	}

	// /responses/compact 是会话压缩请求：上游不接受 tool_choice（400 unknown_parameter），
	// 注入 image_generation 工具也没有意义，整块豁免。
	if imageGenerationAllowed && !isCompactRequest && (codexImageGenerationBridgeEnabled || isOpenAIImageGenerationModel(requestView.Model) || openAIRequestBodyImageGenerationToolNeedsNormalization(body) || isOpenAIImageGenerationModel(upstreamModel)) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if codexImageGenerationBridgeEnabled && ensureOpenAIResponsesImageGenerationTool(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Injected /responses image_generation tool for Codex client")
		}
		if codexImageGenerationBridgeEnabled && ensureOpenAIResponsesImageGenerationToolChoiceAuto(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Set /responses image_generation tool_choice=auto for Codex client")
		}
		if normalizeOpenAIResponsesImageGenerationTools(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image_generation tool payload")
		}
		if normalizeOpenAIResponsesImageOnlyModel(decoded) {
			markDecodedModified()
			if model, ok := decoded["model"].(string); ok {
				upstreamModel = strings.TrimSpace(model)
			}
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image-only model request inbound_model=%s image_model=%s upstream_model=%s", requestView.Model, billingModel, upstreamModel)
		}
		if err := validateOpenAIResponsesImageModel(decoded, upstreamModel); err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": "model"}})
			return nil, err
		}
		if hasOpenAIImageGenerationTool(decoded) {
			imageIntent = true
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s", requestView.Model, upstreamModel, account.Type)
		}
		if codexImageGenerationBridgeEnabled && applyCodexImageGenerationBridgeInstructions(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Added Codex image_generation bridge instructions")
		}
	} else if imageGenerationAllowed && imageIntent && openAIRequestBodyHasImageGenerationDeclaration(body) {
		// 完整 image_generation tool 只做 raw 计费读取，校验/桥接/旧字段迁移命中时才展开大 input map。
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s", requestView.Model, upstreamModel, account.Type)
	}

	if isCodexSparkModel(upstreamModel) && openAIRequestBodyMayContainImageInput(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if err := validateCodexSparkInput(decoded, upstreamModel); err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": "input"}})
			return nil, err
		}
	}

	// gpt-5.3-codex-spark also rejects the image_generation tool (HTTP 400,
	// param=tools). Strip it here so both APIKey and OAuth /responses paths are
	// covered regardless of the image-generation feature gate.
	if isCodexSparkModel(upstreamModel) && openAIRequestBodyHasImageGenerationDeclaration(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stripCodexSparkImageGenerationTools(decoded) {
			markDecodedModified()
		}
	}

	if account.UsesOpenAICodexProtocol() {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		// Responses OAuth 与 Chat 兼容入口保持一致：纯文本 system 可以无损提升后删除，
		// JSON object 模式仍需在 input 中保留 JSON 指令供上游兼容校验。
		omitPromotedSystemMessages := !strings.EqualFold(
			strings.TrimSpace(gjson.GetBytes(body, "text.format.type").String()),
			"json_object",
		)
		codexResult := codexTransformResult{}
		if compatMessagesBridge {
			codexResult = applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{
				IsCodexCLI:                          isCodexCLI,
				IsCompact:                           isCompactRequest,
				SkipDefaultInstructions:             true,
				PreserveToolCallIDs:                 true,
				OmitPromotedSystemMessagesFromInput: omitPromotedSystemMessages,
				ResponsesLite:                       responsesLite,
			})
			ensureCodexOAuthInstructionsField(decoded)
			markDecodedModified()
		} else {
			codexResult = applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{
				IsCodexCLI:                          isCodexCLI,
				IsCompact:                           isCompactRequest,
				OmitPromotedSystemMessagesFromInput: omitPromotedSystemMessages,
				ResponsesLite:                       responsesLite,
			})
		}
		if codexResult.Error != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": codexResult.Error.Error()}})
			return nil, codexResult.Error
		}
		setCodexToolNameReverse(c, codexResult.ToolNameReverse)
		if codexResult.Modified {
			markDecodedModified()
		}
		// 带真实 device_id 时补齐 client_metadata 安装标识，与真实 Codex 对齐（compact 形态不同，跳过）。
		if !isCompactRequest && applyCodexClientMetadata(decoded, account) {
			markDecodedModified()
		}
		if currentClientPromptCacheKey, ok := decoded["prompt_cache_key"].(string); ok {
			clientPromptCacheKey = currentClientPromptCacheKey
		}
		// 在账号级 scoping 之前捕获 body 的原始会话标识：它是 session-id 头
		// 缺失时的 fallback，必须与 body / prompt_cache_key 收敛到同一串。
		clientBodySessionID := ""
		if clientMetadata, ok := decoded["client_metadata"].(map[string]any); ok {
			if value, ok := clientMetadata["session_id"].(string); ok {
				clientBodySessionID = strings.TrimSpace(value)
			}
		}
		// Account namespace is orthogonal to fingerprint convergence: preserve
		// each client's identity cardinality, but never reuse it across OAuth
		// credentials after scheduler failover.
		if !isCompactRequest && applyCodexAccountIdentityClientMetadataMap(decoded, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c)) {
			markDecodedModified()
		}
		stageCodexFingerprintIDs(c, nil)
		// 指纹收敛：一次性解析收敛 ID，请求体和出站头共享同一份 IDs（保证 turn_id 等随机字段一致）。
		// fingerprintIDs 在此处解析，后续 buildUpstreamRequest 中使用同一份。
		// compact 路径同样进入头合成（与探测同构），但 body 形态不同、不写
		// client_metadata。
		{
			var clientHeaders http.Header
			if c != nil && c.Request != nil {
				clientHeaders = c.Request.Header
			}
			fpIDs := resolveCodexFingerprintIDsFromRequestWithFallback(
				account, clientHeaders, firstNonEmpty(clientBodySessionID, clientPromptCacheKey),
			)
			if fpIDs != nil && !isCompactRequest {
				if applyCodexFingerprintClientMetadata(decoded, fpIDs) {
					markDecodedModified()
				}
			}
			// 将 fpIDs 存入 gin context，供 buildUpstreamRequest 中头改写使用。
			// 无条件覆写（含 nil）：failover 从收敛账号切到 off 账号时，上一
			// 账号的 IDs 不得残留（stageCodexFingerprintIDs 注释）。
			stageCodexFingerprintIDs(c, fpIDs)
		}
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if strings.TrimSpace(clientPromptCacheKey) != "" {
			// The body now carries an account-scoped value. Keep the original here
			// so the header builder derives the same namespace exactly once.
			promptCacheKey = clientPromptCacheKey
		} else if currentPromptCacheKey, ok := decoded["prompt_cache_key"].(string); ok && currentPromptCacheKey != "" {
			// Fingerprint convergence may inject a default key when the client did
			// not provide one; preserve that existing fallback.
			promptCacheKey = currentPromptCacheKey
		} else if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		}
	}

	if !SupportsVerbosity(upstreamModel) && gjson.GetBytes(body, "text.verbosity").Exists() {
		markPatchDelete("text.verbosity")
	}

	if !isCodexCLI {
		maxOutputTokens := gjson.GetBytes(body, "max_output_tokens")
		if maxOutputTokens.Exists() {
			switch account.Platform {
			case PlatformOpenAI, PlatformDeepseek:
				// Preserve Responses-native output limits unless the selected upstream
				// explicitly rejects the field in the bounded HTTP retry loop below.
			case PlatformAnthropic:
				decoded, decodeErr := ensureReqBody()
				if decodeErr != nil {
					return nil, decodeErr
				}
				delete(decoded, "max_output_tokens")
				if _, hasMaxTokens := decoded["max_tokens"]; !hasMaxTokens {
					decoded["max_tokens"] = maxOutputTokens.Value()
				}
				markDecodedModified()
			case PlatformGemini:
				markPatchDelete("max_output_tokens")
			default:
				markPatchDelete("max_output_tokens")
			}
		}
		// /v1/responses 的规范输出上限字段是 max_output_tokens；部分客户端仍按
		// Chat Completions 习惯发送 max_tokens，兼容 Responses 上游会拒绝该字段（#4417）。
		// 仅对 OpenAI 平台归一化：Anthropic 合法使用 max_tokens，其 max_output_tokens
		// 反向转换已在上方 switch 中处理。
		if account.Platform == PlatformOpenAI {
			if maxTokens := gjson.GetBytes(body, "max_tokens"); maxTokens.Exists() {
				if !gjson.GetBytes(body, "max_output_tokens").Exists() {
					markPatchSet("max_output_tokens", maxTokens.Value())
				}
				markPatchDelete("max_tokens")
			}
		}
		if gjson.GetBytes(body, "max_completion_tokens").Exists() && (account.Type == AccountTypeAPIKey || account.Platform != PlatformOpenAI) {
			markPatchDelete("max_completion_tokens")
		}
		for _, unsupportedField := range []string{"prompt_cache_retention", "safety_identifier", "prompt_cache_options"} {
			if gjson.GetBytes(body, unsupportedField).Exists() {
				markPatchDelete(unsupportedField)
			}
		}
	}
	// Ollama Cloud（实际 Responses 上游为 ollama.com）输出上限 clamp：对 Codex 与
	// 非 Codex 客户端一律执行（真实 Codex 客户端同样会带超限 max_output_tokens 被
	// ollama.com 以 400 拒绝）。在 `!isCodexCLI` 归一化块之后独立调用：非 Codex 时
	// 位于平台字段归一化之后，不跳过原有平台 switch（patch 按追加顺序应用，set 在
	// 先前的 delete/set 之后生效）；Codex 请求不做归一化，直接按 body 现值判定。
	if clampedCap, ok := ollamaCloudResponsesMaxOutputTokensClamp(account, upstreamModel, body); ok {
		markPatchSet("max_output_tokens", clampedCap)
	}
	if wsDecision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 &&
		!account.IsOpenAIApiKey() && gjson.GetBytes(body, "previous_response_id").Exists() {
		markPatchDelete("previous_response_id")
	}
	if openAIRequestBodyMayContainEmptyBase64InputImage(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(decoded) {
			markDecodedModified()
		}
	}

	rawTier := requestView.ServiceTier
	if openAIGroupForcesFast(ctx, account) {
		rawTier = OpenAIFastTierPriority
		if requestView.ServiceTier != OpenAIFastTierPriority {
			markPatchSet("service_tier", OpenAIFastTierPriority)
		}
	}
	if rawTier != "" {
		if normTier := normalizedOpenAIServiceTierValue(rawTier); normTier != "" {
			action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, upstreamModel, normTier)
			switch action {
			case BetaPolicyActionBlock:
				msg := errMsg
				if msg == "" {
					msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, upstreamModel)
				}
				blocked := &OpenAIFastBlockedError{Message: msg}
				writeOpenAIFastPolicyBlockedResponse(c, blocked)
				return nil, blocked
			case BetaPolicyActionFilter:
				markPatchDelete("service_tier")
			case OpenAIFastPolicyActionForcePriority:
				if rawTier != OpenAIFastTierPriority {
					markPatchSet("service_tier", OpenAIFastTierPriority)
				}
			default:
				if normTier != rawTier {
					markPatchSet("service_tier", normTier)
				}
			}
		}
	} else if s.shouldForceOpenAIFastPriorityForMissingTier(ctx, account, upstreamModel) {
		markPatchSet("service_tier", OpenAIFastTierPriority)
	}

	if account.UsesOpenAICodexProtocol() {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if input, ok := decoded["input"].([]any); ok && sanitizeOpenAIResponsesOrphanToolOutputs(
			decoded,
			input,
			strings.TrimSpace(firstNonEmptyString(decoded["previous_response_id"])) != "",
		) {
			markDecodedModified()
		}
	}
	if reqBody != nil || openAIResponsesInputMayNeedTruncation(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if truncateOpenAIResponsesInputText(decoded) {
			markDecodedModified()
		}
	}

	if bodyModified {
		if requestView.HasPatches() {
			if patchedBody, patchErr := requestView.ApplyPatches(); patchErr == nil {
				body = patchedBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				bodyModified = false
			}
		}
		if bodyModified {
			decoded, decodeErr := ensureReqBody()
			if decodeErr != nil {
				return nil, decodeErr
			}
			var marshalErr error
			body, marshalErr = marshalOpenAIUpstreamJSON(decoded)
			if marshalErr != nil {
				return nil, fmt.Errorf("serialize request body: %w", marshalErr)
			}
			requestView = newOpenAIRequestView(body)
		}
	}
	// Run after orphan-output filtering and all request-map rebuilds so a
	// compaction trigger cannot remain ahead of surviving history items.
	if normalizedBody, changed, normalizeErr := NormalizeCompactionTriggerInputOrder(body); normalizeErr != nil {
		return nil, fmt.Errorf("normalize compaction trigger order: %w", normalizeErr)
	} else if changed {
		body = normalizedBody
		requestView = newOpenAIRequestView(body)
		reqBody = nil
	}
	// 剥离本会话已被上游判定失效的加密项（invalid_encrypted_content lineage），
	// 阻断同一失效密文随客户端历史在每一轮重复触发"被拒→剥离→重试/重连"。
	// lineage 会话键统一按进场形态的 body 派生：后续重试可能改写 body，
	// 延迟计算会与下一请求的进场键漂移。
	lineageGroupID := getOpenAIGroupIDFromContext(c)
	lineageEntryBody := body
	lineageSessionHash := ""
	if stateStore := s.getOpenAIWSStateStore(); stateStore != nil && stateStore.HasAnySessionInvalidEncryptedContent() {
		lineageSessionHash = s.GenerateSessionHash(c, body)
		if invalidDigests := stateStore.GetSessionInvalidEncryptedContentDigests(lineageGroupID, lineageSessionHash); len(invalidDigests) > 0 {
			strippedBody, strippedCount := s.stripSessionInvalidEncryptedContentLogged(
				body, invalidDigests, "invalid_encrypted_lineage_strip", account.ID, 0,
			)
			if strippedCount > 0 {
				body = strippedBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
			}
		}
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if imageIntent {
		var imageCfg OpenAIResponsesImageBillingConfig
		var imageCfgErr error
		if reqBody != nil {
			imageCfg, imageCfgErr = resolveOpenAIResponsesImageBillingConfigDetailed(reqBody, billingModel)
		} else {
			imageCfg, imageCfgErr = resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, billingModel)
		}
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": imageCfgErr.Error(), "param": "size"}})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}
	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	SetOpsUpstreamModel(c, upstreamModel)

	// 命中 WS 时仅走 WebSocket Mode；不再自动回退 HTTP。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		// WS 分支需要结构化 payload 与重连恢复，命中后再触发 full-map decode。
		wsReqBody, err := ensureReqBody()
		if err != nil {
			return nil, err
		}
		_, hasPreviousResponseID := wsReqBody["previous_response_id"]
		logOpenAIWSModeDebug(
			"forward_start account_id=%d account_type=%s model=%s stream=%v has_previous_response_id=%v",
			account.ID,
			account.Type,
			upstreamModel,
			reqStream,
			hasPreviousResponseID,
		)
		maxAttempts := openAIWSReconnectRetryLimit + 1
		wsAttempts := 0
		var wsResult *OpenAIForwardResult
		var wsErr error
		wsLastFailureReason := ""
		agentTaskRecoveryTried := false
		wsPrevResponseRecoveryTried := false
		wsInvalidEncryptedContentRecoveryTried := false
		recoverPrevResponseNotFound := func(attempt int) bool {
			if wsPrevResponseRecoveryTried {
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			if previousResponseID == "" {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=missing_previous_response_id previous_response_id_present=false",
					account.ID,
					attempt,
				)
				return false
			}
			if HasFunctionCallOutput(wsReqBody) {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=has_function_call_output previous_response_id_present=true",
					account.ID,
					attempt,
				)
				return false
			}
			delete(wsReqBody, "previous_response_id")
			wsPrevResponseRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_prev_response_recovery account_id=%d attempt=%d action=drop_previous_response_id retry=1 previous_response_id=%s previous_response_id_kind=%s",
				account.ID,
				attempt,
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
			)
			return true
		}
		recoverInvalidEncryptedContent := func(attempt int) bool {
			if wsInvalidEncryptedContentRecoveryTried {
				return false
			}
			// 写入 lineage 后，同一失效密文在后续 turn 进场时被预剥离，不再重复
			// 触发上游拒绝与重连。摘要取自进场形态的 body（密文项只可能来自
			// 客户端进场请求，重复摘要幂等）。
			invalidDigests := collectOpenAIEncryptedContentDigestsRaw(lineageEntryBody)
			removedReasoningItems := trimOpenAIEncryptedReasoningItems(wsReqBody)
			if !removedReasoningItems {
				logOpenAIWSModeInfo(
					"reconnect_invalid_encrypted_content_recovery_skip account_id=%d attempt=%d reason=missing_encrypted_reasoning_items",
					account.ID,
					attempt,
				)
				return false
			}
			if len(invalidDigests) > 0 {
				if lineageSessionHash == "" {
					lineageSessionHash = s.GenerateSessionHash(c, lineageEntryBody)
				}
				s.markOpenAIWSInvalidEncryptedContentLineage(lineageGroupID, lineageSessionHash, invalidDigests)
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			hasFunctionCallOutput := HasFunctionCallOutput(wsReqBody)
			if previousResponseID != "" && !hasFunctionCallOutput {
				delete(wsReqBody, "previous_response_id")
			}
			wsInvalidEncryptedContentRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_invalid_encrypted_content_recovery account_id=%d attempt=%d action=drop_encrypted_reasoning_items retry=1 previous_response_id_present=%v previous_response_id=%s previous_response_id_kind=%s has_function_call_output=%v dropped_previous_response_id=%v",
				account.ID,
				attempt,
				previousResponseID != "",
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
				hasFunctionCallOutput,
				previousResponseID != "" && !hasFunctionCallOutput,
			)
			return true
		}
		retryBudget := s.openAIWSRetryTotalBudget()
		retryStartedAt := time.Now()
	wsRetryLoop:
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			wsAttempts = attempt
			wsResult, wsErr = s.forwardOpenAIWSV2(
				ctx,
				c,
				account,
				wsReqBody,
				clientPromptCacheKey,
				wsExecutionScope,
				token,
				wsDecision,
				isCodexCLI,
				reqStream,
				originalModel,
				upstreamModel,
				startTime,
				attempt,
				wsLastFailureReason,
				&agentTaskRecoveryTried,
			)
			if wsErr == nil {
				break
			}
			if c != nil && c.Writer != nil && c.Writer.Written() {
				break
			}
			var taskRecoveredErr *agentIdentityTaskRecoveredError
			if errors.As(wsErr, &taskRecoveredErr) {
				continue
			}

			reason, retryable := classifyOpenAIWSReconnectReason(wsErr)
			if reason != "" {
				wsLastFailureReason = reason
			}
			// previous_response_not_found 说明续链锚点不可用：
			// 对非 function_call_output 场景，允许一次“去掉 previous_response_id 后重放”。
			if reason == "previous_response_not_found" && recoverPrevResponseNotFound(attempt) {
				continue
			}
			if reason == "invalid_encrypted_content" && recoverInvalidEncryptedContent(attempt) {
				continue
			}
			if retryable && attempt < maxAttempts {
				backoff := s.openAIWSRetryBackoff(attempt)
				if retryBudget > 0 && time.Since(retryStartedAt)+backoff > retryBudget {
					s.recordOpenAIWSRetryExhausted()
					logOpenAIWSModeInfo(
						"reconnect_budget_exhausted account_id=%d attempts=%d max_retries=%d reason=%s elapsed_ms=%d budget_ms=%d",
						account.ID,
						attempt,
						openAIWSReconnectRetryLimit,
						normalizeOpenAIWSLogValue(reason),
						time.Since(retryStartedAt).Milliseconds(),
						retryBudget.Milliseconds(),
					)
					break
				}
				s.recordOpenAIWSRetryAttempt(backoff)
				logOpenAIWSModeInfo(
					"reconnect_retry account_id=%d retry=%d max_retries=%d reason=%s backoff_ms=%d",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
					backoff.Milliseconds(),
				)
				if backoff > 0 {
					timer := time.NewTimer(backoff)
					select {
					case <-ctx.Done():
						if !timer.Stop() {
							<-timer.C
						}
						wsErr = wrapOpenAIWSFallback("retry_backoff_canceled", ctx.Err())
						break wsRetryLoop
					case <-timer.C:
					}
				}
				continue
			}
			if retryable {
				s.recordOpenAIWSRetryExhausted()
				logOpenAIWSModeInfo(
					"reconnect_exhausted account_id=%d attempts=%d max_retries=%d reason=%s",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
				)
			} else if reason != "" {
				s.recordOpenAIWSNonRetryableFastFallback()
				logOpenAIWSModeInfo(
					"reconnect_stop account_id=%d attempt=%d reason=%s",
					account.ID,
					attempt,
					normalizeOpenAIWSLogValue(reason),
				)
			}
			break
		}
		if wsErr == nil {
			firstTokenMs := int64(0)
			hasFirstTokenMs := wsResult != nil && wsResult.FirstTokenMs != nil
			if hasFirstTokenMs {
				firstTokenMs = int64(*wsResult.FirstTokenMs)
			}
			requestID := ""
			if wsResult != nil {
				requestID = strings.TrimSpace(wsResult.RequestID)
			}
			logOpenAIWSModeDebug(
				"forward_succeeded account_id=%d request_id=%s stream=%v has_first_token_ms=%v first_token_ms=%d ws_attempts=%d",
				account.ID,
				requestID,
				reqStream,
				hasFirstTokenMs,
				firstTokenMs,
				wsAttempts,
			)
			wsResult.UpstreamModel = upstreamModel
			if wsResult.BillingModel == "" {
				wsResult.BillingModel = billingModel
			}
			if wsResult.ImageCount > 0 {
				wsResult.ImageSize = imageSizeTier
				wsResult.ImageInputSize = imageInputSize
				wsResult.BillingModel = imageBillingModel
			}
			return wsResult, nil
		}
		s.writeOpenAIWSFallbackErrorResponse(c, account, wsErr)
		return nil, wsErr
	}

	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	// 国产模型默认 effort 补充：此处 reqModel 已被 mapping 重写为 billingModel。
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, reqModel)
	reasoningEffortValue := ""
	if reasoningEffort != nil {
		reasoningEffortValue = *reasoningEffort
	}
	firstOutputTimeout := time.Duration(0)
	if reqStream && account.Platform == PlatformOpenAI {
		firstOutputTimeout = s.openAIFirstOutputTimeout(reasoningEffortValue)
	}

	httpInvalidEncryptedContentRetryTried := false
	compactModelFallbackRetried := false
	agentTaskRecoveryTried := false
	rejectedFieldRetryState := openAIResponsesRejectedFieldRetryStateForRequest(c, body)

	// 503 流式断开自动续跑（design §5）：状态只在本次 Forward 的编排 goroutine 写。
	// contState == nil（开关关闭 / 非目标账号 / 非流式）时所有新增分支短路，
	// 第 1 次请求的 body 与行为保持逐字节一致。
	contState := s.newOpenAIStreamContinuationState(account, c, reqStream, body)
	aggregatedUsage := OpenAIUsage{}
	aggregatedImageCount := 0
	aggregatedSearchCount := 0
	var aggregatedImageOutputSizes []string
	var firstAttempt *openAIStreamFirstAttemptResult

	// buildForwardResult 统一构造唯一一条 Forward 结果：usage 取聚合值，
	// 身份/时间字段取第 1 次 attempt（design §6.2），Duration 覆盖整次 Forward。
	buildForwardResult := func(
		resp *http.Response,
		responseID string,
		firstTokenMs *int,
		serviceTier *string,
		upstreamModel string,
		usage OpenAIUsage,
		imageCount int,
		searchCount int,
		imageOutputSizes []string,
	) *OpenAIForwardResult {
		requestID := ""
		var upstreamHeaders http.Header
		if resp != nil {
			requestID = resp.Header.Get("x-request-id")
			upstreamHeaders = resp.Header
		}
		resultResponseID := responseID
		resultFirstTokenMs := firstTokenMs
		resultUpstreamModel := upstreamModel
		resultServiceTier := resolvedOpenAIUpstreamServiceTier(c, serviceTier)
		if contState != nil && firstAttempt != nil {
			requestID = firstAttempt.requestID
			upstreamHeaders = firstAttempt.upstreamHeaders
			resultResponseID = firstAttempt.responseID
			resultFirstTokenMs = firstAttempt.firstTokenMs
			resultUpstreamModel = firstAttempt.upstreamModel
			resultServiceTier = resolvedOpenAIUpstreamServiceTier(c, firstAttempt.serviceTier)
		}
		result := &OpenAIForwardResult{
			RequestID:                     requestID,
			UpstreamHeaders:               upstreamHeaders,
			ResponseID:                    resultResponseID,
			Usage:                         usage,
			Model:                         originalModel,
			BillingModel:                  billingModel,
			UpstreamModel:                 resultUpstreamModel,
			UpstreamResponseModel:         observedUpstreamResponseModel(c),
			UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
			UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
			ServiceTier:                   resultServiceTier,
			ReasoningEffort:               reasoningEffort,
			Stream:                        reqStream,
			OpenAIWSMode:                  false,
			Duration:                      time.Since(startTime),
			FirstTokenMs:                  resultFirstTokenMs,
		}
		if imageCount > 0 {
			result.ImageCount = imageCount
			result.ImageSize = imageSizeTier
			result.ImageInputSize = imageInputSize
			result.ImageOutputSizes = imageOutputSizes
			result.BillingModel = imageBillingModel
		}
		if searchCount > 0 && account != nil && account.IsGrok() {
			result.SearchCount = searchCount
		}
		stampOpenAIResponsesUpstreamEndpoint(c, result)
		return result
	}

	for {
		isContinuation := contState != nil && contState.InContinuation()
		if contState != nil && !isContinuation {
			// 始终以“原始上游 body”为续跑请求体的基座：循环内的 retry 分支可能改写
			// body，续跑 attempt 本身不覆盖它，避免第二次续跑重复追加 assistant item。
			contState.SetCurrentBody(body)
		}
		attemptBody := body
		if isContinuation {
			nextBody, buildErr := contState.NextRequestBody()
			if buildErr != nil {
				logOpenAIStream503AutoContinueExhausted(ctx, c, account, contState, "continuation_failed")
				s.writeOpenAIStreamContinuationExhausted(c, contState, contState.lastSignal, originalModel)
				return buildForwardResult(
					nil, "", nil, extractOpenAIServiceTierFromBody(body), upstreamModel,
					aggregatedUsage, aggregatedImageCount, aggregatedSearchCount, aggregatedImageOutputSizes,
				), fmt.Errorf("build openai stream continuation body: %w", buildErr)
			}
			attemptBody = nextBody
		}

		// Build upstream request
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		// R8：续跑 attempt 不再建 first-output header guard（startTime 仍是第 1 次的，
		// 建 guard 会立刻以第 1 次的时间判超时）。
		attemptFirstOutputTimeout := firstOutputTimeout
		if isContinuation {
			attemptFirstOutputTimeout = 0
			upstreamCtx = withOpenAIContinuationAttempt(upstreamCtx)
		}
		var headerGuard *openAIFirstOutputHeaderGuard
		if attemptFirstOutputTimeout > 0 {
			upstreamCtx, headerGuard = newOpenAIFirstOutputHeaderGuard(
				upstreamCtx, releaseUpstreamCtx, startTime.Add(attemptFirstOutputTimeout),
			)
		}
		upstreamReq, err := s.buildUpstreamRequest(upstreamCtx, c, account, attemptBody, token, reqStream, promptCacheKey, isCodexCLI)
		if headerGuard == nil {
			releaseUpstreamCtx()
		}
		if err != nil {
			if headerGuard != nil {
				headerGuard.close()
			}
			return nil, err
		}

		// Get proxy URL
		proxyURL := ""
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}

		// Send request
		upstreamStart := time.Now()
		resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		if headerGuard != nil && headerGuard.stopHeaderWait() {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			headerGuard.close()
			return nil, s.newOpenAIFirstOutputTimeoutError(
				ctx, c, account, opsUpstreamProxyID(account), opsUpstreamProxyName(account),
				startTime, originalModel, reasoningEffortValue,
				attemptFirstOutputTimeout, "response_headers", nil,
			)
		}
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if headerGuard != nil {
				headerGuard.close()
			}
			// B1：续跑 attempt 在出流前传输失败时，06 尚未下发本次失败事件，
			// 客户端已提交的 SSE 只能以恰好一个 response.failed 收尾。不得退化为
			// 换号 failover、不得写 JSON、不得登记账号健康副作用；usage 仍须返回
			// 已聚合的 Σ（一次 Forward 一条用量）。
			if isContinuation {
				s.writeOpenAIStreamContinuationExhausted(c, contState, contState.lastSignal, originalModel)
				return buildForwardResult(
					nil, "", nil, extractOpenAIServiceTierFromBody(body), upstreamModel,
					aggregatedUsage, aggregatedImageCount, aggregatedSearchCount, aggregatedImageOutputSizes,
				), fmt.Errorf("openai stream continuation transport failure: %w", err)
			}
			// Transport-level failure (proxy/DNS/TCP/TLS — no HTTP response). Convert to
			// a failover so the handler switches to a healthy account, and temporarily
			// unschedule the account on durable faults (e.g. rejected proxy credentials).
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
		}
		if headerGuard != nil {
			resp.Body = &openAIRequestContextReadCloser{ReadCloser: resp.Body, cleanup: headerGuard.close}
		}

		// Handle error response
		if resp.StatusCode >= 400 {
			respBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			// B1：续跑 attempt 在出流前收到 HTTP 错误时，06 尚未下发本次失败事件，
			// 已提交的 SSE 只能以恰好一个 response.failed 收尾（与
			// writeOpenAIStreamContinuationExhausted 同一 sanitize 语义）。不得走
			// failover/重试、不得把 JSON 错误体追加到流里、不得登记账号健康副作用；
			// usage 仍须返回已聚合的 Σ（一次 Forward 一条用量）。
			if isContinuation {
				s.writeOpenAIStreamContinuationExhausted(c, contState, contState.lastSignal, originalModel)
				upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
				upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
				if upstreamMsg == "" {
					upstreamMsg = fmt.Sprintf("upstream status %d", resp.StatusCode)
				}
				return buildForwardResult(
					resp, "", nil, extractOpenAIServiceTierFromBody(body), upstreamModel,
					aggregatedUsage, aggregatedImageCount, aggregatedSearchCount, aggregatedImageOutputSizes,
				), fmt.Errorf("openai stream continuation upstream error: %d %s", resp.StatusCode, upstreamMsg)
			}
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
			upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
			upstreamCode := extractUpstreamErrorCode(respBody)
			if !agentTaskRecoveryTried && s.isAgentIdentityAccount(ctx, account) && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, respBody) {
				agentTaskRecoveryTried = true
				expectedTaskID := account.GetCredential("task_id")
				if err := s.recoverAgentIdentityTask(ctx, account, expectedTaskID); err != nil {
					return nil, fmt.Errorf("agent identity task recovery failed: %w", err)
				}
				continue
			}
			respBody = s.redactAgentIdentitySensitiveBody(ctx, account, respBody)
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			invalidEncryptedContentError := upstreamCode == "invalid_encrypted_content" ||
				(upstreamCode == "thinking_signature_invalid" &&
					strings.Contains(upstreamMsg, "The encrypted content") &&
					strings.Contains(upstreamMsg, "could not be verified") &&
					strings.Contains(upstreamMsg, "could not be decrypted or parsed"))
			if !httpInvalidEncryptedContentRetryTried && resp.StatusCode == http.StatusBadRequest && invalidEncryptedContentError {
				decoded, decodeErr := ensureReqBody()
				if decodeErr != nil {
					return nil, decodeErr
				}
				invalidDigests := collectOpenAIEncryptedContentDigestsRaw(lineageEntryBody)
				if trimOpenAIEncryptedReasoningItems(decoded) {
					body, err = marshalOpenAIUpstreamJSON(decoded)
					if err != nil {
						return nil, fmt.Errorf("serialize invalid_encrypted_content retry body: %w", err)
					}
					if len(invalidDigests) > 0 {
						if lineageSessionHash == "" {
							lineageSessionHash = s.GenerateSessionHash(c, lineageEntryBody)
						}
						s.markOpenAIWSInvalidEncryptedContentLineage(lineageGroupID, lineageSessionHash, invalidDigests)
					}
					httpInvalidEncryptedContentRetryTried = true
					rejectedFieldRetryState.remember(body)
					logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request once after invalid_encrypted_content (account: %s)", account.Name)
					continue
				}
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip non-WSv2 invalid_encrypted_content retry because encrypted reasoning items are missing (account: %s)", account.Name)
			}
			if retryBody, reason, changed, retryErr := normalizeOpenAIResponsesRejectedFieldRetryBody(resp.StatusCode, body, respBody); retryErr != nil {
				return nil, fmt.Errorf("normalize rejected Responses field retry body: %w", retryErr)
			} else if changed && rejectedFieldRetryState.Allow(retryBody) {
				body = retryBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request after %s (account: %s)", reason, account.Name)
				continue
			}
			if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
				c, account, requestedModel, body, resp.StatusCode, upstreamMsg, respBody, compactModelFallbackRetried,
			); retry {
				s.appendOpenAICompactFallbackRetryOps(c, account, resp, respBody, upstreamMsg, false)
				fromModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
				body = retryBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				upstreamModel = fallbackModel
				compactModelFallbackRetried = true
				SetOpsUpstreamModel(c, fallbackModel)
				logger.LegacyPrintf(
					"service.openai_gateway",
					"[OpenAI] Retrying explicit compact request once with fallback model (account: %s, from: %s, to: %s, upstream_code: %s)",
					account.Name, fromModel, fallbackModel, upstreamCode,
				)
				continue
			}
			if s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, upstreamMsg, respBody) {
				upstreamDetail := ""
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
					if maxBytes <= 0 {
						maxBytes = 2048
					}
					upstreamDetail = truncateString(string(respBody), maxBytes)
				}
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					ProxyID:            opsUpstreamProxyID(account),
					ProxyName:          opsUpstreamProxyName(account),
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "failover",
					Message:            upstreamMsg,
					Detail:             upstreamDetail,
				})

				shouldDisable := s.handleFailoverSideEffects(ctx, resp, account, respBody, upstreamModel)
				return nil, s.newOpenAIAccountFailoverError(
					account,
					resp.StatusCode,
					resp.Header,
					respBody,
					upstreamMsg,
					shouldDisable,
					!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
				)
			}
			return s.handleErrorResponse(ctx, resp, c, account, body, resolveOpenAIErrorSchedulingModel(billingModel, upstreamModel))
		}
		defer func() { _ = resp.Body.Close() }()

		maxLineSize := defaultMaxLineSize
		if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
			maxLineSize = s.cfg.Gateway.MaxLineSize
		}
		// 顺序硬要求（design §3 决策 3）：拼接器必须最内层（紧贴上游），先于
		// client-tool 还原器，这样它面对的是原始上游事件语义。
		if isContinuation {
			resp.Body = newOpenAIStreamContinuationBody(resp.Body, contState.Snapshot(), maxLineSize, contState)
		}
		if mapping, ok := openAIResponsesClientToolMapping(c); ok && isEventStreamResponse(resp.Header) {
			resp.Body = newResponsesClientToolStreamBody(resp.Body, mapping, maxLineSize)
		}

		serviceTier := extractOpenAIServiceTierFromBody(body)
		// 上游接受后只保留计费需要的标量，避免响应处理期间继续保活完整 input/tools map。
		reqBody = nil

		// Handle normal response
		var usage *OpenAIUsage
		var firstTokenMs *int
		responseID := ""
		imageCount := 0
		searchCount := 0
		var imageOutputSizes []string
		captureFirstAttempt := func(result *openaiStreamingResult) {
			if contState == nil || firstAttempt != nil {
				return
			}
			attempt := &openAIStreamFirstAttemptResult{
				requestID:       resp.Header.Get("x-request-id"),
				upstreamHeaders: resp.Header,
				serviceTier:     serviceTier,
				upstreamModel:   upstreamModel,
			}
			if result != nil {
				attempt.responseID = strings.TrimSpace(result.responseID)
				attempt.firstTokenMs = result.firstTokenMs
			}
			firstAttempt = attempt
		}
		if reqStream {
			streamResult, err := s.handleStreamingResponseWithReasoningState(ctx, resp, c, account, startTime, originalModel, upstreamModel, reasoningEffortValue, contState)
			if err != nil {
				if signal, ok := asOpenAICompactFallbackSignal(err); ok {
					if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
						c, account, requestedModel, body, http.StatusBadRequest, signal.message, signal.payload, compactModelFallbackRetried,
					); retry {
						s.appendOpenAICompactFallbackRetryOps(c, account, resp, signal.payload, signal.message, false)
						body = retryBody
						requestView = newOpenAIRequestView(body)
						upstreamModel = fallbackModel
						compactModelFallbackRetried = true
						SetOpsUpstreamModel(c, fallbackModel)
						continue
					}
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					compactResp, compactBody := openAICompactFallbackErrorResponse(resp, signal)
					if s.shouldFailoverOpenAIUpstreamResponse(account, compactResp.StatusCode, signal.message, compactBody) {
						appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
							ProxyID:            opsUpstreamProxyID(account),
							ProxyName:          opsUpstreamProxyName(account),
							Platform:           account.Platform,
							AccountID:          account.ID,
							AccountName:        account.Name,
							UpstreamStatusCode: compactResp.StatusCode,
							UpstreamRequestID:  compactResp.Header.Get("x-request-id"),
							Kind:               "failover",
							Message:            signal.message,
						})
						shouldDisable := s.handleFailoverSideEffects(ctx, compactResp, account, compactBody, upstreamModel)
						return nil, s.newOpenAIAccountFailoverError(
							account, compactResp.StatusCode, compactResp.Header, compactBody, signal.message, shouldDisable,
							!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(compactResp.StatusCode) || isOpenAITransientProcessingError(compactResp.StatusCode, signal.message, compactBody)),
						)
					}
					return s.handleErrorResponse(ctx, compactResp, c, account, body, resolveOpenAIErrorSchedulingModel(billingModel, upstreamModel))
				}
				if sig, ok := asOpenAIStreamContinueSignal(err); ok && contState != nil {
					// 先聚合本次失败尝试的 attempt-local usage（裁定 #1：失败尝试也计入），
					// 再决定是否继续。
					attemptUsage := OpenAIUsage{}
					if streamResult != nil && streamResult.usage != nil {
						attemptUsage = *streamResult.usage
					}
					addOpenAIUsage(&aggregatedUsage, attemptUsage)
					if streamResult != nil {
						aggregatedImageCount += streamResult.imageCount
						aggregatedSearchCount += streamResult.searchCount
						aggregatedImageOutputSizes = append(aggregatedImageOutputSizes, streamResult.imageOutputSizes...)
					}
					captureFirstAttempt(streamResult)
					contState.RecordCapacitySignal(sig)
					s.appendOpenAIStreamContinuationRetryOps(c, account, sig)
					if canContinue, reason := contState.CanContinue(time.Now()); canContinue {
						contState.BeginContinuation(aggregatedUsage, sig)
						if !sleepCtx(c.Request.Context(), contState.Backoff()) {
							// 客户端在退避期间取消：不再发上游请求。
							return nil, context.Canceled
						}
						continue
					} else {
						logOpenAIStream503AutoContinueExhausted(ctx, c, account, contState, reason)
					}
					// 拒绝续跑：走现有 sanitize 路径写出恰好一个 response.failed，
					// 并返回普通 error（不是 UpstreamFailoverError）与已聚合 result。
					s.writeOpenAIStreamContinuationExhausted(c, contState, sig, originalModel)
					return buildForwardResult(
						resp, "", nil, serviceTier, upstreamModel,
						aggregatedUsage, aggregatedImageCount, aggregatedSearchCount, aggregatedImageOutputSizes,
					), fmt.Errorf("upstream response failed: %s", sig.Error())
				}
				// 非续跑错误：开关开启时仍保留已聚合 usage（一次 Forward 一条用量，
				// 失败尝试的 token 不丢）；UpstreamFailoverError 必须保持 result=nil。
				if contState != nil {
					var failoverErr *UpstreamFailoverError
					if !errors.As(err, &failoverErr) {
						if streamResult != nil && streamResult.usage != nil {
							addOpenAIUsage(&aggregatedUsage, *streamResult.usage)
						}
						if streamResult != nil {
							aggregatedImageCount += streamResult.imageCount
							aggregatedSearchCount += streamResult.searchCount
							aggregatedImageOutputSizes = append(aggregatedImageOutputSizes, streamResult.imageOutputSizes...)
						}
						captureFirstAttempt(streamResult)
						return buildForwardResult(
							resp, "", nil, serviceTier, upstreamModel,
							aggregatedUsage, aggregatedImageCount, aggregatedSearchCount, aggregatedImageOutputSizes,
						), err
					}
				}
				return nil, err
			}
			if streamResult != nil {
				if streamResult.usage != nil {
					if isContinuation {
						// 05 已把续跑终态 usage 重写为 prior(此前 Σ) + 本次 local，就是裁定 #1
						// 的 Σ；直接采用累计值，禁止再相加（会双计）。
						aggregatedUsage = *streamResult.usage
						if snapshot := contState.Snapshot(); snapshot != nil && *streamResult.usage == snapshot.priorUsage {
							// 终态未携带 usage 时 05 用 priorUsage 兜底；此刻累计值不可能增长。
							logOpenAIStream503AutoContinueTerminalUsageMissing(ctx, c, account, contState)
						}
					} else {
						addOpenAIUsage(&aggregatedUsage, *streamResult.usage)
					}
				}
				aggregatedImageCount += streamResult.imageCount
				aggregatedSearchCount += streamResult.searchCount
				aggregatedImageOutputSizes = append(aggregatedImageOutputSizes, streamResult.imageOutputSizes...)
			}
			captureFirstAttempt(streamResult)
			usage = streamResult.usage
			firstTokenMs = streamResult.firstTokenMs
			responseID = strings.TrimSpace(streamResult.responseID)
			imageCount = streamResult.imageCount
			imageOutputSizes = streamResult.imageOutputSizes
			searchCount = streamResult.searchCount
		} else {
			nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
			if err != nil {
				if signal, ok := asOpenAICompactFallbackSignal(err); ok {
					if retryBody, fallbackModel, retry := s.prepareOpenAICompactFallbackRetry(
						c, account, requestedModel, body, http.StatusBadRequest, signal.message, signal.payload, compactModelFallbackRetried,
					); retry {
						s.appendOpenAICompactFallbackRetryOps(c, account, resp, signal.payload, signal.message, false)
						body = retryBody
						requestView = newOpenAIRequestView(body)
						upstreamModel = fallbackModel
						compactModelFallbackRetried = true
						SetOpsUpstreamModel(c, fallbackModel)
						continue
					}
				}
				return nil, err
			}
			usage = nonStreamResult.usage
			responseID = strings.TrimSpace(nonStreamResult.responseID)
			imageCount = nonStreamResult.imageCount
			imageOutputSizes = nonStreamResult.imageOutputSizes
			searchCount = nonStreamResult.searchCount
		}
		s.bindHTTPResponseAccount(ctx, c, account, responseID)

		// Extract and save Codex usage snapshot from response headers (for OAuth accounts).
		// 排除 spark 影子:其 codex_* 仅由 QueryUsage(/wham/usage bengalfox)更新(外审第7轮 P1)。
		if account.UsesOpenAICodexProtocol() && !account.IsShadow() {
			if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
				s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
			}
		} else if account.IsShadow() && account.ParentAccountID != nil {
			notifyOpenAIAutoReset(*account.ParentAccountID)
		}

		if usage == nil {
			usage = &OpenAIUsage{}
		}

		resultUsage := *usage
		resultImageCount := imageCount
		resultSearchCount := searchCount
		resultImageOutputSizes := imageOutputSizes
		if contState != nil {
			resultUsage = aggregatedUsage
			resultImageCount = aggregatedImageCount
			resultSearchCount = aggregatedSearchCount
			resultImageOutputSizes = aggregatedImageOutputSizes
		}
		forwardResult := buildForwardResult(
			resp, responseID, firstTokenMs, serviceTier, upstreamModel,
			resultUsage, resultImageCount, resultSearchCount, resultImageOutputSizes,
		)
		if contState != nil && contState.Continuations() > 0 {
			logOpenAIStream503AutoContinueOK(ctx, c, account, contState, startTime, forwardResult)
		}
		return forwardResult, nil
	}
}

func shouldForwardOpenAIResponsesViaRawChatCompletions(account *Account) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	if account.IsCodeBuddy() {
		// CodeBuddy 上游只提供 /v2/chat/completions（/v1/responses 实测 404），
		// 无条件直转，不做 Responses 探测。
		return true
	}
	if account.routesByModel() {
		// Model protocol_rules are the authority. Probe Extra must not collapse
		// Grok/GPT/Muse into Chat Completions.
		return false
	}
	if account.RoutesProtocolByInbound() {
		// 按入站协议分流的供应商（国产厂商等）：显式协议配置优先于异步探针
		// Extra；adaptive 仅在供应商有原生 Responses 端点时直转，否则回退
		// Chat Completions（如 GLM）。
		switch account.GetAPIProtocol() {
		case APIProtocolChatCompletions:
			return true
		case APIProtocolAdaptive:
			return !account.SupportsNativeCNResponses()
		default:
			return false
		}
	}
	return !openai_compat.ShouldUseResponsesAPI(account.Extra)
}

func (s *OpenAIGatewayService) buildUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string, isStream bool, promptCacheKey string, isCodexCLI bool) (*http.Request, error) {
	// Determine target URL based on account type
	var targetURL string
	switch account.Type {
	case AccountTypeOAuth:
		// OAuth accounts use ChatGPT internal API
		targetURL = chatgptCodexURL
	case AccountTypeSetupToken:
		if account.IsOpenAIOAuthLike() {
			targetURL = chatgptCodexURL
		} else {
			targetURL = openaiPlatformAPIURL
		}
	case AccountTypeAPIKey:
		// API Key accounts use Platform API or custom base URL
		baseURL := account.GetOpenAIBaseURL()
		if account.UsesNativeCNResponses() && account.IsAdaptiveAPIProtocol() {
			baseURL = account.GetCNProtocolBaseURL(APIProtocolResponses)
		}
		if baseURL == "" {
			targetURL = openaiPlatformAPIURL
		} else {
			validatedURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, err
			}
			targetURL = buildOpenAIResponsesURLForPlatform(account.Platform, validatedURL)
		}
	default:
		targetURL = openaiPlatformAPIURL
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	// DeepSeek / Kimi 原生 Responses 端点为无状态实现：强制 store=false、清除
	// previous_response_id，避免携带状态字段被上游拒绝。
	body = normalizeDeepSeekResponsesRequestBody(account, body)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// Build authentication for this request. Agent Identity signs a fresh
	// assertion here; OAuth/PAT/API-key keep their existing Bearer behavior.
	authHeaders, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, fmt.Errorf("build openai authentication headers: %w", err)
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Set headers specific to OAuth accounts (ChatGPT internal API)
	if account.UsesOpenAICodexProtocol() {
		// Required: set Host for ChatGPT API (must use req.Host, not Header.Set)
		req.Host = "chatgpt.com"
		if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
			return nil, fmt.Errorf("resolve chatgpt account headers: %w", err)
		}
	}

	// Whitelist passthrough headers
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if openaiAllowedHeaders[lowerKey] {
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}
	// 客户端回带的 x-codex-turn-state 若已知由其他账号铸造（failover 换号），
	// 剥离后再出站——异账号 blob 与本账号的（指纹收敛后）出站身份自相矛盾。
	s.guardOpenAICodexTurnStateEcho(c, account, req.Header)
	// 续跑 attempt 不沿用入站 turn-state（design §3 决策 3 第 5 条）：上一回合的
	// turn 状态与“部分输出重放”的新回合语义冲突。session-id 在下面对
	// UsesOpenAICodexProtocol 分支里按客户端会话标识 / prompt_cache_key 重新派生
	// （裁定 #6）；Codex 出站不再发送 conversation_id。
	if isOpenAIContinuationAttempt(ctx) {
		req.Header.Del(openAICodexTurnStateHeader)
	}
	if account.UsesOpenAICodexProtocol() {
		compatMessagesBridge := isOpenAICompatMessagesBridgeContext(c) || isOpenAICompatMessagesBridgeBody(body)
		// 客户端可能来自不同代际：读连字符与下划线两种写法，出站统一为官方 0.163
		// 的 session-id / thread-id。清除入站原值后按隔离/收敛结果重新写入，防止
		// 跨用户会话碰撞。
		clientSessionID := firstNonEmptyHeader(req.Header, "session-id", "session_id", "conversation-id", "conversation_id")
		clientThreadID := firstNonEmptyHeader(req.Header, "thread-id", "thread_id", "x-client-request-id")
		for _, name := range []string{"session-id", "session_id", "conversation-id", "conversation_id", "thread-id", "thread_id", "x-client-request-id"} {
			req.Header.Del(name)
		}

		if compatMessagesBridge {
			req.Header.Del("OpenAI-Beta")
			req.Header.Del("originator")
		} else {
			stripOpenAILegacyResponsesBeta(req.Header)
			req.Header.Set("originator", resolveOpenAIUpstreamOriginator(c, isCodexCLI))
		}
		apiKeyID := getAPIKeyIDFromContext(c)
		// fallback 链：session-id 头 → body client_metadata.session_id →
		// prompt_cache_key → 账号级 device 兜底（applyCodexFingerprintHeaders）。
		if clientSessionID == "" {
			clientSessionID = stagedCodexFingerprintClientSessionID(c, account)
		}
		if isOpenAIResponsesCompactPath(c) {
			req.Header.Set("accept", "application/json")
			if clientSessionID == "" {
				clientSessionID = resolveOpenAICompactSessionID(c)
			}
		} else {
			req.Header.Set("accept", "text/event-stream")
			if clientSessionID == "" {
				clientSessionID = promptCacheKey
			}
		}
		if clientSessionID != "" {
			req.Header.Set("session-id", isolateOpenAIUpstreamSessionID(apiKeyID, codexAccountIdentitySource(c, account), clientSessionID))
		}
		if clientThreadID != "" {
			req.Header.Set("thread-id", clientThreadID)
			// 官方语义 x-client-request-id == thread-id；入站原值已删除，
			// 这里统一回写最终 thread-id。
			req.Header.Set("x-client-request-id", clientThreadID)
		}
	} else if isOpenAIResponsesCompactPath(c) {
		// compact 上游是 unary JSON 协议：API-key 账号也显式声明 Accept，
		// 避免 OpenAI 兼容网关按 SSE 返回（#3777 期望行为 4）。
		req.Header.Set("accept", "application/json")
	}

	// Apply custom User-Agent if configured
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}

	// 若开启 ForceCodexCLI，则强制将上游 User-Agent 伪装为规范 Codex 身份。
	// 用于网关未透传/改写 User-Agent 时，仍能命中 Codex 侧识别逻辑。
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", CodexCanonicalUserAgent())
	}

	// 账号 namespace 不改变客户端身份基数，但确保 scheduler failover 后不会把
	// 同一组 Codex IDs 发送给另一份 OAuth 凭据。可选指纹收敛随后仍可覆盖这些值。
	applyCodexAccountIdentityHeaders(req.Header, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c))

	// 指纹收敛：使用 Forward() 中预计算的收敛 ID 改写出站头，与请求体使用同一份 IDs。
	applyStagedCodexFingerprintHeaders(c, account, req.Header)

	// 终态收口：强制统一 OAuth 出站身份（User-Agent / originator 同源自洽；
	// 官方 CLI 不发送 version 头，该头由收口函数删除）。
	// 客户端自报身份不参与构造，浏览器型 UA 也因此不会再到达上游（原浏览器 UA 兜底已被吸收）。
	if account.UsesOpenAICodexProtocol() {
		enforceCodexIdentityHeadersWithUA(req.Header, s.codexIdentityOverrideUA(account))
	}

	// Ensure required headers exist
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	// 官方 OpenCode / Command Code 上游收敛为规范客户端 UA：客户端透传的编程库
	// UA 会命中其前置 Cloudflare bot 拦截（CF 1010/403），并被计入账号 403 strike。
	applyOpenCodeUpstreamUserAgent(account, targetURL, req.Header)

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效；OAuth 路径 no-op）
	account.ApplyHeaderOverrides(req.Header)
	if account.UsesOpenAICodexProtocol() {
		// 官方 Codex CLI 从不发送 version 头；即使账号覆写配置了也一并删除。
		// API-key / 第三方兼容上游不受此约束，保留管理员覆写能力。
		req.Header.Del("version")
	}
	applyOpenCodeSessionHeader(c, account, targetURL, req.Header, body, openCodeSessionHintBody(promptCacheKey))
	// x-codex-beta-features：按真实 Codex 的会话级行为补注（在账号级覆写之后，
	// 保证不被覆盖丢失）。
	applyOpenAICodexBetaFeatures(c, account, req.Header)
	setOpenAICodexRoutingHintFromBody(req.Header, account, body)
	logOpenAIRoutingDiagnosticsFromBody(ctx, account, "http", req.Header, body, "not_applicable")

	if err := applyMappedGPT55LiteCompatibility(req, account, body); err != nil {
		return nil, err
	}
	return req, nil
}

// codexIdentityOverrideUA 返回账号级显式配置的出站 User-Agent，供强制统一身份时作为覆写来源。
// ForceCodexCLI 语义是「强制使用 Codex CLI 身份」，等价于使用网关规范身份，故返回空串；
// 该优先级与历史行为一致（ForceCodexCLI 在账号自定义 UA 之后生效）。
func (s *OpenAIGatewayService) codexIdentityOverrideUA(account *Account) string {
	if s != nil && s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		return ""
	}
	return account.GetOpenAIUserAgent()
}

// openAIStreamFirstAttemptResult 冻结第 1 次 attempt 的身份/时间字段，
// 保证续跑后 OpenAIForwardResult 的 RequestID/ResponseID/FirstTokenMs 等仍取第 1 次
// （design §6.2）。
type openAIStreamFirstAttemptResult struct {
	requestID       string
	upstreamHeaders http.Header
	responseID      string
	firstTokenMs    *int
	serviceTier     *string
	upstreamModel   string
}

// openAIContinuationAttemptContextKey 标记“本次上游请求是 503 续跑 attempt”。
// buildUpstreamRequest 在该标记下剥离入站 turn-state（design §3 决策 3 第 5 条）。
type openAIContinuationAttemptContextKey struct{}

func withOpenAIContinuationAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIContinuationAttemptContextKey{}, true)
}

func isOpenAIContinuationAttempt(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(openAIContinuationAttemptContextKey{}).(bool)
	return enabled
}

// sleepCtx 是可被 ctx 取消的退避等待。返回 false 表示等待期间 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if ctx == nil {
		if d > 0 {
			time.Sleep(d)
		}
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// appendOpenAIStreamContinuationRetryOps 每次续跑前追加一条 ops 事件
// （design 决策 7：Kind=retry，Reason=openai_stream_503_auto_continue）。
func (s *OpenAIGatewayService) appendOpenAIStreamContinuationRetryOps(c *gin.Context, account *Account, sig *openAIStreamContinueSignal) {
	if sig == nil {
		return
	}
	detail := ""
	if s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = truncateString(string(sig.capacityPayload), maxBytes)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusServiceUnavailable,
		UpstreamRequestID:  sig.upstreamRequestID,
		Kind:               "retry",
		Reason:             "openai_stream_503_auto_continue",
		Message:            sig.message,
		Detail:             detail,
	})
}

// writeOpenAIStreamContinuationExhausted 在 07 拒绝续跑时，按现有 sanitize 路径
// 向客户端写出恰好一个 response.failed（code=server_error，保留上游原文）。
// 06 的资格门是权威门，因此该分支只在“06 已放行但 07 复核拒绝”（例如总耗时跨线）
// 或续跑请求体构造失败时使用；常规预算耗尽仍由 06 的既有路径下发。
func (s *OpenAIGatewayService) writeOpenAIStreamContinuationExhausted(
	c *gin.Context,
	cont *openAIStreamContinuationState,
	sig *openAIStreamContinueSignal,
	model string,
) {
	if c == nil || c.Writer == nil || sig == nil {
		return
	}
	payload := append([]byte(nil), sig.capacityPayload...)
	if len(payload) == 0 {
		return
	}
	if rewritten, changed := sanitizeOpenAICapacityShedErrorCodeForClient(payload); changed {
		payload = rewritten
	}
	var frame string
	if effectiveOpenAISSEEventType(payload, "") == "error" {
		// T3/T4 复用的是裸 error 帧：合成为协议合法的 response.failed 终态。
		responseID := ""
		createdAt := int64(0)
		if cont != nil {
			responseID = cont.responseID
			createdAt = cont.createdAt
		}
		frame = buildOpenAIResponseFailedSSE(responseID, model, createdAt, payload, sig.message)
	} else {
		sanitized, _ := sanitizeOpenAIResponseFailedEventForClient(payload, "response.failed", true)
		frame = "event: response.failed\ndata: " + string(sanitized) + "\n\n"
	}
	if _, err := c.Writer.WriteString(frame); err != nil {
		return
	}
	c.Writer.Flush()
	MarkResponseCommitted(c)
}

// logOpenAIStream503AutoContinueOK 是成功收尾日志（design §7.1）。
func logOpenAIStream503AutoContinueOK(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	cont *openAIStreamContinuationState,
	startTime time.Time,
	result *OpenAIForwardResult,
) {
	if cont == nil {
		return
	}
	extraElapsedMs := int64(0)
	if !cont.firstTriggerAt.IsZero() {
		extraElapsedMs = time.Since(cont.firstTriggerAt).Milliseconds()
	}
	aggInputTokens := 0
	aggOutputTokens := 0
	if result != nil {
		aggInputTokens = result.Usage.InputTokens
		aggOutputTokens = result.Usage.OutputTokens
	}
	fields := []zap.Field{
		zap.String("path", "native_sse"),
		zap.String("endpoint", openAIStreamContinuationEndpoint(c)),
		zap.Int("continuation_count", cont.Continuations()),
		zap.Int64("extra_elapsed_ms", extraElapsedMs),
		zap.Int("agg_input_tokens", aggInputTokens),
		zap.Int("agg_output_tokens", aggOutputTokens),
		zap.Int("dropped_reasoning_events", cont.DroppedReasoningEvents()),
		zap.Int("overlap_trimmed_bytes", cont.OverlapTrimmedBytes()),
		zap.Int64("elapsed_ms", time.Since(startTime).Milliseconds()),
	}
	if account != nil {
		fields = append(fields,
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("platform", account.Platform),
		)
	}
	logger.FromContext(ctx).Info("gateway.openai_stream_503_auto_continue_ok", fields...)
}

// logOpenAIStream503AutoContinueTerminalUsageMissing 记录“续跑终态未携带 usage”
// （design §6.3.4）。05 会用 priorUsage 兜底重写终态，因此这里看到的是累计值未增长。
func logOpenAIStream503AutoContinueTerminalUsageMissing(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	cont *openAIStreamContinuationState,
) {
	fields := []zap.Field{
		zap.String("path", "native_sse"),
		zap.String("endpoint", openAIStreamContinuationEndpoint(c)),
		zap.Int("continuation_index", cont.Continuations()),
	}
	if account != nil {
		fields = append(fields,
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("platform", account.Platform),
		)
	}
	logger.FromContext(ctx).Warn("gateway.openai_stream_503_auto_continue_usage_missing_on_continuation_terminal", fields...)
}
