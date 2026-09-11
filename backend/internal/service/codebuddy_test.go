package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// --- Platform constants & routing normalization ---

func TestNormalizeOpenAICompatiblePlatform_KeepsCodeBuddy(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{PlatformCodeBuddy, PlatformCodeBuddy},
		{PlatformGrok, PlatformGrok},
		{PlatformOpenCode, PlatformOpenCode},
		{PlatformKimi, PlatformKimi},
		{"", PlatformOpenAI},
		{"other", PlatformOpenAI},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, NormalizeOpenAICompatiblePlatform(tc.in), "input %q", tc.in)
	}
}

// --- Account predicates / base URL / API key capability ---

func TestCodeBuddyAccountPredicates(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"access_token": "at", "refresh_token": "rt", "uid": "u1", "enterprise_id": "",
	}}
	assert.True(t, cb.IsCodeBuddy())
	assert.True(t, cb.IsOpenAICompatible())
	assert.False(t, isOpenAIAccount(cb), "codebuddy must not enter the OpenAI runtime-block fastpath")
	assert.Equal(t, "https://copilot.tencent.com", cb.GetOpenAIBaseURL())
	assert.Equal(t, "at", cb.GetOpenAIProtocolAPIKey())
	assert.True(t, cb.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions))
	assert.False(t, cb.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses))
	assert.False(t, cb.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
	assert.Equal(t, "", cb.GetCodeBuddyEnterpriseID())
}

func TestCodeBuddyModelWhitelistExemptAuto(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"model_mapping": map[string]any{"glm-5.2": "glm-5.2"},
	}}
	assert.True(t, cb.IsModelSupported(CodeBuddyAutoModel), "auto must bypass model_mapping whitelist")
	assert.True(t, cb.IsModelSupported("glm-5.2"))
	assert.False(t, cb.IsModelSupported("unknown-model"))

	cbFree := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}
	assert.True(t, cbFree.IsModelSupported("anything"))
}

// --- Credential normalization ---

func TestNormalizeCodeBuddyCredentials_AuthJSONWhitelist(t *testing.T) {
	raw := map[string]any{
		"auth": map[string]any{
			"accessToken":  "at",
			"refreshToken": "rt",
			"expiresAt":    1759000000000, // 毫秒
			"domain":       "www.codebuddy.cn",
		},
		"account": map[string]any{
			"uid":          "uid-1",
			"enterpriseId": nil, // 个人账号
		},
		"accounts":  []any{"volatile"},
		"other_key": "drop-me",
	}
	out := NormalizeCodeBuddyCredentials(raw)
	require.NotNil(t, out)
	assert.Equal(t, "at", out["access_token"])
	assert.Equal(t, "rt", out["refresh_token"])
	assert.Equal(t, "www.codebuddy.cn", out["domain"])
	assert.Equal(t, "uid-1", out["uid"])
	assert.Equal(t, "", out["enterprise_id"])
	expiresAt, ok := out["expires_at"].(string)
	require.True(t, ok)
	assert.Equal(t, "2025-09-27T19:06:40Z", expiresAt)
	// 易变键与未知键全部丢弃（白名单字段式）。
	assert.NotContains(t, out, "accounts")
	assert.NotContains(t, out, "other_key")
}

func TestNormalizeCodeBuddyCredentials_FlatPassthrough(t *testing.T) {
	out := NormalizeCodeBuddyCredentials(map[string]any{
		"access_token":  "at",
		"refresh_token": "rt",
		"expires_at":    "2026-10-01T00:00:00Z",
		"uid":           "uid-1",
		"enterprise_id": "ent-1",
		"base_url":      "https://custom.example.com",
		"model_mapping": map[string]any{"glm-5.2": "glm-5.2"},
	})
	require.NotNil(t, out)
	assert.Equal(t, "at", out["access_token"])
	assert.Equal(t, "ent-1", out["enterprise_id"])
	assert.Equal(t, "2026-10-01T00:00:00Z", out["expires_at"])
	assert.Equal(t, "https://custom.example.com", out["base_url"])
	assert.Equal(t, "glm-5.2", out["model_mapping"].(map[string]any)["glm-5.2"])
}

