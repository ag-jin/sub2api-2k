//go:build unit

package service

// 06 — native SSE 流处理触发点（T1/T2/T3/T4）与观察接线。
//
// 这些用例直接调用 handleStreamingResponseWithReasoningState（本卡新增的
// cont 变体），用假 *http.Response 喂上游 SSE，不启动上游、不调用 Forward，
// 因此只验证“流处理函数在何处返回续跑信号 / 不返回”，编排由 07 的测试覆盖。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	contTestCreatedFrame      = `{"type":"response.created","response":{"id":"resp_cont"},"sequence_number":0}`
	contTestMessageAddedFrame = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_cont","type":"message","role":"assistant","content":[]},"sequence_number":1}`
	contTestPartialDeltaFrame = `{"type":"response.output_text.delta","item_id":"msg_cont","output_index":0,"content_index":0,"delta":"partial","sequence_number":2}`
	contTestFunctionCallFrame = `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_cont","type":"function_call","name":"lookup","arguments":""},"sequence_number":3}`

	contTestCapacityFailedFrame = `{"type":"response.failed","response":{"id":"resp_cont","status":"failed","usage":{"input_tokens":7,"output_tokens":3},"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}},"sequence_number":3}`
	contTestCapacityErrorFrame  = `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"sequence_number":2}`
	contTestNonCapacityFrame    = `{"type":"response.failed","response":{"id":"resp_cont","status":"failed","error":{"code":"server_error","message":"boom"}},"sequence_number":3}`
	contTestCompletedFrame      = `{"type":"response.completed","response":{"id":"resp_cont","status":"completed","usage":{"input_tokens":5,"output_tokens":2},"output":[]},"sequence_number":4}`

	contTestOverloadedMessage = "Our servers are currently overloaded. Please try again later."
)

// contTestSSE 把一组 data 帧拼成 SSE 字节流。故意不带 event: 行：event type
// 由 payload 的 type 字段推导（effectiveOpenAISSEEventType），与真实上游一致。
func contTestSSE(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		b.WriteString("data: ")
		b.WriteString(event)
		b.WriteString("\n\n")
	}
	return b.String()
}

func contTestService(gateway config.GatewayConfig) *OpenAIGatewayService {
	if gateway.MaxLineSize == 0 {
		gateway.MaxLineSize = defaultMaxLineSize
	}
	return &OpenAIGatewayService{cfg: &config.Config{Gateway: gateway}}
}

func contTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

func contTestResponse(body io.ReadCloser, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Request-Id": []string{requestID}},
		Body:       body,
	}
}

func contTestState(t *testing.T, svc *OpenAIGatewayService, c *gin.Context, account *Account) *openAIStreamContinuationState {
	t.Helper()
	st := svc.newOpenAIStreamContinuationState(account, c, true, []byte(`{"model":"model","input":[]}`))
	require.NotNil(t, st)
	return st
}

func contTestRun(
	t *testing.T,
	svc *OpenAIGatewayService,
	c *gin.Context,
	account *Account,
	cont *openAIStreamContinuationState,
	body io.ReadCloser,
	requestID string,
) (*openaiStreamingResult, error) {
	t.Helper()
	resp := contTestResponse(body, requestID)
	return svc.handleStreamingResponseWithReasoningState(
		c.Request.Context(), resp, c, account, time.Now(), "model", "model", "", cont,
	)
}

func countContinuationLogMessages(sink *inMemoryLogSink, message string) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	count := 0
	for _, ev := range sink.events {
		if ev != nil && ev.Message == message {
			count++
		}
	}
	return count
}

func continuationLogFields(sink *inMemoryLogSink, message string) map[string]any {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, ev := range sink.events {
		if ev != nil && ev.Message == message {
			return ev.Fields
		}
	}
	return nil
}

// --- T1: response.failed（权威终态） ---

