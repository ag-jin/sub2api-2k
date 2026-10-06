//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 智谱签名自愈状态机（design M3.1(a) / 票 23）的表驱动用例：
//
//	ok ──响应含 VERIFY_*──▶ Signer.Invalidate(apiKeyID) → 重新握手 → re-sign
//	                     → 重放同一请求体（恰好一次）
//	  2xx → ok ｜ 再次 VERIFY_*/重放失败 → 交回既有错误链（fail 策略接缝属票 24）
//
// 与票 22 的签名注入用例共用 zhipuSignTestAccount / zhipuSignTestConfig /
// zhipuSignTestAPIKey / zhipuSignHeaderNames 等脚手架；本文件只新增「可编程
// 上游 + 可编程 Signer」这两个测试替身，断言全部落在公开路径与接缝上：
// forwardAsRawChatCompletions（CC 直转）与 ForwardAsAnthropic（anthropic 原生直通）。

// zhipuSelfHealSignerStub 是自愈用例的 Signer 替身：Sign 每次调用写入带序号的头值
// （用于断言重放确实重新签名、而不是复用首发头），Invalidate 记录被作废的 apiKeyID。
type zhipuSelfHealSignerStub struct {
	mu          sync.Mutex
	signCalls   int
	invalidated []string
	// signErr 非 nil 时 Sign 一律失败（模拟握手/退避窗口内的签名失败 → fail-open）。
	signErr error
}

func (s *zhipuSelfHealSignerStub) Sign(_ context.Context, apiKey, sessionID string, h http.Header) error {
	s.mu.Lock()
	s.signCalls++
	call := s.signCalls
	err := s.signErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, name := range zhipuSignHeaderNames() {
		h.Set(name, fmt.Sprintf("stub-%s-%d", name, call))
	}
	return nil
}

func (s *zhipuSelfHealSignerStub) Invalidate(apiKeyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, apiKeyID)
}

func (s *zhipuSelfHealSignerStub) signCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signCalls
}

func (s *zhipuSelfHealSignerStub) invalidatedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.invalidated...)
}

// zhipuSelfHealUpstream 是可编程上游：按「第几次发送」返回响应或传输错误，
// 同时记录每个出站请求与请求体（用于断言重放次数、重放体一致与新签名）。
type zhipuSelfHealUpstream struct {
	respond func(call int, req *http.Request) (*http.Response, error)

	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (u *zhipuSelfHealUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body []byte
	if req != nil && req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	u.mu.Lock()
	u.requests = append(u.requests, req)
	u.bodies = append(u.bodies, body)
	call := len(u.requests)
	u.mu.Unlock()
	if u.respond == nil {
		return nil, fmt.Errorf("zhipu self-heal test: no response programmed")
	}
	return u.respond(call, req)
}

func (u *zhipuSelfHealUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *zhipuSelfHealUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *zhipuSelfHealUpstream) requestAt(i int) *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[i]
}

func (u *zhipuSelfHealUpstream) bodyAt(i int) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[i]
}

// zhipuSelfHealErrorResponse 构造携带协议错误体的上游 4xx 响应。
func zhipuSelfHealErrorResponse(t *testing.T, status int, body string) *http.Response {
	t.Helper()
	require.True(t, json.Valid([]byte(body)), "错误体必须是合法 JSON: %s", body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// zhipuSelfHealJSONResponse 构造上游 2xx 响应。
func zhipuSelfHealJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

const zhipuSelfHealCCSuccessBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"glm-4.7",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`

const zhipuSelfHealAnthropicSuccessBody = `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7",` +
	`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":9,"output_tokens":2}}`

// zhipuSelfHealTestContext 构造 /v1/chat/completions 或 /v1/messages 入站上下文，
// 并把 recorder 暴露给调用方（断言最终写给客户端的响应）。
func zhipuSelfHealTestContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

// TestForwardAsRawChatCompletionsSelfHealsVerifySignatureInvalid 是自愈状态机的
// 主路径（CC 直转）：首发响应带 VERIFY_SIGNATURE_INVALID → 作废私钥 → 重放恰好一次
// → 二次响应 2xx 原样返回客户端。
func TestForwardAsRawChatCompletionsSelfHealsVerifySignatureInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	signer := &zhipuSelfHealSignerStub{}
	upstream := &zhipuSelfHealUpstream{}
	upstream.respond = func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized,
				`{"error":{"code":40005,"message":"signature rejected","reason":"VERIFY_SIGNATURE_INVALID"}}`), nil
		}
		return zhipuSelfHealJSONResponse(http.StatusOK, zhipuSelfHealCCSuccessBody), nil
	}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: signer}
	account := zhipuSignTestAccount(t, map[string]any{})
	c, recorder := zhipuSelfHealTestContext("/v1/chat/completions", body)

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount(), "VERIFY_* 后必须恰好重放一次（上游请求数 = 2）")
	require.Equal(t, []string{"1234567890abcdef"}, signer.invalidatedIDs(), "必须作废该 apiKeyID 的缓存私钥")
	require.Equal(t, 2, signer.signCallCount(), "重放必须重新签名")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "pong", "重放成功的响应必须原样返回客户端")

	first, replay := upstream.requestAt(0), upstream.requestAt(1)
	require.Equal(t, "stub-"+zcodesign.HeaderClientNonce+"-2", replay.Header.Get(zcodesign.HeaderClientNonce),
		"重放必须带新 nonce")
	require.Equal(t, "stub-"+zcodesign.HeaderClientSig+"-2", replay.Header.Get(zcodesign.HeaderClientSig),
		"重放必须带新签名")
	require.Equal(t, "stub-"+zcodesign.HeaderClientTs+"-2", replay.Header.Get(zcodesign.HeaderClientTs),
		"重放必须带新 ts")
	require.NotEqual(t, first.Header.Get(zcodesign.HeaderClientSig), replay.Header.Get(zcodesign.HeaderClientSig))
	require.Equal(t, string(upstream.bodyAt(0)), string(upstream.bodyAt(1)), "重放必须使用同一请求体")
	require.Contains(t, string(upstream.bodyAt(1)), `"content":"hello"`)
}