// Scenario: 输入已是 RFC3339 字符串的 expires_at 必须原样保留，即使 expires_in
// 仍在场（旧值）——否则每次编辑保存都会用过期 expiresIn 重算，expires_at 持续漂移。
// 仅 expires_at 缺失时才由 expires_in 换算（flat 与嵌套 auth 两条路径一致）。
func TestNormalizeCodeBuddyCredentials_RFC3339ExpiresAtWinsOverStaleExpiresIn(t *testing.T) {
	fixed := "2026-10-01T00:00:00Z"

	out := NormalizeCodeBuddyCredentials(map[string]any{
		"access_token": "at",
		"expires_at":   fixed,
		"expires_in":   60, // 过期秒数，不得参与重算
	})
	require.NotNil(t, out)
	assert.Equal(t, fixed, out["expires_at"], "flat path must keep RFC3339 expires_at as-is")

	out = NormalizeCodeBuddyCredentials(map[string]any{
		"auth": map[string]any{
			"accessToken": "at",
			"expiresAt":   fixed,
			"expiresIn":   60,
		},
	})
	require.NotNil(t, out)
	assert.Equal(t, fixed, out["expires_at"], "nested auth path must keep RFC3339 expiresAt as-is")

	// expires_at 缺失时 expires_in 兜底换算仍有效（now+60s）。
	out = NormalizeCodeBuddyCredentials(map[string]any{
		"access_token": "at",
		"expires_in":   60,
	})
	require.NotNil(t, out)
	parsed, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(60*time.Second), parsed, 5*time.Second)
}

// Scenario: refresh_expires_in 在编辑保存往返中不得丢失（白名单保留键）。
func TestNormalizeCodeBuddyCredentials_PreservesRefreshExpiresIn(t *testing.T) {
	out := NormalizeCodeBuddyCredentials(map[string]any{
		"access_token":       "at",
		"refresh_expires_in": 86400,
	})
	require.NotNil(t, out)
	assert.Equal(t, 86400, out["refresh_expires_in"])
}

// --- Refresh: expiresIn boundary handling ---

func TestBuildCodeBuddyRefreshedCredentials_ExpiresInBoundaries(t *testing.T) {
	// 60s：expiresAt = now(ms) + 60s。
	out, err := BuildCodeBuddyRefreshedCredentials(1_700_000_000_000, "www.codebuddy.cn", map[string]any{
		"accessToken": "new-at", "refreshToken": "new-rt", "expiresIn": 60,
	})
	require.NoError(t, err)
	assert.Equal(t, "new-at", out["access_token"])
	assert.Equal(t, "new-rt", out["refresh_token"], "refreshToken 轮换写回")
	assert.Equal(t, int64(1_700_000_000_000), out["last_refresh_time"])
	assert.Equal(t, "www.codebuddy.cn", out["domain"])
	assert.Equal(t, "2023-11-14T22:14:20Z", out["expires_at"], "expiresIn=60 → now(ms)+60s")

	// 0：expiresAt = now（下一轮 NeedsRefresh 立即触发）。
	out, err = BuildCodeBuddyRefreshedCredentials(1_700_000_000_000, "d", map[string]any{
		"accessToken": "new-at", "expiresIn": 0,
	})
	require.NoError(t, err)
	assert.Equal(t, "2023-11-14T22:13:20Z", out["expires_at"])

	// 负值：expiresAt 在过去，如实换算不 clamp。
	out, err = BuildCodeBuddyRefreshedCredentials(1_700_000_000_000, "", map[string]any{
		"accessToken": "new-at", "expiresIn": -5,
	})
	require.NoError(t, err)
	assert.Equal(t, "2023-11-14T22:13:15Z", out["expires_at"])

	// domain 继承：响应缺失时沿用刷新前 auth.domain。
	out, err = BuildCodeBuddyRefreshedCredentials(1_700_000_000_000, "old.example.com", map[string]any{
		"accessToken": "new-at", "expiresIn": 1,
	})
	require.NoError(t, err)
	assert.Equal(t, "old.example.com", out["domain"])
}