func TestOpenAIStream503AutoContinue_FailedAfterOutputReturnsSignal(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-t1")
	require.Error(t, err)

	sig, ok := asOpenAIStreamContinueSignal(err)
	require.True(t, ok, "已输出后遇到 response.failed 降载必须返回续跑信号，实际: %v", err)
	require.Equal(t, contTestOverloadedMessage, sig.message)
	require.Equal(t, "rid-cont-t1", sig.upstreamRequestID)
	require.JSONEq(t, contTestCapacityFailedFrame, string(sig.capacityPayload))
	require.Equal(t, 7, sig.usage.InputTokens)
	require.Equal(t, 3, sig.usage.OutputTokens)
	require.Equal(t, "partial", cont.ObservedPartialText())
	require.True(t, cont.HasOpenTextItem())

	// 接管时不写客户端、不发 suppressed 日志、不登记 ops 失败。
	body := rec.Body.String()
	require.Contains(t, body, `"delta":"partial"`)
	require.NotContains(t, body, `"type":"response.failed"`)
	require.NotContains(t, body, "event: error")
	require.False(t, logSink.ContainsMessage("gateway.failover_suppressed_after_semantic_output"))
	_, opsRecorded := c.Get(OpsUpstreamErrorsKey)
	require.False(t, opsRecorded, "接管时不得调用 recordOpenAIStreamUpstreamError / 账号健康副作用")

	require.Equal(t, 1, countContinuationLogMessages(logSink, "gateway.openai_stream_503_auto_continue"))
	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue")
	require.NotNil(t, fields)
	require.Equal(t, "native_sse", fields["path"])
	require.Equal(t, "/v1/responses", fields["endpoint"])
	require.Equal(t, "model", fields["model"])
	require.Equal(t, "rid-cont-t1", fields["upstream_request_id"])
	require.EqualValues(t, 7, fields["account_id"])
	require.Equal(t, "cont-test", fields["account_name"])
	require.Equal(t, PlatformOpenAI, fields["platform"])
	require.EqualValues(t, 7, fields["partial_text_bytes"])
	require.EqualValues(t, 1, fields["continuation_index"])
	require.EqualValues(t, openAIStream503AutoContinueMaxContinuations, fields["budget_left"])
	require.Contains(t, fields, "elapsed_ms")
	require.Contains(t, fields, "client_request_id")
}

func TestOpenAIStream503AutoContinue_NonCapacityFailedKeepsLegacyStream(t *testing.T) {
	stream := contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestNonCapacityFrame,
	)

	run := func(enableContinuation bool) (string, error) {
		svc := contTestService(config.GatewayConfig{})
		c, rec := contTestContext()
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, enableContinuation, false)
		var cont *openAIStreamContinuationState
		if enableContinuation {
			cont = contTestState(t, svc, c, account)
		}
		_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(stream)), "rid-cont-noncap")
		return rec.Body.String(), err
	}

	disabledBody, disabledErr := run(false)
	enabledBody, enabledErr := run(true)

	require.Error(t, disabledErr)
	require.Error(t, enabledErr)
	require.Equal(t, disabledBody, enabledBody, "开关开启但非降载失败时下游字节必须与关闭态一致")
	require.Equal(t, disabledErr.Error(), enabledErr.Error())
	_, isSignal := asOpenAIStreamContinueSignal(enabledErr)
	require.False(t, isSignal)
	require.Equal(t, 1, strings.Count(enabledBody, `"type":"response.failed"`))
}

func TestOpenAIStream503AutoContinue_FunctionCallInProgressIsNotEligible(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestFunctionCallFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-tool")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal, "function_call 参数流进行中不得续跑")

	body := rec.Body.String()
	require.Contains(t, body, `"delta":"partial"`)
	require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`))
	require.Contains(t, body, `"code":"server_error"`)

	require.True(t, logSink.ContainsMessage("gateway.failover_suppressed_after_semantic_output"))
	require.Equal(t, 1, countContinuationLogMessages(logSink, "gateway.openai_stream_503_auto_continue_exhausted"))
	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
	require.NotNil(t, fields)
	require.Equal(t, openAIStream503AutoContinueReasonIneligibleOutput, fields["reason"])
	require.Equal(t, "native_sse", fields["path"])
}

func TestOpenAIStream503AutoContinue_BudgetExhaustedKeepsLegacyPath(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)
	cont.continuations = openAIStream503AutoContinueMaxContinuations

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-budget")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal, "预算耗尽时不得续跑")

	body := rec.Body.String()
	require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`))
	require.Contains(t, body, `"code":"server_error"`)

	require.True(t, logSink.ContainsMessage("gateway.failover_suppressed_after_semantic_output"))
	require.Equal(t, 1, countContinuationLogMessages(logSink, "gateway.openai_stream_503_auto_continue_exhausted"))
	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
	require.NotNil(t, fields)
	require.Equal(t, openAIStream503AutoContinueReasonBudget, fields["reason"])
}

