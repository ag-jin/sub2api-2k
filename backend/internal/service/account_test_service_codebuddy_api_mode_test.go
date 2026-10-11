//go:build unit

package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CodeBuddy「API 线路」账号的测连通：GET {base_url}/models（Bearer=api_key），
// HTTP 200 即成功。腾讯 auth JSON 账号的 /v2/chat/completions 连通口径见
// account_test_service_codebuddy_test.go（回归基线，不随本改动变化）。

func TestAccountTestService_CodeBuddyAPIMode_ModelsProbe(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotAccept string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"deepseek-v4.1-flash","object":"model"}]}`)
	}))
	defer srv.Close()

	account := &Account{
		ID:       801,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-relay",
			"base_url": srv.URL + "/v1",
		},
	}

	svc := newCodeBuddyTestService(account)
	c, rec := newTestContext()
	require.NoError(t, svc.TestAccountConnection(c, account.ID, "", "hi", ""))

	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/v1/models", gotPath)
	assert.Equal(t, "Bearer sk-relay", gotAuth)
	assert.Equal(t, "application/json", gotAccept)

	success := false
	for _, ev := range parseCodeBuddyTestEvents(rec) {
		if ev.Type == "test_complete" {
			success = ev.Success
		}
	}
	assert.True(t, success, "GET /models 返回 200 应以 test_complete success 结束")
}

func TestAccountTestService_CodeBuddyAPIMode_ModelsProbeNon200Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	account := &Account{
		ID:       802,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-relay",
			"base_url": srv.URL + "/v1",
		},
	}

	svc := newCodeBuddyTestService(account)
	c, rec := newTestContext()
	_ = svc.TestAccountConnection(c, account.ID, "", "hi", "")

	out := rec.Body.String()
	assert.Contains(t, out, "returned 401", "应透出上游状态码")
	assert.Contains(t, out, "invalid api key", "应透出上游错误信息")
}

// ── 停顿守卫（与 testCodeBuddyConnection 同款，见
// account_test_service_codebuddy_test.go 的断流修复回归 F）─────────────────
//
// API 线路探测走 GET /models（OpenAI profile：responseHeaderTimeout=0）。中转
// 「接住连接但不吐数据」时，若没有停顿守卫，面板会无限干等。下面两条分别锁定
// 两个停顿阶段：响应头已发但永久静默（读取阶段）、以及一个字节都不回（等响应头
// 阶段）——后者正是生产事故的报错形态。

// apiModeTestAccount 构造指向 fake 上游的 API 线路账号。
func apiModeTestAccount(id int64, baseURL string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-relay",
			"base_url": baseURL,
		},
	}
}

// runCodeBuddyTestBounded 在后台跑连通测试，断言在 30s 内返回（上游静默时
// 守卫未生效则无限挂起——这正是用户看到「点了没反应」的形态）。
func runCodeBuddyTestBounded(t *testing.T, svc *AccountTestService, c *gin.Context, accountID int64) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- svc.TestAccountConnection(c, accountID, "", "hi", "") }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("API 线路测连通在上游静默时无限挂起——面板会一直等到用户放弃")
	}
}

func TestAccountTestService_CodeBuddyAPIMode_StalledUpstreamIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 响应头已发出，此后永久静默：不产字节、不结束。
		<-release
	}))
	defer srv.Close()
	defer close(release)

	account := apiModeTestAccount(803, srv.URL+"/v1")
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.StreamDataIntervalTimeout = 1 // 1s 空闲即判定停顿（绕过校验边界，直测行为）
	svc := newCodeBuddyTestServiceWithConfig(account, cfg)

	c, rec := newTestContext()
	runCodeBuddyTestBounded(t, svc, c, account.ID)

	out := rec.Body.String()
	require.Contains(t, out, `"type":"error"`,
		"必须以可见 error 事件收尾，而不是静默结束；实际输出: %s", out)
	require.Contains(t, out, "上游",
		"错误文案应说明是上游未产生数据，而不是笼统 context canceled；实际输出: %s", out)
	assert.NotContains(t, out, `"success":true`,
		"上游未产出任何数据时不得误报成功；实际输出: %s", out)
}

func TestAccountTestService_CodeBuddyAPIMode_HeaderStallIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 接住连接但一个字节都不回：卡在「等响应头」阶段（生产事故形态）。
		<-release
	}))
	defer srv.Close()
	defer close(release)

	account := apiModeTestAccount(804, srv.URL+"/v1")
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.StreamDataIntervalTimeout = 1
	svc := newCodeBuddyTestServiceWithConfig(account, cfg)

	c, rec := newTestContext()
	runCodeBuddyTestBounded(t, svc, c, account.ID)

	out := rec.Body.String()
	require.Contains(t, out, `"type":"error"`,
		"必须以可见 error 事件收尾；实际输出: %s", out)
	assert.Contains(t, out, "未返回响应头",
		"错误文案应归因为上游未返回响应头；实际输出: %s", out)
}