func TestBuildCodeBuddyRefreshedCredentials_MissingAccessTokenFails(t *testing.T) {
	_, err := BuildCodeBuddyRefreshedCredentials(0, "", map[string]any{"expiresIn": 60})
	require.Error(t, err)
	assert.True(t, isNonRetryableRefreshError(err), "missing accessToken 归为确认性刷新失败")
}

// --- Refresher: needs refresh boundaries ---

func TestCodeBuddyTokenRefresher_NeedsRefresh(t *testing.T) {
	r := NewCodeBuddyTokenRefresher()
	window := 24 * time.Hour

	missingRT := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}
	assert.False(t, r.NeedsRefresh(missingRT, window))
	assert.False(t, r.CanRefresh(missingRT))

	withoutAccess := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"refresh_token": "rt",
	}}
	assert.True(t, r.NeedsRefresh(withoutAccess, window), "access_token 缺失 → 立即刷新")
	assert.True(t, r.CanRefresh(withoutAccess))

	valid := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"refresh_token": "rt",
		"access_token":  "at",
		"expires_at":    time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}}
	assert.False(t, r.NeedsRefresh(valid, window))
	assert.True(t, r.CanRefresh(valid))

	expiring := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"refresh_token": "rt",
		"expires_at":    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}}
	assert.True(t, r.NeedsRefresh(expiring, window))
}

// --- Refresher refresh flow against a fake upstream ---

func TestCodeBuddyTokenRefresher_Refresh_FakeUpstream(t *testing.T) {
	var seenPath string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "plugin", r.Header.Get("X-Auth-Refresh-Source"))
		assert.NotEmpty(t, r.Header.Get("X-Refresh-Token"))
		assert.Equal(t, "Bearer test-access", r.Header.Get("Authorization"))
		assert.Equal(t, "user-1", r.Header.Get("X-User-Id"))
		assert.Empty(t, r.Header.Get("X-Enterprise-Id"), "个人账号 enterpriseId 归一为空串")
		assert.Equal(t, "www.codebuddy.cn", r.Header.Get("X-Domain"))
		seenBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":5184000,"refreshExpiresIn":7776000}}`))
	}))
	defer srv.Close()

	account := &Account{ID: 42, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"access_token": "test-access", "refresh_token": "old-rt",
		"uid": "user-1", "enterprise_id": "", "domain": "www.codebuddy.cn",
	}}
	refresher := NewCodeBuddyTokenRefresher()
	refresher.testBaseURL = srv.URL
	newCreds, err := refresher.Refresh(context.Background(), account)
	require.NoError(t, err)
	assert.Equal(t, "/v2/plugin/auth/token/refresh", seenPath)
	assert.Equal(t, "{}", strings.TrimSpace(string(seenBody)), "refresh body 恒为 {}")
	assert.Equal(t, "new-rt", newCreds["refresh_token"])
	assert.Equal(t, "www.codebuddy.cn", newCreds["domain"])
	assert.NotZero(t, newCreds["last_refresh_time"])
	expiresAt, ok := newCreds["expires_at"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, expiresAt)
	_, ok = newCreds["refresh_expires_at"]
	require.True(t, ok)
}

func TestCodeBuddyTokenRefresher_Refresh_401Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
	}))
	defer srv.Close()
	account := &Account{ID: 7, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"refresh_token": "r", "access_token": "a",
	}}
	refresher := NewCodeBuddyTokenRefresher()
	refresher.testBaseURL = srv.URL
	_, err := refresher.Refresh(context.Background(), account)
	require.Error(t, err)
	var authErr *codeBuddyRefreshAuthError
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, http.StatusUnauthorized, authErr.status)
	assert.True(t, isNonRetryableRefreshError(err), "401/403 → 不可重试（StatusError 提示重录）")
}

func TestCodeBuddyTokenRefresher_Refresh_Transient5xxRetriable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	account := &Account{ID: 9, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"refresh_token": "r", "access_token": "a",
	}}
	refresher := NewCodeBuddyTokenRefresher()
	refresher.testBaseURL = srv.URL
	_, err := refresher.Refresh(context.Background(), account)
	require.Error(t, err)
	assert.False(t, isNonRetryableRefreshError(err), "瞬态 5xx 应进重试链，重试耗尽后 temp-unsched")
}

