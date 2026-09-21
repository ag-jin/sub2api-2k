//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// directHTTPUpstream 把请求原样发给真实 httptest 上游（测试专用 HTTPUpstream）。
type directHTTPUpstream struct {
	client *http.Client
}

func newDirectHTTPUpstream() *directHTTPUpstream {
	return &directHTTPUpstream{client: &http.Client{}}
}

func (u *directHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *directHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.client.Do(req)
}

// codeBuddyAccountTestRepo 返回固定的 codebuddy 账号供连通测试取用。
type codeBuddyAccountTestRepo struct {
	openAIAccountTestRepo
	account *Account
}

func (r *codeBuddyAccountTestRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

// newCodeBuddyTestService 构造指向 fake 上游的账号连通测试服务。
func newCodeBuddyTestService(account *Account) *AccountTestService {
	repo := &codeBuddyAccountTestRepo{account: account}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return NewAccountTestService(repo, nil, nil, nil, nil, newDirectHTTPUpstream(), cfg, nil)
}

// newCodeBuddyTestServiceWithConfig 与 newCodeBuddyTestService 同构，但允许注入
// 自定义 config（用于把 stream_data_interval_timeout 调小以便快速判定停顿）。
func newCodeBuddyTestServiceWithConfig(account *Account, cfg *config.Config) *AccountTestService {
	repo := &codeBuddyAccountTestRepo{account: account}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return NewAccountTestService(repo, nil, nil, nil, nil, newDirectHTTPUpstream(), cfg, nil)
}

// TestAccountTestService_CodeBuddy_Connection 覆盖 codebuddy 账号连通测试：
// 端点 /v2/chat/completions、Bearer accessToken、专用身份头 + 自有 UA、
// stream=true 流式体，成功判定以 [DONE] 收尾。
func TestAccountTestService_CodeBuddy_Connection(t *testing.T) {
	var (
		gotPath        string
		gotAuth        string
		gotUA          string
		gotAccept      string
		gotXUser       string
		gotXEnterprise string
		gotXTenant     string
		gotXDomain     string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotXUser = r.Header.Get("X-User-Id")
		gotXEnterprise = r.Header.Get("X-Enterprise-Id")
		gotXTenant = r.Header.Get("X-Tenant-Id")
		gotXDomain = r.Header.Get("X-Domain")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"cb-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"cb-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	account := &Account{
		ID:       1,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url":      srv.URL,
			"access_token":  "cb-access-token",
			"uid":           "user-1",
			"enterprise_id": "ent-9",
			"domain":        "www.codebuddy.cn",
		},
	}

	svc := newCodeBuddyTestService(account)
	c, rec := newTestContext()
	err := svc.TestAccountConnection(c, account.ID, "", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, "/v2/chat/completions", gotPath)
	assert.Equal(t, "Bearer cb-access-token", gotAuth)
	assert.Equal(t, "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1", gotUA,
		"连通性测试与 chat 同形：官方三段式 UA")
	assert.Equal(t, "application/json, text/event-stream", gotAccept)
	assert.Equal(t, "user-1", gotXUser)
	assert.Equal(t, "ent-9", gotXEnterprise)
	assert.Equal(t, "ent-9", gotXTenant)
	assert.Equal(t, "www.codebuddy.cn", gotXDomain)
	assert.Equal(t, true, gjson.GetBytes(gotBody, "stream").Bool(), "强制流式")
	assert.Equal(t, true, gjson.GetBytes(gotBody, "stream_options.include_usage").Bool())

	events := parseCodeBuddyTestEvents(rec)
	success := false
	var content string
	for _, ev := range events {
		if ev.Type == "test_complete" {
			success = ev.Success
		}
		if ev.Type == "content" {
			content += ev.Text
		}
	}
	assert.True(t, success, "应以 test_complete success 结束")
	assert.Equal(t, "pong", content)
}

// TestAccountTestService_CodeBuddy_UpstreamError 覆盖非 200：错误信息带上游 code/msg。
func TestAccountTestService_CodeBuddy_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
	}))
	defer srv.Close()

	account := &Account{
		ID:          2,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": srv.URL, "access_token": "expired"},
	}

	svc := newCodeBuddyTestService(account)
	c, rec := newTestContext()
	_ = svc.TestAccountConnection(c, account.ID, "", "hi", "")

	out := rec.Body.String()
	assert.Contains(t, out, "returned 401", "应透出上游状态码")
	assert.Contains(t, out, "code=401", "应透出上游 code")
	assert.Contains(t, out, "unauthorized", "应透出上游 msg")
}

// TestAccountTestService_CodeBuddy_TypeGuard 非法 type 连通测试直接报错。
func TestAccountTestService_CodeBuddy_TypeGuard(t *testing.T) {
	account := &Account{ID: 3, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}
	svc := newCodeBuddyTestService(account)

	c, rec := newTestContext()
	_ = svc.TestAccountConnection(c, account.ID, "", "hi", "")
	assert.Contains(t, rec.Body.String(), "type=apikey")
}

func parseCodeBuddyTestEvents(rec *httptest.ResponseRecorder) []TestEvent {
	var events []TestEvent
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		events = append(events, TestEvent{Type: gjson.Get(payload, "type").String(), Text: gjson.Get(payload, "text").String(), Success: gjson.Get(payload, "success").Bool()})
	}
	return events
}

// ── 断流修复回归 F（面板「模型测试」路径的停顿兜底）──────────────────────────
//
// 生产实证：用户在面板点「模型测试」后**长时间无响应**，服务端三次记录
// `context canceled`（间隔恰好 60s）。根因：该路径把上游请求标为
// HTTPUpstreamProfileOpenAI，而 applyProfilePoolSettings 对该 profile 把
// responseHeaderTimeout 置 0（永不超时），生产 config.yaml 又没有 gateway 段
// → openai_response_header_timeout 取默认 0；且读取阶段原本没有任何空闲守卫。
//
// 本测试锁定：上游「接住连接但不吐数据」时，模型测试必须在空闲阈值附近结束
// 并给出可归因的错误，而不是无限等待到用户放弃。
func TestAccountTestService_CodeBuddy_StalledUpstreamIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 响应头已发出，此后永久静默：不产字节、不结束。
		<-release
	}))
	defer srv.Close()
	defer close(release)

	account := &Account{
		ID:       7,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url":     srv.URL,
			"access_token": "cb-access-token",
			"uid":          "user-1",
			"domain":       "www.codebuddy.cn",
		},
	}

	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.StreamDataIntervalTimeout = 1 // 1s 空闲即判定停顿（绕过校验边界，直测行为）
	svc := newCodeBuddyTestServiceWithConfig(account, cfg)

	c, rec := newTestContext()

	done := make(chan error, 1)
	go func() { done <- svc.TestAccountConnection(c, account.ID, "", "hi", "") }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("模型测试在上游静默时无限挂起——这正是用户看到「点了没反应」的形态")
	}

	out := rec.Body.String()
	require.Contains(t, out, `"type":"error"`,
		"必须以可见 error 事件收尾，而不是静默结束；实际输出: %s", out)
	require.Contains(t, out, "上游",
		"错误文案应说明是上游未产生数据，而不是笼统 context canceled；实际输出: %s", out)
}