// 回归（v0.1.221 热修）：keepalive>0 时流处理走异步扫描路径。异步循环在队列
// 未排空时不做 event-boundary flush，created/added/delta 会停留在 attempt 本地
// 缓冲区；若在 failed 事件评估 outputStarted 之前不 flush，已 staged 的语义帧会
// 被判成“尚未输出”，错误返回 pre-output failover 并丢弃这些帧（客户端空 body）。
//
// 基线没有续跑 keepalive 兜底（openAIStreamKeepaliveInterval），因此这里显式用
// 1s keepalive 触发异步路径；预算耗尽的续跑状态使 stageFirstOutput=false，走与
// 生产 bug 相同的非 staging 异步分支。
func TestOpenAIStream503AutoContinue_KeepaliveAsyncPathKeepsStagedOutputOnExhaustedBudget(t *testing.T) {
	// 真实上游会带 "event:" 行；测试历史上省略它。两种线格式都必须保证
	// 失败事件之前 staged 的语义帧先出站。
	dataOnlySSE := contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	)
	explicitEventSSE := "event: response.created\ndata: " + contTestCreatedFrame + "\n\n" +
		"event: response.output_item.added\ndata: " + contTestMessageAddedFrame + "\n\n" +
		"event: response.output_text.delta\ndata: " + contTestPartialDeltaFrame + "\n\n" +
		"event: response.failed\ndata: " + contTestCapacityFailedFrame + "\n\n"

	for _, tt := range []struct {
		name string
		sse  string
	}{
		{name: "data_only_frames", sse: dataOnlySSE},
		{name: "explicit_event_lines", sse: explicitEventSSE},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logSink, restore := captureStructuredLog(t)
			defer restore()

			svc := contTestService(config.GatewayConfig{StreamKeepaliveInterval: 1})
			c, rec := contTestContext()
			account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
			cont := contTestState(t, svc, c, account)
			cont.continuations = openAIStream503AutoContinueMaxContinuations
			require.True(t, cont.InContinuation(), "续跑 attempt 跳过 first-output staging，走异步扫描路径")

			_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(tt.sse)), "rid-cont-async-budget")
			require.Error(t, err)
			_, isSignal := asOpenAIStreamContinueSignal(err)
			require.False(t, isSignal, "预算耗尽时不得续跑")
			require.NotContains(t, err.Error(), "failover", "已 stage 语义输出后的失败不得降级为 pre-output failover")

			body := rec.Body.String()
			require.Contains(t, body, `"type":"response.created"`)
			require.Contains(t, body, `"delta":"partial"`, "已 staged 的语义帧必须写入客户端")
			require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`))
			require.Contains(t, body, `"code":"server_error"`)

			require.True(t, logSink.ContainsMessage("gateway.failover_suppressed_after_semantic_output"))
			fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
			require.NotNil(t, fields)
			require.Equal(t, openAIStream503AutoContinueReasonBudget, fields["reason"])
		})
	}
}

// 对照：keepalive=0 走同步扫描路径，行为必须不变。同步路径在每个事件边界
// （queueDrained=true）已 flush staged 语义帧，失败事件判定 outputStarted 时不
// 需要 flush 前置逻辑。
func TestOpenAIStream503AutoContinue_SyncPathKeepsStagedOutputOnExhaustedBudget(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)
	cont.continuations = openAIStream503AutoContinueMaxContinuations
	require.True(t, cont.InContinuation())

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-sync-budget")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal, "预算耗尽时不得续跑")
	require.NotContains(t, err.Error(), "failover")

	body := rec.Body.String()
	require.Contains(t, body, `"type":"response.created"`)
	require.Contains(t, body, `"delta":"partial"`)
	require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`))
	require.Contains(t, body, `"code":"server_error"`)

	require.True(t, logSink.ContainsMessage("gateway.failover_suppressed_after_semantic_output"))
	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
	require.NotNil(t, fields)
	require.Equal(t, openAIStream503AutoContinueReasonBudget, fields["reason"])
}