// --- Request body constraints ---

func TestTransformCodeBuddyRequestBody(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","stream":false,"messages":[{"role":"developer","content":"be terse"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	out, err := transformCodeBuddyRequestBody(body)
	require.NoError(t, err)
	assert.Equal(t, "true", gjson.GetBytes(out, "stream").Raw, "stream=true mandatory")
	assert.Equal(t, "true", gjson.GetBytes(out, "stream_options.include_usage").Raw)
	wantRoles := []string{"system", "user", "assistant"} // developer→system；其余不受影响
	for i, role := range wantRoles {
		assert.Equal(t, role, gjson.GetBytes(out, fmt.Sprintf("messages.%d.role", i)).String())
	}
	assert.Equal(t, "be terse", gjson.GetBytes(out, "messages.0.content").String())
}

func TestCodeBuddyHeaders_ExactNames(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "enterprise_id": "ent-1", "domain": "www.codebuddy.cn",
	}}
	h := http.Header{}
	applyCodeBuddyUpstreamHeaders(h, cb)
	assert.Equal(t, "u-1", h.Get("X-User-Id"))
	assert.Equal(t, "ent-1", h.Get("X-Enterprise-Id"))
	assert.Equal(t, "ent-1", h.Get("X-Tenant-Id"))
	assert.Equal(t, "www.codebuddy.cn", h.Get("X-Domain"))
	assert.NotEmpty(t, h.Get("User-Agent"))
	assert.Equal(t, "", h.Get("Authorization"), "Authorization 由 pipeline 注入")

	// 空 enterpriseId（个人账号）→ 空串 header 仍发送。
	emptyEnt := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "domain": "d",
	}}
	h2 := http.Header{}
	applyCodeBuddyUpstreamHeaders(h2, emptyEnt)
	v, has := h2["X-Enterprise-Id"]
	require.True(t, has, "空串也要发送 header")
	assert.Equal(t, []string{""}, v)

	// 非 codebuddy 账号不注入。
	kimi := &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	h3 := http.Header{}
	applyCodeBuddyUpstreamHeaders(h3, kimi)
	assert.Equal(t, "", h3.Get("X-User-Id"))
}

func TestCodeBuddyHeadersNotOverridable(t *testing.T) {
	for _, name := range []string{"x-user-id", "x-enterprise-id", "x-tenant-id", "x-domain"} {
		assert.True(t, isHeaderOverrideBlockedName(name), "专用身份头 %s 不可被 header override 覆写", name)
	}
	assert.True(t, (&Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}).IsHeaderOverrideEligible())
}

// --- Stream aggregation (non-streaming fallback) ---

