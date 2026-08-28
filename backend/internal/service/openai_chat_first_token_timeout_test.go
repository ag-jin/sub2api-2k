//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// realRoundTripUpstream 用真实 http.Client 执行上游请求，使守卫的 context 取消
// 能够真实地中断响应体读取（与生产 net/http 传输行为一致）。
type realRoundTripUpstream struct {
	client *http.Client
}

func (u *realRoundTripUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *realRoundTripUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func newFirstTokenTimeoutTestService(t *testing.T, timeoutSeconds int, handler http.Handler) (*OpenAIGatewayService, *Account) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.ChatCompletionsFirstTokenTimeoutSeconds = timeoutSeconds
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &realRoundTripUpstream{client: &http.Client{}}}
	account := &Account{
		ID:          201,
		Name:        "first-token-test",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": ts.URL,
		},
	}
	return svc, account
}

func firstTokenTimeoutTestContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func firstTokenTimeoutStreamBody() []byte {
	return []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
}

// 上游收下请求（HTTP 200 + SSE 头）后挂起，模拟 OpenCode 排队不吐首包。
func firstTokenTimeoutHangingHandler(hang time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(hang):
		}
	}
}

func TestRawChatCompletionsFirstTokenTimeoutFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, account := newFirstTokenTimeoutTestService(t, 1, firstTokenTimeoutHangingHandler(30*time.Second))
	body := firstTokenTimeoutStreamBody()
	c, recorder := firstTokenTimeoutTestContext(body)

	start := time.Now()
	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "expected UpstreamFailoverError, got: %v", err)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite, "timeout must allow failover after keepalive-only writes")
	require.False(t, failoverErr.RetryableOnSameAccount)
	// 超时应在上游挂起 30s 之前触发（1s 限时，留出调度与传输余量）。
	require.Less(t, elapsed, 10*time.Second)
	// 未向客户端写出任何字节 → handler 可透明换号重放。
	require.Zero(t, recorder.Body.Len())
}

// 上游在响应头阶段挂起（连 HTTP 头都不回），守卫应在限时内取消请求并归类为
// 首 token 超时 failover，而不是通用传输错误。
func TestRawChatCompletionsFirstTokenTimeoutAtHeaderStage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, account := newFirstTokenTimeoutTestService(t, 1, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	body := firstTokenTimeoutStreamBody()
	c, _ := firstTokenTimeoutTestContext(body)

	start := time.Now()
	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "expected UpstreamFailoverError, got: %v", err)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Less(t, elapsed, 10*time.Second)
}

func TestRawChatCompletionsFirstTokenTimeoutDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// timeout=0 → 不启用守卫；上游挂起 1s 后自行断开（EOF）。
	// 180-card 语义：无数据块的空流按正常收尾处理（不判超时、不判截断失败），
	// 验证超时功能未启用时不会拦截流。
	svc, account := newFirstTokenTimeoutTestService(t, 0, firstTokenTimeoutHangingHandler(time.Second))
	body := firstTokenTimeoutStreamBody()
	c, _ := firstTokenTimeoutTestContext(body)

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
}

func writeFirstTokenChunk(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestRawChatCompletionsFirstTokenArrivesBeforeTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, account := newFirstTokenTimeoutTestService(t, 5, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFirstTokenChunk(w)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	body := firstTokenTimeoutStreamBody()
	c, _ := firstTokenTimeoutTestContext(body)

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.FirstTokenMs)
}

func TestRawChatCompletionsGuardStopsAfterFirstToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 首 token 立即到达，随后上游静默 1.5s（超过 1s 限时）再发 [DONE]：
	// 守卫在首数据块处停表，超时不得误杀健康的长间歇流。
	svc, account := newFirstTokenTimeoutTestService(t, 1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFirstTokenChunk(w)
		time.Sleep(1500 * time.Millisecond)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	body := firstTokenTimeoutStreamBody()
	c, _ := firstTokenTimeoutTestContext(body)

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.FirstTokenMs)
}

// 处理链路（Responses→CC 转换）的首 token 超时由 select 循环 timer 驱动，
// 这里直接验证守卫与错误构造器的关键语义：nil 安全 + SafeToFailoverAfterWrite。
func TestOpenAIChatFirstTokenTimeoutErrorShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.ChatCompletionsFirstTokenTimeoutSeconds = 60
	svc := &OpenAIGatewayService{cfg: cfg}

	var nilGuard *openAIFirstOutputHeaderGuard
	require.False(t, nilGuard.Fired())
	require.False(t, nilGuard.stopHeaderWait())

	body := firstTokenTimeoutStreamBody()
	c, _ := firstTokenTimeoutTestContext(body)
	account := rawChatCompletionsTestAccount()
	err := svc.newOpenAIChatFirstTokenTimeoutError(context.Background(), c, account, "glm-5.2", "req-1", 60*time.Second)
	require.Equal(t, http.StatusGatewayTimeout, err.StatusCode)
	require.True(t, err.SafeToFailoverAfterWrite)
	require.True(t, err.ShouldRetryNextAccount())
	require.Contains(t, string(err.ResponseBody), "first_token_timeout")
}