func TestOpenAIStream503AutoContinue_SwitchOffEmitsNoContinuationLogs(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, false, false)

	_, err := contTestRun(t, svc, c, account, nil, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityFailedFrame,
	))), "rid-cont-off")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal)

	body := rec.Body.String()
	require.Contains(t, body, `"delta":"partial"`)
	require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`))
	require.True(t, logSink.ContainsMessage("gateway.failover_suppressed_after_semantic_output"))
	require.Zero(t, countContinuationLogMessages(logSink, "gateway.openai_stream_503_auto_continue"))
	require.Zero(t, countContinuationLogMessages(logSink, "gateway.openai_stream_503_auto_continue_exhausted"))
}

// --- 纯读错误（无降载帧）：保持现状 ---

func TestOpenAIStream503AutoContinue_PlainReadErrorDoesNotContinue(t *testing.T) {
	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)

	payload := contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
	)
	_, err := contTestRun(t, svc, c, account, cont,
		&streamReadCloser{payload: []byte(payload), err: io.ErrUnexpectedEOF}, "rid-cont-read")
	require.Error(t, err)
	_, isSignal := asOpenAIStreamContinueSignal(err)
	require.False(t, isSignal, "没有降载帧的纯读错误不得续跑")

	body := rec.Body.String()
	require.Contains(t, body, `"delta":"partial"`)
	require.Contains(t, body, "event: error", "既有 stream_read_error 事件必须保留")
	require.Contains(t, body, `"type":"error"`)
}

// --- T3: 降载帧后读错误 ---

func TestOpenAIStream503AutoContinue_CodexBareErrorCapacityThenReadErrorReturnsSignal(t *testing.T) {
	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)

	payload := contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityErrorFrame,
	)
	_, err := contTestRun(t, svc, c, account, cont,
		&streamReadCloser{payload: []byte(payload), err: io.ErrUnexpectedEOF}, "rid-cont-t3")
	require.Error(t, err)

	sig, ok := asOpenAIStreamContinueSignal(err)
	require.True(t, ok, "codex 账号裸 error 降载后读错误必须续跑（T3），实际: %v", err)
	require.Equal(t, contTestOverloadedMessage, sig.message)
	require.Equal(t, "rid-cont-t3", sig.upstreamRequestID)

	body := rec.Body.String()
	require.Contains(t, body, `"delta":"partial"`)
	require.NotContains(t, body, "event: error")
	require.NotContains(t, body, `"type":"error"`)
	require.NotContains(t, body, `"type":"response.failed"`)
}

// --- T4: 降载帧后空闲超时 ---

func TestOpenAIStream503AutoContinue_CodexBareErrorCapacityThenIdleTimeoutReturnsSignal(t *testing.T) {
	svc := contTestService(config.GatewayConfig{StreamDataIntervalTimeout: 1})
	c, rec := contTestContext()
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	cont := contTestState(t, svc, c, account)

	payload := contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityErrorFrame,
	)
	body := &contBlockingReadCloser{payload: []byte(payload), release: make(chan struct{})}
	_, err := contTestRun(t, svc, c, account, cont, body, "rid-cont-t4")
	require.Error(t, err)

	sig, ok := asOpenAIStreamContinueSignal(err)
	require.True(t, ok, "codex 账号裸 error 降载后空闲超时必须续跑（T4），实际: %v", err)
	require.Equal(t, contTestOverloadedMessage, sig.message)
	require.Equal(t, "rid-cont-t4", sig.upstreamRequestID)

	downstream := rec.Body.String()
	require.Contains(t, downstream, `"delta":"partial"`)
	require.NotContains(t, downstream, `"type":"error"`)
	require.NotContains(t, downstream, `stream_timeout`)
}

// --- T2: 裸 error（仅非 codex 终态账号） ---

func TestOpenAIStream503AutoContinue_NonCodexBareErrorCapacityReturnsSignal(t *testing.T) {
	svc := contTestService(config.GatewayConfig{})
	c, rec := contTestContext()
	account := &Account{
		ID:       9,
		Name:     "apikey-account",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{openAIStream503AutoContinueKey: true},
	}
	// v1 不会为 APIKey 账号创建续跑状态；这里直接构造状态以覆盖 T2 分支。
	cont := &openAIStreamContinuationState{enabled: true, accountID: account.ID}

	_, err := contTestRun(t, svc, c, account, cont, io.NopCloser(strings.NewReader(contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCapacityErrorFrame,
	))), "rid-cont-t2")
	require.Error(t, err)

	sig, ok := asOpenAIStreamContinueSignal(err)
	require.True(t, ok, "非 codex 终态账号裸 error 降载必须续跑（T2），实际: %v", err)
	require.Equal(t, contTestOverloadedMessage, sig.message)

	downstream := rec.Body.String()
	require.Contains(t, downstream, `"delta":"partial"`)
	require.NotContains(t, downstream, `"type":"error"`)
}

// --- R8: 续跑 attempt 关闭 first-output 超时与 staging ---

func TestOpenAIStream503AutoContinue_ContinuationAttemptSkipsFirstOutputTimeout(t *testing.T) {
	stream := contTestSSE(
		contTestCreatedFrame,
		contTestMessageAddedFrame,
		contTestPartialDeltaFrame,
		contTestCompletedFrame,
	)
	gateway := config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}

	t.Run("initial_attempt_still_enforces_first_output_timeout", func(t *testing.T) {
		svc := contTestService(gateway)
		c, _ := contTestContext()
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)

		resp := contTestResponse(&contDelayedReadCloser{payload: []byte(stream), delay: 50 * time.Millisecond}, "rid-cont-r8-control")
		// startTime 已在超时之后：未开启续跑 attempt 语义时，first-output 计时器
		// 必须立刻触发，证明下面的续跑用例不是“恰好没超时”。
		_, err := svc.handleStreamingResponseWithReasoningState(
			c.Request.Context(), resp, c, account, time.Now().Add(-2*time.Second), "model", "model", "", nil,
		)
		require.Error(t, err)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	})

	t.Run("continuation_attempt_has_no_first_output_timeout", func(t *testing.T) {
		svc := contTestService(gateway)
		c, rec := contTestContext()
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		cont := contTestState(t, svc, c, account)
		cont.BeginContinuation(OpenAIUsage{}, nil)
		require.True(t, cont.InContinuation())

		resp := contTestResponse(&contDelayedReadCloser{payload: []byte(stream), delay: 50 * time.Millisecond}, "rid-cont-r8")
		result, err := svc.handleStreamingResponseWithReasoningState(
			c.Request.Context(), resp, c, account, time.Now().Add(-2*time.Second), "model", "model", "", cont,
		)
		require.NoError(t, err, "续跑 attempt 不得触发 first-output 超时（R8）")
		require.NotNil(t, result)
		require.Contains(t, rec.Body.String(), `"type":"response.completed"`)
	})
}

// contBlockingReadCloser 先一次性交付 payload，之后的 Read 阻塞到 Close，
// 用于触发上游“空闲超时”分支（T4）。
type contBlockingReadCloser struct {
	payload []byte
	sent    bool
	release chan struct{}
	once    sync.Once
}

func (r *contBlockingReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.payload), nil
	}
	<-r.release
	return 0, io.EOF
}

func (r *contBlockingReadCloser) Close() error {
	r.once.Do(func() { close(r.release) })
	return nil
}

// contDelayedReadCloser 延迟交付 payload，用于让 first-output 计时器与首帧
// 到达产生确定的先后顺序（R8 对照）。
type contDelayedReadCloser struct {
	payload []byte
	delay   time.Duration
	sent    bool
}

func (r *contDelayedReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		time.Sleep(r.delay)
		return copy(p, r.payload), nil
	}
	return 0, io.EOF
}

func (r *contDelayedReadCloser) Close() error { return nil }
