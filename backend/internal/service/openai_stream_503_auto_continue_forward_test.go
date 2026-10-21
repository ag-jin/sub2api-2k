//go:build unit

package service

// 07 — Forward 续跑编排集成测试。
//
// 这些用例用 httpUpstreamRecorder 串联多次上游响应并调用 svc.Forward，验证：
// 同一账号/同一 slot/一次 Forward；客户端事件流被 05 拼成一条连续流；usage 按
// 裁定 #1 恰好计一次（Σ）；结果字段取第 1 次 attempt；退避可被 ctx 取消。

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/tidwall/gjson"
)

const (
	contForwardFirstRequestID  = "rid-cont-forward-1"
	contForwardSecondRequestID = "rid-cont-forward-2"
	contForwardThirdRequestID  = "rid-cont-forward-3"

	contForwardCreatedFrame = `{"type":"response.created","sequence_number":1,"response":{"id":"resp_1","created_at":1700000000}}`
	contForwardMessageAdded = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]},"sequence_number":2}`
	contForwardHelloDelta   = `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello wor","sequence_number":3}`
	contForwardOverloaded   = `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"sequence_number":4}`
	contForwardFailedFrame  = `{"type":"response.failed","response":{"id":"resp_1","status":"failed","usage":{"input_tokens":10,"output_tokens":5},"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}},"sequence_number":5}`

	contForwardContinuationCreated  = `{"type":"response.created","sequence_number":1,"response":{"id":"resp_2","created_at":1700000001}}`
	contForwardContinuationAdded    = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant","content":[]},"sequence_number":2}`
	contForwardContinuationDelta    = `{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"ld!","sequence_number":3}`
	contForwardContinuationFullText = `{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"Hello world!","sequence_number":3}`

	contForwardReasoningAdded = `{"type":"response.output_item.added","output_index":1,"item":{"id":"rs_1","type":"reasoning","summary":[]},"sequence_number":3}`
	contForwardReasoningDelta = `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":1,"summary_index":0,"delta":"thinking","sequence_number":4}`

	// 三段拼接用例：每段都在文本中途被容量降载打断（前两段）或正常收尾（第三段）。
	contForwardSegment1Delta = `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello ","sequence_number":3}`
	contForwardSegment2Delta = `{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"wor","sequence_number":3}`
	contForwardSegment3Added = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_3","type":"message","role":"assistant","content":[]},"sequence_number":2}`
	contForwardSegment3Delta = `{"type":"response.output_text.delta","item_id":"msg_3","output_index":0,"content_index":0,"delta":"ld!","sequence_number":3}`

	contForwardFunctionAdded = `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":""},"sequence_number":4}`
	contForwardFunctionDone  = `{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{}"},"sequence_number":5}`

	// v1.1 零 partial：attempt-1 只下发 created / reasoning 帧。
	contForwardZeroAttemptOneReasoningAdded = `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]},"sequence_number":2}`
	contForwardZeroAttemptOneReasoningDelta = `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"thinking","sequence_number":3}`
	contForwardZeroAttemptTwoReasoningAdded = `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_2","type":"reasoning","summary":[]},"sequence_number":2}`
	contForwardZeroAttemptTwoReasoningDelta = `{"type":"response.reasoning_summary_text.delta","item_id":"rs_2","output_index":0,"summary_index":0,"delta":"more","sequence_number":3}`
	contForwardZeroMessageAdded             = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant","content":[]},"sequence_number":4}`
	contForwardZeroAnswerDelta              = `{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"answer","sequence_number":5}`
)

// contForwardZeroPartialFailure 是“推理期容量降载”的 attempt-1：
// 客户端只见过 created + reasoning 帧，没有任何文本 item。
func contForwardZeroPartialFailure() *http.Response {
	return contForwardStream(contForwardFirstRequestID,
		contForwardCreatedFrame,
		contForwardZeroAttemptOneReasoningAdded,
		contForwardZeroAttemptOneReasoningDelta,
		contForwardOverloaded,
		contForwardFailedFrame,
	)
}

