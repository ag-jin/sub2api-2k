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
	assert.Equal(t, codeBuddyUpstreamUserAgent, gotUA)
	assert.Equal(t, "text/event-stream", gotAccept)
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