func TestAggregateCodeBuddyCCResponse_TextAndUsage(t *testing.T) {
	sse := `data: {"id":"chat-1","created":1700,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"id":"chat-1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n" +
		`data: {"id":"chat-1","model":"deepseek-v4.1-flash","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}` + "\n" +
		"data: [DONE]\n"
	out, usage, model, err := aggregateCodeBuddyCCResponse(strings.NewReader(sse))
	require.NoError(t, err)
	assert.Equal(t, "deepseek-v4.1-flash", model, "model 用上游回显实值")
	assert.Equal(t, 11, usage.InputTokens)
	assert.Equal(t, 5, usage.OutputTokens)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	choice := parsed["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	assert.Equal(t, "Hello", message["content"])
	assert.Equal(t, "stop", choice["finish_reason"])
	usageOut := parsed["usage"].(map[string]any)
	assert.Equal(t, float64(11), usageOut["prompt_tokens"])
}

func TestAggregateCodeBuddyCCResponse_ToolCallsIndexFragments(t *testing.T) {
	sse := `data: {"id":"call-1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"get","arguments":"{\"lat\":"}}]}}]}` + "\n" +
		`data: {"id":"call-1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"id":"call_B","function":{"name":"put","arguments":"{}"}}]}}]}` + "\n" +
		`data: {"id":"call-1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}` + "\n" +
		"data: [DONE]\n"
	out, usage, model, err := aggregateCodeBuddyCCResponse(strings.NewReader(sse))
	require.NoError(t, err)
	assert.Equal(t, "glm-5.2", model)
	assert.Equal(t, 3, usage.InputTokens)
	assert.Equal(t, 9, usage.OutputTokens)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	choice := parsed["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "tool_calls", choice["finish_reason"])
	tcs := choice["message"].(map[string]any)["tool_calls"].([]any)
	require.Len(t, tcs, 2)
	ta := tcs[0].(map[string]any)
	assert.Equal(t, "call_A", ta["id"])
	assert.Equal(t, "get", ta["function"].(map[string]any)["name"])
	assert.Equal(t, `{"lat":1}`, ta["function"].(map[string]any)["arguments"], "arguments 分片拼接")
	tb := tcs[1].(map[string]any)
	assert.Equal(t, "put", tb["function"].(map[string]any)["name"])
	assert.Equal(t, "{}", tb["function"].(map[string]any)["arguments"])
}

// --- Redact lists coverage（对齐 audit_log_test 模式）---

func TestCodeBuddyCredentialKeysRedacted(t *testing.T) {
	for _, key := range []string{"access_token", "refresh_token", "uid", "enterprise_id", "domain"} {
		assert.True(t, IsSensitiveCredentialKey(key), "codebuddy credential key %q must be sensitive", key)
	}
	// expires_at 只是过期时间戳（非凭据），既有前端编辑流按普通字段回显，保持非敏感。
	assert.False(t, IsSensitiveCredentialKey("expires_at"), "expires_at must stay non-sensitive")
	assert.True(t, isAuditSensitiveBodyKey("access_token"))
	assert.True(t, isAuditSensitiveBodyKey("refresh_token"))
}

func TestRedactAuditBody_CodeBuddyTokens(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"kind": "codebuddy-import",
		"payload": map[string]any{
			"access_token":  "tok-ZZWILLING",
			"refresh_token": "ref-ZZWILLING",
			"uid":           "uid-ZZWILLING",
			"enterprise_id": "ent-ZZWILLING",
		},
	})
	out := RedactAuditBody(payload, "application/json")
	assert.NotContains(t, out, "tok-ZZWILLING")
	assert.NotContains(t, out, "ref-ZZWILLING")
	assert.NotContains(t, out, "uid-ZZWILLING")
	assert.NotContains(t, out, "ent-ZZWILLING")
	assert.Contains(t, out, "codebuddy-import")
}

func TestLogRedactHidesCodeBuddyTokens(t *testing.T) {
	line := `codebuddy refresh body {"access_token":"raw-secret-bearer"} refresh_token=ref-secret-bearer`
	out := logredact.RedactText(line)
	assert.NotContains(t, out, "raw-secret-bearer")
	assert.NotContains(t, out, "ref-secret-bearer")
}

// --- 上游 401：重读 DB 凭据 → 重试一次 → 仍失败判死（R3-H3）---

type codeBuddyRereadAccountRepo struct {
	AccountRepository
	account *Account
	err     error
	calls   int
}

func (r *codeBuddyRereadAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.account, nil
}

// scriptedHTTPUpstream 按次序返回预设响应，并记录每次请求的 Authorization / X-User-Id。
type scriptedHTTPUpstream struct {
	seq  []string // 每项为一个完整响应：状态码 + "\n" + body
	auth []string
	uid  []string
}

func (u *scriptedHTTPUpstream) respond(req *http.Request) (*http.Response, error) {
	u.auth = append(u.auth, req.Header.Get("Authorization"))
	u.uid = append(u.uid, req.Header.Get("X-User-Id"))
	s := u.seq[0]
	u.seq = u.seq[1:]
	status := 200
	if i := strings.IndexByte(s, '\n'); i > 0 {
		if v, err := strconv.Atoi(s[:i]); err == nil {
			status = v
			s = s[i+1:]
		}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(s)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (u *scriptedHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.respond(req)
}

func (u *scriptedHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.respond(req)
}

func newCodeBuddyCCGatewayForTest(t *testing.T, repo AccountRepository, upstream *scriptedHTTPUpstream) *OpenAIGatewayService {
	t.Helper()
	svc := NewOpenAIGatewayService(
		repo,
		nil, nil, nil, nil, nil, nil,
		&config.Config{},
		nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	return svc
}

func newCodeBuddyCCAccount(token string) *Account {
	return &Account{
		ID:       99,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":  token,
			"refresh_token": "rt-1",
			"uid":           "uid-77",
			"enterprise_id": "",
			"domain":        "codebuddy",
		},
	}
}

func codeBuddyCCTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func TestSendCCUpstreamRequest_CodeBuddy401RereadsAndRetries(t *testing.T) {
	upstream := &scriptedHTTPUpstream{seq: []string{
		"401\n{\"error\":{\"message\":\"unauthorized\"}}",
		"200\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n",
	}}
	repo := &codeBuddyRereadAccountRepo{account: newCodeBuddyCCAccount("new-token")}
	svc := newCodeBuddyCCGatewayForTest(t, repo, upstream)
	account := newCodeBuddyCCAccount("old-token")
	c := codeBuddyCCTestContext(t)

	resp, _, err := svc.sendCCUpstreamRequest(context.Background(), c, account,
		"https://upstream.example/v2/chat/completions", []byte(`{}`), true,
		account.GetOpenAIProtocolAPIKey(), "ua-test", "", time.Duration(0))
	require.NoError(t, err)
	require.NotNil(t, resp)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "token 变化 → 用新凭据重试一次成功")
	assert.Equal(t, 2, len(upstream.auth), "上游请求恰好两次")
	assert.Equal(t, "Bearer old-token", upstream.auth[0], "首次用旧凭据")
	assert.Equal(t, "Bearer new-token", upstream.auth[1], "重试用重读后的新凭据")
	assert.Equal(t, "uid-77", upstream.uid[1], "重试请求仍注入专用身份头")
	assert.Equal(t, 1, repo.calls, "仅一次 DB reread")
	_ = body
}

func TestSendCCUpstreamRequest_CodeBuddy401SameTokenFallsThrough(t *testing.T) {
	upstream := &scriptedHTTPUpstream{seq: []string{
		"401\n{\"error\":{\"message\":\"unauthorized\"}}",
	}}
	repo := &codeBuddyRereadAccountRepo{account: newCodeBuddyCCAccount("old-token")}
	svc := newCodeBuddyCCGatewayForTest(t, repo, upstream)
	account := newCodeBuddyCCAccount("old-token")
	c := codeBuddyCCTestContext(t)

	resp, _, err := svc.sendCCUpstreamRequest(context.Background(), c, account,
		"https://upstream.example/v2/chat/completions", []byte(`{}`), true,
		account.GetOpenAIProtocolAPIKey(), "ua-test", "", time.Duration(0))
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	bearer := upstream.auth
	assert.Equal(t, []string{"Bearer old-token"}, bearer, "token 未变化 → 不重试")
	assert.Equal(t, 1, repo.calls, "仍执行一次 DB reread（判死前的兜底）")
	// 响应体仍可读（错误链需要读取错误信息）。
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Contains(t, string(body), "unauthorized")
}

func TestSendCCUpstreamRequest_CodeBuddy401RereadErrFallsThrough(t *testing.T) {
	upstream := &scriptedHTTPUpstream{seq: []string{
		"401\n{\"error\":{\"message\":\"unauthorized\"}}",
	}}
	repo := &codeBuddyRereadAccountRepo{err: fmt.Errorf("db down")}
	svc := newCodeBuddyCCGatewayForTest(t, repo, upstream)
	account := newCodeBuddyCCAccount("old-token")
	c := codeBuddyCCTestContext(t)

	resp, _, err := svc.sendCCUpstreamRequest(context.Background(), c, account,
		"https://upstream.example/v2/chat/completions", []byte(`{}`), true,
		account.GetOpenAIProtocolAPIKey(), "ua-test", "", time.Duration(0))
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Len(t, upstream.auth, 1, "reread 失败 → 原响应上抛，不重试")
}