// contForwardZeroPartialSuccess 是零 partial 的 attempt-2：
// 新 reasoning 应被丢弃，message 保留自己的 id 并重排索引。
func contForwardZeroPartialSuccess(requestID string) *http.Response {
	return contForwardStream(requestID,
		contForwardContinuationCreated,
		contForwardZeroAttemptTwoReasoningAdded,
		contForwardZeroAttemptTwoReasoningDelta,
		contForwardZeroMessageAdded,
		contForwardZeroAnswerDelta,
		contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`,
			`[{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]`),
	)
}

func contForwardRequest() []byte {
	return []byte(`{"model":"gpt-5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"say hello"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
}

func contForwardAccount(enabled bool) *Account {
	extra := map[string]any{}
	if enabled {
		extra[openAIStream503AutoContinueKey] = true
	}
	return &Account{
		ID:          77,
		Name:        "cont-forward",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
		},
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
		Extra:          extra,
	}
}

func contForwardResponse(requestID string, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: body,
	}
}

func contForwardStream(requestID string, events ...string) *http.Response {
	return contForwardResponse(requestID, io.NopCloser(strings.NewReader(contForwardSSE(events...))))
}

// contForwardJSONErrorResponse 模拟上游在出流前直接返回 HTTP 错误（非 SSE）。
func contForwardJSONErrorResponse(requestID string, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

// contForwardSSE 按真实上游形态生成 event + data 行，便于断言
// "event: response.created" / "event: response.completed" 的出现次数。
func contForwardSSE(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		eventType := gjson.Get(event, "type").String()
		if eventType != "" {
			b.WriteString("event: ")
			b.WriteString(eventType)
			b.WriteString("\n")
		}
		b.WriteString("data: ")
		b.WriteString(event)
		b.WriteString("\n\n")
	}
	return b.String()
}

func contForwardCompletedFrame(usageJSON string, output string) string {
	usage := ""
	if usageJSON != "" {
		usage = `"usage":` + usageJSON + `,`
	}
	return `{"type":"response.completed","response":{"id":"resp_2","status":"completed",` + usage + `"output":` + output + `},"sequence_number":6}`
}

func contForwardTextOutput() string {
	return `[{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"ld!"}]}]`
}

func contForwardRun(t *testing.T, svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	t.Helper()
	return svc.Forward(c.Request.Context(), c, account, body)
}

func contForwardContext(ctx context.Context) (*gin.Context, *httptest.ResponseRecorder, []byte) {
	gin.SetMode(gin.TestMode)
	if ctx == nil {
		ctx = context.Background()
	}
	body := contForwardRequest()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec, body
}

// contForwardEventPayloads 从下游 SSE 文本中取出所有 data 帧。
func contForwardEventPayloads(body string) []string {
	payloads := make([]string, 0)
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				payloads = append(payloads, strings.TrimPrefix(line, "data: "))
			}
		}
	}
	return payloads
}

func contForwardDeltas(body string) string {
	var b strings.Builder
	for _, payload := range contForwardEventPayloads(body) {
		if gjson.Get(payload, "type").String() == "response.output_text.delta" {
			b.WriteString(gjson.Get(payload, "delta").String())
		}
	}
	return b.String()
}

func contForwardTerminal(t *testing.T, body string) gjson.Result {
	t.Helper()
	terminal := gjson.Result{}
	for _, payload := range contForwardEventPayloads(body) {
		if gjson.Get(payload, "type").String() == "response.completed" {
			terminal = gjson.Parse(payload)
		}
	}
	require.True(t, terminal.Exists(), "下游必须收到 response.completed，实际 body: %s", body)
	return terminal
}

func contForwardSequenceNumbers(t *testing.T, body string) []int64 {
	t.Helper()
	numbers := make([]int64, 0, 8)
	for _, payload := range contForwardEventPayloads(body) {
		seq := gjson.Get(payload, "sequence_number")
		if seq.Exists() {
			numbers = append(numbers, seq.Int())
		}
	}
	return numbers
}

func requireContForwardSequencesAscending(t *testing.T, body string) {
	t.Helper()
	numbers := contForwardSequenceNumbers(t, body)
	require.NotEmpty(t, numbers)
	for i := 1; i < len(numbers); i++ {
		require.Greater(t, numbers[i], numbers[i-1], "sequence_number 必须严格递增: %v", numbers)
	}
}