// TestForwardAsRawChatCompletionsDoesNotReplayTwice 覆盖防循环：重放再次命中
// VERIFY_* 时上游请求数必须停在 2，绝不出现第三次发送。
func TestForwardAsRawChatCompletionsDoesNotReplayTwice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	verifyBody := `{"error":{"code":40005,"message":"signature rejected","reason":"VERIFY_SIGNATURE_INVALID"}}`
	signer := &zhipuSelfHealSignerStub{}
	upstream := &zhipuSelfHealUpstream{}
	upstream.respond = func(_ int, _ *http.Request) (*http.Response, error) {
		return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized, verifyBody), nil
	}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: signer}
	account := zhipuSignTestAccount(t, map[string]any{})
	c, recorder := zhipuSelfHealTestContext("/v1/chat/completions", body)

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")

	require.Error(t, err, "重放仍失败必须交回既有错误链（fail 策略接入点，票 24）")
	require.Nil(t, result)
	require.Equal(t, 2, upstream.callCount(), "同一请求最多自愈 1 次：不得出现第三次发送")
	require.Equal(t, []string{"1234567890abcdef"}, signer.invalidatedIDs(), "重放失败也不得重复作废")
	require.NotContains(t, recorder.Body.String(), "pong", "重放失败绝不能被当作成功响应下发")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "重放仍失败必须交给既有错误链（failover/错误响应）")
}

// TestForwardAsRawChatCompletionsReplayTransportErrorFailsOver 覆盖重放阶段网络错误：
// 与首发传输失败同处置（可 failover 错误交调度器），且不多发第三次请求。
func TestForwardAsRawChatCompletionsReplayTransportErrorFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	signer := &zhipuSelfHealSignerStub{}
	upstream := &zhipuSelfHealUpstream{}
	upstream.respond = func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized,
				`{"error":{"message":"VERIFY_SIGNATURE_INVALID"}}`), nil
		}
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: signer}
	account := zhipuSignTestAccount(t, map[string]any{})
	c, _ := zhipuSelfHealTestContext("/v1/chat/completions", body)

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 2, upstream.callCount(), "重放网络失败不得再试")
	require.Equal(t, []string{"1234567890abcdef"}, signer.invalidatedIDs())
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "重放传输失败必须归一为既有 failover 语义")
}

// zhipuSelfHealNativeAccount 是 anthropic 原生直通路径的智谱托管账号。
func zhipuSelfHealNativeAccount(t *testing.T) *Account {
	t.Helper()
	return zhipuSignTestAccount(t, map[string]any{"api_protocol": APIProtocolAnthropic})
}

// TestForwardAsAnthropicSelfHealsVerifySignatureInvalid 是同一自愈状态机在
// anthropic 原生直通路径（/v1/messages → 供应商原生 Anthropic 端点）上的对照用例：
// 与 CC 路径共用一份编排（"同一编排，不得只测一条"）。
func TestForwardAsAnthropicSelfHealsVerifySignatureInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hello"}]}`)

	signer := &zhipuSelfHealSignerStub{}
	upstream := &zhipuSelfHealUpstream{}
	upstream.respond = func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized,
				`{"error":{"message":"signature rejected","reason":"VERIFY_SIGNATURE_INVALID"}}`), nil
		}
		return zhipuSelfHealJSONResponse(http.StatusOK, zhipuSelfHealAnthropicSuccessBody), nil
	}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: signer}
	account := zhipuSelfHealNativeAccount(t)
	c, recorder := zhipuSelfHealTestContext("/v1/messages", body)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount(), "anthropic 原生路径同样必须恰好重放一次")
	require.Equal(t, []string{"1234567890abcdef"}, signer.invalidatedIDs())
	require.Contains(t, recorder.Body.String(), "pong", "重放成功的响应必须原样返回客户端")
	require.Equal(t, "http://upstream.example/v1/messages", upstream.requestAt(1).URL.String())
	require.Equal(t, "stub-"+zcodesign.HeaderClientNonce+"-2", upstream.requestAt(1).Header.Get(zcodesign.HeaderClientNonce),
		"重放必须带新签名")
}

// TestForwardAsAnthropicReplayFailureFailsOver 覆盖 anthropic 原生路径的重放失败分支：
// 上游请求数停在 2，错误照旧按路径既有语义上报。
func TestForwardAsAnthropicReplayFailureFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hello"}]}`)

	signer := &zhipuSelfHealSignerStub{}
	upstream := &zhipuSelfHealUpstream{}
	upstream.respond = func(_ int, _ *http.Request) (*http.Response, error) {
		return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized,
			`{"msg":"VERIFY_SIGNATURE_INVALID"}`), nil
	}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: signer}
	account := zhipuSelfHealNativeAccount(t)
	c, _ := zhipuSelfHealTestContext("/v1/messages", body)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 2, upstream.callCount())
	require.Equal(t, []string{"1234567890abcdef"}, signer.invalidatedIDs())
}