func TestOpenAIStream503AutoContinueForward_SplicesContinuation(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardContinuationDelta,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)
	c.Request.Header.Set("x-codex-turn-state", "turn-state-1")

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	// 客户端：一条连续的响应流。
	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.created"), "只能有一个 response.created")
	require.Equal(t, 1, strings.Count(downstream, "event: response.completed"), "只能有一个 response.completed")
	require.Zero(t, strings.Count(downstream, "event: error"))
	require.Zero(t, strings.Count(downstream, "event: response.failed"))
	require.Equal(t, 1, strings.Count(contForwardDeltas(downstream), "Hello wor"), "\"Hello wor\" 在 delta 流中只能出现一次")
	require.Equal(t, "Hello world!", contForwardDeltas(downstream))
	requireContForwardSequencesAscending(t, downstream)

	terminal := contForwardTerminal(t, downstream)
	require.Equal(t, "Hello world!", terminal.Get("response.output.0.content.0.text").String())
	// 05 把续跑终态 usage 重写为 prior(10/5)+local(2/4)=12/9，即裁定 #1 的 Σ。
	require.Equal(t, int64(12), terminal.Get("response.usage.input_tokens").Int())
	require.Equal(t, int64(9), terminal.Get("response.usage.output_tokens").Int())

	// 上游：同一账号、恰好两次请求；第 2 次带 assistant partial 与续写指令。
	require.Len(t, upstream.requests, 2)
	require.Equal(t, upstream.requests[0].URL.String(), upstream.requests[1].URL.String())
	second := upstream.bodies[1]
	require.Contains(t, string(second), "Hello wor")
	require.Contains(t, string(second), openAIStream503AutoContinueInstruction)
	require.Len(t, gjson.GetBytes(second, "input").Array(), len(gjson.GetBytes(upstream.bodies[0], "input").Array())+2)
	require.Equal(t, gjson.GetBytes(upstream.bodies[0], "model").Raw, gjson.GetBytes(second, "model").Raw)
	require.Equal(t, gjson.GetBytes(upstream.bodies[0], "tools").Raw, gjson.GetBytes(second, "tools").Raw)
	require.Empty(t, upstream.requests[1].Header.Get("x-codex-turn-state"), "续跑 attempt 不得沿入站 turn-state")

	// 结果：usage = Σ（恰好计一次），身份字段来自第 1 次 attempt。
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
	require.Equal(t, contForwardFirstRequestID, result.RequestID)
	require.Equal(t, "resp_1", result.ResponseID)
	require.NotNil(t, result.FirstTokenMs)
	require.Greater(t, result.Duration, time.Duration(0))

	// 成功日志。
	require.True(t, logSink.ContainsMessage("gateway.openai_stream_503_auto_continue_ok"))
	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_ok")
	require.NotNil(t, fields)
	require.EqualValues(t, 1, fields["continuation_count"])
	require.EqualValues(t, 12, fields["agg_input_tokens"])
	require.EqualValues(t, 9, fields["agg_output_tokens"])
	require.Contains(t, fields, "extra_elapsed_ms")
	require.Contains(t, fields, "dropped_reasoning_events")
	require.Contains(t, fields, "overlap_trimmed_bytes")
}

func TestOpenAIStream503AutoContinueForward_BudgetExhausted(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	failing := func() *http.Response {
		return contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		)
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{failing(), failing(), failing()}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.Error(t, err)
	require.NotNil(t, result, "放弃续跑时仍须返回已聚合的 result（一次 Forward 一条用量）")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "续跑路径不得退化为换号 failover")

	require.Len(t, upstream.requests, 3, "1 次原始 + maxContinuations(2) 次续跑，第 4 次不得发生")

	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.failed"))
	require.Equal(t, 1, strings.Count(downstream, `"code":"server_error"`))
	require.NotContains(t, downstream, "server_is_overloaded")
	require.Contains(t, downstream, "Our servers are currently overloaded")

	// 失败尝试的 usage 也计入（裁定 #1）：三次失败帧各 10/5，Σ = 30/15。
	require.Equal(t, 30, result.Usage.InputTokens)
	require.Equal(t, 15, result.Usage.OutputTokens)
	require.True(t, logSink.ContainsMessage("gateway.openai_stream_503_auto_continue_exhausted"))
}

// B1 回归：续跑 attempt 在“上游还没出流”就返回 HTTP 503 时，
// 不得退化为换号 failover、不得把 JSON 错误体追加到已提交的 SSE、
// 不得登记账号健康副作用；客户端只收到恰好一个 response.failed，
// usage 仍须取含此前尝试的 Σ（一次 Forward 一条用量）。
func TestOpenAIStream503AutoContinueForward_ContinuationHTTP503KeepsUsage(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardJSONErrorResponse(contForwardSecondRequestID, http.StatusServiceUnavailable,
			`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`,
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	account := contForwardAccount(true)
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, account, body)
	require.Error(t, err)
	require.NotNil(t, result, "续跑前置失败仍须返回已聚合的 result（一次 Forward 一条用量）")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "续跑路径不得退化为换号 failover")
	require.Len(t, upstream.requests, 2)

	// usage = 第 1 次（失败尝试）10/5；第 2 次尚未出流，无 local usage。
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Equal(t, contForwardFirstRequestID, result.RequestID)
	require.Equal(t, "resp_1", result.ResponseID)

	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.failed"), "客户端必须收到恰好一个 response.failed")
	require.Zero(t, strings.Count(downstream, "event: error"))
	require.Contains(t, downstream, "Our servers are currently overloaded")
	require.NotContains(t, downstream, "server_is_overloaded", "降载码须被改写为客户端可重试的 server_error")
	// 已提交的 SSE 流里不得混入裸 JSON 错误体（每条 data 帧都必须带 type）。
	for _, payload := range contForwardEventPayloads(downstream) {
		require.NotEmpty(t, gjson.Get(payload, "type").String(), "不得向已提交的 SSE 流追加裸 JSON: %s", payload)
	}

	// 不登记 failover/健康副作用：账号状态保持不变，ops 里不得出现 failover 事件。
	require.Equal(t, StatusActive, account.Status)
	require.True(t, account.Schedulable)
	require.Nil(t, account.TempUnschedulableUntil)
	require.Empty(t, account.ErrorMessage)
	if raw, ok := c.Get(OpsUpstreamErrorsKey); ok {
		events, _ := raw.([]*OpsUpstreamErrorEvent)
		for _, ev := range events {
			require.NotEqual(t, "failover", ev.Kind, "续跑 attempt 出流前失败不得登记 failover 副作用")
		}
	}
}

// M1 回归：两次续跑都走到终态时，客户端看到三段文本无缝拼接、恰好一个
// response.completed，sequence_number 跨三段严格递增，usage = 三段 Σ。
func TestOpenAIStream503AutoContinueForward_TwoContinuationsSpliceThreeSegments(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardSegment1Delta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardSegment2Delta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardThirdRequestID,
			contForwardContinuationCreated,
			contForwardSegment3Added,
			contForwardSegment3Delta,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 3, "1 次原始 + 2 次续跑")

	// 第 3 次请求体必须基于累计 partial "Hello wor" 构造。
	require.Contains(t, string(upstream.bodies[2]), "Hello wor")

	downstream := rec.Body.String()
	require.Equal(t, "Hello world!", contForwardDeltas(downstream), "三段文本拼接后不得重复")
	require.Equal(t, 1, strings.Count(downstream, "event: response.created"))
	require.Equal(t, 1, strings.Count(downstream, "event: response.completed"))
	require.Zero(t, strings.Count(downstream, "event: response.failed"))
	require.Zero(t, strings.Count(downstream, "event: error"))
	requireContForwardSequencesAscending(t, downstream)

	terminal := contForwardTerminal(t, downstream)
	require.Equal(t, "Hello world!", terminal.Get("response.output.0.content.0.text").String())
	// 三段 usage：10/5 + 10/5 + 2/4 = 22/14。
	require.Equal(t, int64(22), terminal.Get("response.usage.input_tokens").Int())
	require.Equal(t, int64(14), terminal.Get("response.usage.output_tokens").Int())
	require.Equal(t, 22, result.Usage.InputTokens)
	require.Equal(t, 14, result.Usage.OutputTokens)
	require.Equal(t, contForwardFirstRequestID, result.RequestID)
	require.Equal(t, "resp_1", result.ResponseID)
}

func TestOpenAIStream503AutoContinueForward_ToolCallInProgressNotContinued(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardFunctionAdded,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	_, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "工具调用参数流进行中不得续跑")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
}

func TestOpenAIStream503AutoContinueForward_OverlapTrimmed(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardContinuationFullText,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.Equal(t, "Hello world!", contForwardDeltas(downstream), "复述的前缀必须被裁剪")
	require.Equal(t, 1, strings.Count(contForwardDeltas(downstream), "Hello wor"))
	require.Equal(t, int64(12), contForwardTerminal(t, downstream).Get("response.usage.input_tokens").Int())
}

func TestOpenAIStream503AutoContinueForward_TerminalOutputMerged(t *testing.T) {
	mergedOutput := `[{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"ld!"}]},{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{}"}]`
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardContinuationDelta,
			contForwardFunctionAdded,
			contForwardFunctionDone,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, mergedOutput),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	terminal := contForwardTerminal(t, rec.Body.String())
	output := terminal.Get("response.output").Array()
	require.Len(t, output, 2)
	require.Equal(t, "message", output[0].Get("type").String())
	require.Equal(t, "Hello world!", output[0].Get("content.0.text").String())
	require.Equal(t, "function_call", output[1].Get("type").String())
	require.Equal(t, "fc_1", output[1].Get("id").String())

	// 续跑新增 item 的 output_index 必须接在第 1 次已打开的 message 之后。
	var functionCallIndex int64 = -1
	for _, payload := range contForwardEventPayloads(rec.Body.String()) {
		if gjson.Get(payload, "type").String() == "response.output_item.added" &&
			gjson.Get(payload, "item.type").String() == "function_call" {
			functionCallIndex = gjson.Get(payload, "output_index").Int()
		}
	}
	require.Equal(t, int64(1), functionCallIndex)
}

func TestOpenAIStream503AutoContinueForward_DropsReasoningKeepsUsage(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardReasoningAdded,
			contForwardReasoningDelta,
			contForwardContinuationDelta,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.NotContains(t, downstream, `"type":"reasoning`)
	require.NotContains(t, downstream, "thinking")
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)

	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_ok")
	require.NotNil(t, fields)
	dropped, ok := fields["dropped_reasoning_events"].(int64)
	require.True(t, ok)
	require.GreaterOrEqual(t, dropped, int64(1))
}

func TestOpenAIStream503AutoContinueForward_BackoffCancelable(t *testing.T) {
	consumed := make(chan struct{})
	notified := &contForwardNotifyBody{
		reader: strings.NewReader(contForwardSSE(
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		)),
		read: consumed,
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardResponse(contForwardFirstRequestID, notified),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardContinuationDelta,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-consumed
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	c, _, body := contForwardContext(ctx)
	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Len(t, upstream.requests, 1, "退避期间客户端取消后不得再发上游请求")
}

func TestOpenAIStream503AutoContinueForward_TerminalUsageMissing(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardContinuationDelta,
			contForwardCompletedFrame("", contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Usage.InputTokens == 0 && result.Usage.OutputTokens == 0, "结果不得被清零")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)

	terminal := contForwardTerminal(t, rec.Body.String())
	require.GreaterOrEqual(t, terminal.Get("response.usage.input_tokens").Int(), int64(10))
	require.GreaterOrEqual(t, terminal.Get("response.usage.output_tokens").Int(), int64(5))

	require.True(t, logSink.ContainsMessage("gateway.openai_stream_503_auto_continue_usage_missing_on_continuation_terminal"))
}

func TestOpenAIStream503AutoContinueForward_SwitchOffKeepsLegacyBehaviour(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardStream(contForwardSecondRequestID,
			contForwardContinuationCreated,
			contForwardContinuationAdded,
			contForwardContinuationDelta,
			contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	_, err := contForwardRun(t, svc, c, contForwardAccount(false), body)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "开关关闭时不得续跑")

	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.failed"))
	require.Equal(t, 1, strings.Count(downstream, `"code":"server_error"`))
	require.NotContains(t, downstream, "server_is_overloaded")
}

// R8：续跑 attempt 不建 first-output header guard。用 1s 超时 + 第 2 次响应
// 延迟 1s 交付来区分：若 guard 存在，会以第 1 次的 startTime 判超时。
func TestOpenAIStream503AutoContinueForward_ContinuationSkipsFirstOutputGuard(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardMessageAdded,
			contForwardHelloDelta,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
		contForwardResponse(contForwardSecondRequestID, &contForwardDelayedBody{
			payload: []byte(contForwardSSE(
				contForwardContinuationCreated,
				contForwardContinuationAdded,
				contForwardContinuationDelta,
				contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`, contForwardTextOutput()),
			)),
			delay: 1000 * time.Millisecond,
		}),
	}}
	svc := contTestService(config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1})
	svc.httpUpstream = upstream
	c, _, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err, "续跑 attempt 必须跳过 first-output guard（R8）")
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 2)
}

// contForwardNotifyBody 在首次成功读取时关闭 read channel，供退避取消用例
// 与流处理完成建立确定时序（触发点在读到降载帧后立即返回，不一定读到 EOF）。
type contForwardNotifyBody struct {
	reader *strings.Reader
	read   chan struct{}
	once   sync.Once
}

func (b *contForwardNotifyBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if n > 0 {
		b.once.Do(func() { close(b.read) })
	}
	return n, err
}

func (b *contForwardNotifyBody) Close() error { return nil }

type contForwardDelayedBody struct {
	payload []byte
	delay   time.Duration
	sent    bool
}

func (b *contForwardDelayedBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		time.Sleep(b.delay)
	}
	if len(b.payload) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.payload)
	b.payload = b.payload[n:]
	return n, nil
}

func (b *contForwardDelayedBody) Close() error { return nil }

func TestOpenAIStream503AutoContinueForward_ZeroPartialContinuation(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardZeroPartialFailure(),
		contForwardZeroPartialSuccess(contForwardSecondRequestID),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.created"), "只能有一个 response.created")
	require.Equal(t, 1, strings.Count(downstream, "event: response.completed"), "只能有一个 response.completed")
	require.Zero(t, strings.Count(downstream, "event: error"))
	require.Zero(t, strings.Count(downstream, "event: response.failed"))
	require.NotContains(t, downstream, "rs_2", "attempt-2 的 reasoning 事件必须不可见")
	require.Equal(t, 1, strings.Count(downstream, `"id":"rs_1"`), "attempt-1 的 reasoning 只出现一次")
	require.Equal(t, "answer", contForwardDeltas(downstream), "最终文本必须等于 attempt-2 文本且无重复")
	requireContForwardSequencesAscending(t, downstream)

	var messageIndex int64 = -1
	for _, payload := range contForwardEventPayloads(downstream) {
		if gjson.Get(payload, "type").String() == "response.output_item.added" &&
			gjson.Get(payload, "item.type").String() == "message" {
			messageIndex = gjson.Get(payload, "output_index").Int()
			require.Equal(t, "msg_2", gjson.Get(payload, "item.id").String(),
				"零模式必须保留 attempt-2 自己的 item id")
		}
	}
	require.Equal(t, int64(1), messageIndex, "message 的 output_index 从 snapshot.nextOutputIndex 起")

	terminal := contForwardTerminal(t, downstream)
	require.Equal(t, "answer", terminal.Get("response.output.0.content.0.text").String())
	require.Equal(t, int64(12), terminal.Get("response.usage.input_tokens").Int())
	require.Equal(t, int64(9), terminal.Get("response.usage.output_tokens").Int())

	require.Len(t, upstream.requests, 2)
	require.True(t, bytes.Equal(upstream.bodies[0], upstream.bodies[1]),
		"零 partial 续跑请求体必须与第 1 次逐字节相等")
	require.NotContains(t, string(upstream.bodies[1]), "assistant")
	require.NotContains(t, string(upstream.bodies[1]), openAIStream503AutoContinueInstruction)

	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
	require.Equal(t, contForwardFirstRequestID, result.RequestID)
	require.Equal(t, "resp_1", result.ResponseID)

	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue")
	require.NotNil(t, fields)
	require.EqualValues(t, 0, fields["partial_text_bytes"])
	require.EqualValues(t, 1, fields["continuation_index"])
}

func TestOpenAIStream503AutoContinueForward_ZeroPartialBudgetExhausted(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardZeroPartialFailure(),
		contForwardZeroPartialFailure(),
		contForwardZeroPartialFailure(),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 3, "1 次原始 + maxContinuations(2) 次续跑，第 4 次不得发生")

	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.failed"), "客户端恰好收到一个 response.failed")
	require.Equal(t, 1, strings.Count(downstream, `"code":"server_error"`))
	require.NotContains(t, downstream, "server_is_overloaded")
	require.Contains(t, downstream, "Our servers are currently overloaded", "上游原文必须保留")

	require.Equal(t, 30, result.Usage.InputTokens)
	require.Equal(t, 15, result.Usage.OutputTokens)
}

func TestOpenAIStream503AutoContinueForward_ZeroPartialFunctionCallInProgressNotContinued(t *testing.T) {
	logSink, restore := captureStructuredLog(t)
	defer restore()

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardStream(contForwardFirstRequestID,
			contForwardCreatedFrame,
			contForwardZeroAttemptOneReasoningAdded,
			contForwardZeroAttemptOneReasoningDelta,
			contForwardFunctionAdded,
			contForwardOverloaded,
			contForwardFailedFrame,
		),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	_, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "进行中的 function_call 不得续跑")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))

	fields := continuationLogFields(logSink, "gateway.openai_stream_503_auto_continue_exhausted")
	require.NotNil(t, fields)
	require.Equal(t, "ineligible_output", fields["reason"])
}

func TestOpenAIStream503AutoContinueForward_ZeroPartialKeepaliveDuringSilentContinuation(t *testing.T) {
	// 续跑 attempt 在等待上游首帧时是静默的；现有 keepalive 必须覆盖这段窗口。
	// 延迟取 2.2s（> 2×keepalive）以确定性跨越一次 tick：1.5s 可能在
	// `time.Since(lastDownstreamWriteAt) < keepaliveInterval` 的边界上被跳过。
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardZeroPartialFailure(),
		contForwardResponse(contForwardSecondRequestID, &contForwardDelayedBody{
			payload: []byte(contForwardSSE(
				contForwardContinuationCreated,
				contForwardZeroAttemptTwoReasoningAdded,
				contForwardZeroAttemptTwoReasoningDelta,
				contForwardZeroMessageAdded,
				contForwardZeroAnswerDelta,
				contForwardCompletedFrame(`{"input_tokens":2,"output_tokens":4}`,
					`[{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]`),
			)),
			delay: 2200 * time.Millisecond,
		}),
	}}
	svc := contTestService(config.GatewayConfig{StreamKeepaliveInterval: 1})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	result, err := contForwardRun(t, svc, c, contForwardAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.Contains(t, downstream, ":\n\n", "续跑静默期必须收到 SSE 注释 keepalive")
	require.Equal(t, 1, strings.Count(downstream, "event: response.completed"))
	require.Equal(t, "answer", contForwardDeltas(downstream))
}

func TestOpenAIStream503AutoContinueForward_ZeroPartialSwitchOffKeepsLegacy(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		contForwardZeroPartialFailure(),
		contForwardZeroPartialSuccess(contForwardSecondRequestID),
	}}
	svc := contTestService(config.GatewayConfig{})
	svc.httpUpstream = upstream
	c, rec, body := contForwardContext(nil)

	_, err := contForwardRun(t, svc, c, contForwardAccount(false), body)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "开关关闭时零 partial 请求只发 1 次上游")

	downstream := rec.Body.String()
	require.Equal(t, 1, strings.Count(downstream, "event: response.failed"))
	require.Equal(t, 1, strings.Count(downstream, `"code":"server_error"`))
}
