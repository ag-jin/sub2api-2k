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
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
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
	mapping, ok := out["model_mapping"].(map[string]any)
	require.True(t, ok, "model_mapping 应为对象")
	assert.Equal(t, "glm-5.2", mapping["glm-5.2"])
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
	expiresAt, ok := out["expires_at"].(string)
	require.True(t, ok, "expires_at 应为字符串")
	parsed, err := time.Parse(time.RFC3339, expiresAt)
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
		assert.Empty(t, r.Header.Get("X-Enterprise-Id"), "个人账号不发 enterprise 头")
		assert.Equal(t, "1", r.Header.Get("X-No-Enterprise-Id"), "缺值走官方 X-No-* 缺省声明")
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
	out, err := transformCodeBuddyRequestBody(body, nil)
	require.NoError(t, err)
	assert.Equal(t, "true", gjson.GetBytes(out, "stream").Raw, "stream=true mandatory")
	assert.Equal(t, "true", gjson.GetBytes(out, "stream_options.include_usage").Raw)
	wantRoles := []string{"system", "user", "assistant"} // developer→system；其余不受影响
	for i, role := range wantRoles {
		assert.Equal(t, role, gjson.GetBytes(out, fmt.Sprintf("messages.%d.role", i)).String())
	}
	assert.Equal(t, "be terse", gjson.GetBytes(out, "messages.0.content").String())
}

// collectMessageRoles 大小写不敏感地收集 messages[*] 的角色值（键名拼写任意）。
func collectMessageRoles(body []byte) []string {
	var roles []string
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(key.String()), "role") {
				roles = append(roles, value.String())
				return false
			}
			return true
		})
		return true
	})
	return roles
}

// TestTransformCodeBuddyRequestBody_RoleKeyCaseInsensitive 键名大小写归一：
// 上游（Go 侧 JSON 解析）对键名大小写不敏感，{"Role":"developer"} 与 {"ROLE":"developer"}
// 都会被判为 developer 角色并拒绝（HTTP 400 / 业务码 11128
// "Illegal API invocation from an unapproved channel"；2026-09-13 对本机凭证实测：
// role / Role / ROLE 三种键名等价触发）。归一化若只按小写 role 定位键，
// 这类客户端的整条请求会被上游拒绝（经网关复现：{"Role":"developer"} → 400 Upstream error: 400）。
func TestTransformCodeBuddyRequestBody_RoleKeyCaseInsensitive(t *testing.T) {
	for _, key := range []string{"role", "Role", "ROLE"} {
		t.Run(key, func(t *testing.T) {
			body := []byte(fmt.Sprintf(
				`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"%s":"developer","content":"be terse"},{"role":"user","content":"hi"}]}`,
				key,
			))
			out, err := transformCodeBuddyRequestBody(body, nil)
			require.NoError(t, err)
			assert.Equal(t, []string{"system", "user"}, collectMessageRoles(out),
				"developer 角色必须归一化为 system（键名大小写无关）")
			assert.NotContains(t, string(out), `"developer"`, "出站体不得残留 developer 角色值")
		})
	}
}

func TestCodeBuddyChatHeaders_OfficialShape(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "enterprise_id": "ent-1", "domain": "www.codebuddy.cn",
	}}
	c := codeBuddyCCTestContext(t)
	h := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h, cb, c, nil)

	// 官方公共头（参考实现 workbuddy2api headers.go 的官方客户端形态）。
	assert.Equal(t, "application/json", h.Get("Content-Type"))
	assert.Equal(t, "application/json, text/event-stream", h.Get("Accept"))
	assert.Equal(t, "XMLHttpRequest", h.Get("X-Requested-With"))
	assert.Equal(t, "https://www.codebuddy.cn", h.Get("Origin"))
	assert.Equal(t, "https://www.codebuddy.cn/", h.Get("Referer"))
	assert.Equal(t, "zh-CN", h.Get("Accept-Language"))
	assert.Equal(t, "1", h.Get("X-CodeBuddy-Request"))
	assert.Equal(t, "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1", h.Get("User-Agent"))

	// 账号身份头（逐条对齐官方 CLI 鉴权拦截器）。
	assert.Equal(t, "u-1", h.Get("X-User-Id"))
	assert.Equal(t, "ent-1", h.Get("X-Enterprise-Id"))
	assert.Equal(t, "ent-1", h.Get("X-Tenant-Id"))
	assert.Equal(t, "www.codebuddy.cn", h.Get("X-Domain"))
	assert.Equal(t, "1", h.Get("X-No-Department-Info"), "无 departmentFullName → 官方口径是发 X-No-Department-Info")

	// 用量归属头。
	assert.Equal(t, "conversation", h.Get("X-Agent-Purpose"))
	assert.Equal(t, "craft", h.Get("X-Agent-Intent"), "官方默认意图（会话 meta 的 mode 缺失时回落 craft）")
	assert.Equal(t, "WorkBuddy", h.Get("X-IDE-Name"))
	assert.Equal(t, "WorkBuddy", h.Get("X-IDE-Type"))
	assert.Equal(t, "5.5.4", h.Get("X-IDE-Version"))
	assert.Equal(t, "WorkBuddy", h.Get("X-Product"))

	// 会话头族：聚合主键恒发 32 位 hex；消息级每出站独立；B3 与主键同源。
	convReq := h.Get("X-Conversation-Request-ID")
	msgID := h.Get("X-Conversation-Message-ID")
	assert.Regexp(t, "^[0-9a-f]{32}$", convReq)
	assert.Regexp(t, "^[0-9a-f]{32}$", msgID)
	assert.Equal(t, msgID, h.Get("X-Request-ID"))
	assert.Equal(t, convReq, h.Get("X-Root-Request-ID"))
	assert.Equal(t, convReq, h.Get("X-Trace-ID"))
	assert.Equal(t, convReq, h.Get("X-B3-TraceId"))
	assert.Equal(t, msgID[:16], h.Get("X-B3-SpanId"))
	assert.Equal(t, "1", h.Get("X-B3-Sampled"))

	// X-Conversation-ID 只透传不伪造。
	assert.Empty(t, h.Get("X-Conversation-ID"))
	// Authorization 由调用方按链路注入（Bearer accessToken）。
	assert.Equal(t, "", h.Get("Authorization"))
}

func TestCodeBuddyChatHeaders_ConversationIDPassthrough(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "domain": "www.codebuddy.cn",
	}}
	h := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h, cb, codeBuddyCCTestContext(t), []byte(`{"model":"auto","conversationId":"conv-camel"}`))
	assert.Equal(t, "conv-camel", h.Get("X-Conversation-ID"))

	h2 := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h2, cb, codeBuddyCCTestContext(t), []byte(`{"model":"auto","conversation_id":"conv-snake"}`))
	assert.Equal(t, "conv-snake", h2.Get("X-Conversation-ID"))
}

func TestCodeBuddyChatHeaders_MissingIdentityUsesNoDeclarations(t *testing.T) {
	// 个人账号：无 enterpriseId / 无 domain / 无部门 → 走官方 X-No-* 缺省声明，不发空串身份头。
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{"uid": "u-1"}}
	h := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h, cb, codeBuddyCCTestContext(t), nil)
	assert.Equal(t, "1", h.Get("X-No-Enterprise-Id"))
	assert.Empty(t, h.Get("X-Enterprise-Id"))
	assert.Empty(t, h.Get("X-Tenant-Id"))
	assert.Equal(t, "1", h.Get("X-No-Department-Info"), "无部门 → X-No-Department-Info")
	assert.Empty(t, h.Get("X-Department-Info"))
	assert.Empty(t, h.Get("X-Domain"), "无登录域 → 不发 X-Domain（官方无 X-No-Domain）")

	// 连 uid 都缺的极端情况：X-No-User-Id 声明。
	empty := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	h2 := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h2, empty, codeBuddyCCTestContext(t), nil)
	assert.Equal(t, "1", h2.Get("X-No-User-Id"))

	// 企业账号带部门：发 X-Department-Info 而不是 X-No-Department-Info（官方拦截器口径）。
	corp := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-2", "enterprise_id": "ent-2", "domain": "www.codebuddy.cn",
		"department_full_name": "平台研发部",
	}}
	h3 := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h3, corp, codeBuddyCCTestContext(t), nil)
	assert.Equal(t, "平台研发部", h3.Get("X-Department-Info"))
	assert.Empty(t, h3.Get("X-No-Department-Info"))
}

func TestCodeBuddyChatHeaders_GlobalRealmShape(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "domain": "www.workbuddy.ai",
	}}
	h := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h, cb, codeBuddyCCTestContext(t), nil)
	assert.Equal(t, "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1", h.Get("User-Agent"),
		"global 账号平台段必须是 WorkBuddy AI（送错品牌段会触发上游 403 code 11140）")
	assert.Equal(t, "https://www.workbuddy.ai", h.Get("Origin"))
	assert.Equal(t, "https://www.workbuddy.ai/", h.Get("Referer"))
	assert.Equal(t, "en-US", h.Get("Accept-Language"))
}

func TestCodeBuddyChatHeaders_CredentialOverrides(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "domain": "copilot.tencent.com",
		"client_version": "9.9.9", "cli_version": "1.2.3", "client_name": "Custom",
		"user_agent": "RawUA/1.0", "x_domain": "www.codebuddy.cn", "device_token": "dev-tok",
	}}
	h := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h, cb, codeBuddyCCTestContext(t), nil)
	assert.Equal(t, "RawUA/1.0", h.Get("User-Agent"), "user_agent 凭据最优先")
	assert.Equal(t, "9.9.9", h.Get("X-IDE-Version"))
	assert.Equal(t, "Custom", h.Get("X-Product"))
	assert.Equal(t, "www.codebuddy.cn", h.Get("X-Domain"), "x_domain 覆盖账号 domain")
	assert.Equal(t, "dev-tok", h.Get("X-Device-Token"))
	assert.Equal(t, "Custom", h.Get("X-Product"))

	// 未提供 device_token 时不得伪造设备令牌。
	plain := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{"uid": "u"}}
	h2 := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h2, plain, codeBuddyCCTestContext(t), nil)
	assert.Empty(t, h2.Get("X-Device-Token"))
}

func TestCodeBuddyChatHeaders_SessionIDStableWithinRequest(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{"uid": "u"}}
	c := codeBuddyCCTestContext(t)
	h1, h2 := http.Header{}, http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h1, cb, c, nil) // 同一次入站请求的第 1 次尝试
	applyCodeBuddyChatUpstreamHeaders(h2, cb, c, nil) // 失败重试/换号后的第 2 次尝试
	assert.Equal(t, h1.Get("X-Conversation-Request-ID"), h2.Get("X-Conversation-Request-ID"),
		"同一入站请求的所有上游尝试必须复用同一聚合主键")
	assert.NotEqual(t, h1.Get("X-Request-ID"), h2.Get("X-Request-ID"), "消息级 ID 每次出站独立")

	h3 := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h3, cb, codeBuddyCCTestContext(t), nil)
	assert.NotEqual(t, h1.Get("X-Conversation-Request-ID"), h3.Get("X-Conversation-Request-ID"),
		"不同入站请求不得共用聚合主键")
}

func TestCodeBuddyRefreshHeaders_OfficialShape(t *testing.T) {
	cb := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u-1", "enterprise_id": "ent-1", "domain": "www.codebuddy.cn", "refresh_token": "rt-1",
	}}
	h := http.Header{}
	applyCodeBuddyRefreshUpstreamHeaders(h, cb)
	assert.Equal(t, "application/json", h.Get("Accept"), "刷新请求非流式")
	assert.Equal(t, "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1", h.Get("User-Agent"))
	assert.Equal(t, "https://www.codebuddy.cn", h.Get("Origin"))
	assert.Equal(t, "zh-CN", h.Get("Accept-Language"))
	assert.Equal(t, "1", h.Get("X-CodeBuddy-Request"))
	assert.Equal(t, "rt-1", h.Get("X-Refresh-Token"))
	assert.Equal(t, "plugin", h.Get("X-Auth-Refresh-Source"))
	assert.Equal(t, "u-1", h.Get("X-User-Id"))
	// 刷新请求不带归属头 / 会话头族 / 设备令牌（与官方刷新请求同形）。
	assert.Empty(t, h.Get("X-Agent-Purpose"))
	assert.Empty(t, h.Get("X-Product"))
	assert.Empty(t, h.Get("X-Conversation-Request-ID"))
	assert.Empty(t, h.Get("X-Device-Token"))
}

func TestCodeBuddyChatHeaders_NonCodeBuddyNoop(t *testing.T) {
	kimi := &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	h := http.Header{}
	applyCodeBuddyChatUpstreamHeaders(h, kimi, codeBuddyCCTestContext(t), nil)
	assert.Equal(t, 0, len(h), "非 codebuddy 账号不得注入任何头")
}

func TestCodeBuddyHeadersNotOverridable(t *testing.T) {
	for _, name := range []string{
		"x-user-id", "x-enterprise-id", "x-tenant-id", "x-domain",
		"x-codebuddy-request", "x-agent-purpose", "x-ide-name", "x-ide-type", "x-ide-version",
		"x-device-token", "x-conversation-id", "x-conversation-request-id",
		"x-conversation-message-id", "x-root-request-id", "x-trace-id",
		"x-b3-traceid", "x-b3-spanid", "x-b3-sampled",
	} {
		assert.True(t, isHeaderOverrideBlockedName(name), "规范化头 %s 不可被 header override 覆写", name)
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
	choices, ok := parsed["choices"].([]any)
	require.True(t, ok, "choices 应为数组")
	require.NotEmpty(t, choices)
	choice, ok := choices[0].(map[string]any)
	require.True(t, ok, "choice 应为对象")
	message, ok := choice["message"].(map[string]any)
	require.True(t, ok, "message 应为对象")
	assert.Equal(t, "Hello", message["content"])
	assert.Equal(t, "stop", choice["finish_reason"])
	usageOut, ok := parsed["usage"].(map[string]any)
	require.True(t, ok, "usage 应为对象")
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
	choices, ok := parsed["choices"].([]any)
	require.True(t, ok, "choices 应为数组")
	require.NotEmpty(t, choices)
	choice, ok := choices[0].(map[string]any)
	require.True(t, ok, "choice 应为对象")
	assert.Equal(t, "tool_calls", choice["finish_reason"])
	msgObj, ok := choice["message"].(map[string]any)
	require.True(t, ok, "message 应为对象")
	tcs, ok := msgObj["tool_calls"].([]any)
	require.True(t, ok, "tool_calls 应为数组")
	require.Len(t, tcs, 2)
	ta, ok := tcs[0].(map[string]any)
	require.True(t, ok, "tool_call 应为对象")
	assert.Equal(t, "call_A", ta["id"])
	taFn, ok := ta["function"].(map[string]any)
	require.True(t, ok, "function 应为对象")
	assert.Equal(t, "get", taFn["name"])
	assert.Equal(t, `{"lat":1}`, taFn["arguments"], "arguments 分片拼接")
	tb, ok := tcs[1].(map[string]any)
	require.True(t, ok, "tool_call 应为对象")
	tbFn, ok := tb["function"].(map[string]any)
	require.True(t, ok, "function 应为对象")
	assert.Equal(t, "put", tbFn["name"])
	assert.Equal(t, "{}", tbFn["arguments"])
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
	urls []string
}

func (u *scriptedHTTPUpstream) respond(req *http.Request) (*http.Response, error) {
	u.urls = append(u.urls, req.URL.String())
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

// --- 入口分流：CodeBuddy 上游只有 /v2/chat/completions（/v1/responses 实测 404） ---

const codeBuddyDispatchSSE = `data: {"id":"m1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n" +
	`data: {"id":"m1","model":"deepseek-v4.1-flash","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n" +
	"data: [DONE]\n"

// TestForwardAsAnthropic_CodeBuddyUsesChatCompletionsUpstream /v1/messages 入站必须直转 CC：
// CodeBuddy 上游不提供 /v1/responses（实测 404），缺分流时 Anthropic→Responses 转换后的请求
// 会被发往 /v1/responses 且无兜底，客户端收到 404（2026-09-13 dev 实测 "Upstream error: 404"）。
func TestForwardAsAnthropic_CodeBuddyUsesChatCompletionsUpstream(t *testing.T) {
	upstream := &scriptedHTTPUpstream{seq: []string{"200\n" + codeBuddyDispatchSSE}}
	repo := &codeBuddyRereadAccountRepo{account: newCodeBuddyCCAccount("tok")}
	svc := newCodeBuddyCCGatewayForTest(t, repo, upstream)
	account := newCodeBuddyCCAccount("tok")
	c := codeBuddyCCTestContext(t)
	body := []byte(`{"model":"auto","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotEmpty(t, upstream.urls)
	assert.Contains(t, upstream.urls[0], "/v2/chat/completions",
		"CodeBuddy 必须直转 CC 端点，不得把 Responses 请求发往 /v1/responses")
}

// TestForwardAsChatCompletions_CodeBuddySkipsResponsesProbe CC 入站同样视为
// Responses 不支持：修复前每次请求都要先打一次 /v1/responses 再靠 404 兜底（浪费一次上游调用，
// 且上游若回非 404 的拒绝码即以错误响应告终）。
func TestForwardAsChatCompletions_CodeBuddySkipsResponsesProbe(t *testing.T) {
	upstream := &scriptedHTTPUpstream{seq: []string{"200\n" + codeBuddyDispatchSSE}}
	repo := &codeBuddyRereadAccountRepo{account: newCodeBuddyCCAccount("tok")}
	svc := newCodeBuddyCCGatewayForTest(t, repo, upstream)
	account := newCodeBuddyCCAccount("tok")
	c := codeBuddyCCTestContext(t)
	body := []byte(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Len(t, upstream.urls, 1, "CodeBuddy 不应先发 Responses 探测请求")
	assert.Contains(t, upstream.urls[0], "/v2/chat/completions")
}

// TestTransformCodeBuddyRequestBody_SanitizesBlockedPromptFingerprints 上游按**逐字精确匹配**
// （非语义审核）拦截第三方客户端注入的模板句/键值段，命中即整条请求 400/11128
// （2026-09-14 生产与 dev 双向实测）。净化三层：整句最小改写 / 计费头键值段整段剥离 /
// 尾随裸 kv 循环剥离；覆盖**所有角色**的 content（字符串与 parts）与 tool_calls.arguments
// （对齐参考实现 workbuddy2api sanitize.go；本端点实测 x-anthropic-billing-header 与
// Anthropic 反馈句为真实拦截，裸 11128 与 cc_entrypoint= 为防御性对齐）。
func TestTransformCodeBuddyRequestBody_SanitizesBlockedPromptFingerprints(t *testing.T) {
	blocked := "Main branch (you will usually use this for PRs)"
	neutral := "Default branch (you will usually use this for PRs)"
	ccBlocked := "You are Claude Code, Anthropic's official CLI for Claude"
	ccNeutral := "You are Claude Code, Anthropic's official CLI tool for Claude"

	t.Run("system string content", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"gitStatus:\nCurrent branch: main\n%s: main"},{"role":"user","content":"hi"}]}`,
			blocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), blocked, "出站体不得残留上游黑名单指纹")
		assert.Contains(t, string(out), neutral)
		assert.Contains(t, string(out), "Current branch: main", "指纹之外的内容必须保留")
	})

	t.Run("identity sentence rewritten in place", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"%s.\nkeep me"}]}`,
			ccBlocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), ccBlocked, "整句指纹必须改写（匹配串不带结尾标点，两种收尾形态一并覆盖）")
		assert.Contains(t, string(out), ccNeutral)
		assert.Contains(t, string(out), "keep me")
	})

	t.Run("assistant content parts keep other parts", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":[{"type":"text","text":"%s"},{"type":"text","text":"keep me"}]}]}`,
			blocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), blocked)
		assert.Contains(t, string(out), neutral)
		assert.Contains(t, string(out), "keep me", "同消息内其余 part 必须保留")
	})

	t.Run("all roles are sanitized", func(t *testing.T) {
		// 对齐参考实现：不按角色过滤（抗探测规则上下文无关；本端点实测计费头/反馈句整单拦截）。
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"%s: main"},{"role":"tool","tool_call_id":"c1","content":"%s"}]}`,
			blocked, ccBlocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), blocked)
		assert.NotContains(t, string(out), ccBlocked)
	})

	t.Run("tool_calls arguments sanitized even when content null", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"text\":\"%s\"}"}}]}]}`,
			ccBlocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), ccBlocked, "工具轮 content 为 null 时 arguments 也必须净化（历史盲区）")
		assert.Contains(t, string(out), "CLI tool for Claude")
	})

	t.Run("anthropic billing header segment stripped", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"billing: x-anthropic-billing-header: sha256=abc; other=1"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "x-anthropic-billing-header")
		assert.Contains(t, string(out), "other=1", "同段其余内容保留")
	})

	t.Run("uppercase billing header key variant stripped", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"X-Anthropic-Billing-Header: sha256=abc;"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, strings.ToLower(string(out)), "x-anthropic-billing-header", "键名大小写变体一并剥离")
	})

	t.Run("bare cc key value pairs stripped", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"cfg cc_version=2.1.0; cc_entrypoint=cli; tail"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "cc_version=")
		assert.NotContains(t, string(out), "cc_entrypoint=")
		assert.Contains(t, string(out), "tail")
	})

	t.Run("anthropic feedback sentence rewritten", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "To give feedback")
		assert.Contains(t, string(out), "To provide feedback", "整句按一字之差改写，语义不变")
	})

	t.Run("bare anti-probe number rewritten", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"老报错 code=11128 怎么处理"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "11128", "裸错误码即拦截条件（防御性对齐）")
		assert.Contains(t, string(out), "11-128")
	})

	t.Run("lowercase role key assistant", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"Role":"assistant","content":"%s"}]}`,
			blocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), blocked, "角色键名大小写不影响净化")
	})

	t.Run("absent fingerprint leaves key content intact", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Contains(t, string(out), `"be terse"`)
		assert.Contains(t, string(out), `"messages"`)
	})

	t.Run("unclosed parenthetical is not a fingerprint", func(t *testing.T) {
		// 上游按字面量匹配（括号收尾敏感）：非黑名单文本不得被改写。
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"Main branch (you will usually use this for PRs: main"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Contains(t, string(out), "for PRs: main")
	})

	t.Run("output stays valid json with both constraints applied", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			`{"model":"deepseek-v4.1-flash","messages":[{"Role":"developer","content":"%s"},{"role":"user","content":"hi"}]}`,
			blocked,
		))
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"system", "user"}, collectMessageRoles(out), "developer 归一化不受净化影响")
		assert.NotContains(t, string(out), blocked)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(out, &decoded), "净化后仍是合法 JSON")
	})
}

// TestTransformCodeBuddyRequestBody_ToolChoiceNormalized 上游 tool_choice 是 string 类型，
// 对象形式直接 400 code 11101（2026-09-14 dev 实测 "cannot unmarshal object into Go struct
// field Request.tool_choice"）。归一化规则对齐参考实现 normalizeToolChoice。
func TestTransformCodeBuddyRequestBody_ToolChoiceNormalized(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"get_time","description":"now","parameters":{"type":"object","properties":{}}}}]`
	cases := []struct {
		name      string
		in        string
		wantTC    string // "" = 期望 tool_choice 字段被删除
		wantTools bool   // true = tools 字段保留
	}{
		{"object function → name", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":{"type":"function","function":{"name":"get_time"}},"messages":[{"role":"user","content":"hi"}]}`, tools), "get_time", true},
		{"object function without name → auto", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":{"type":"function"},"messages":[{"role":"user","content":"hi"}]}`, tools), "auto", true},
		{"object auto → string", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"hi"}]}`, tools), "auto", true},
		{"object required → string", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":{"type":"required"},"messages":[{"role":"user","content":"hi"}]}`, tools), "required", true},
		{"object none → drop tool_choice and tools", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":{"type":"none"},"messages":[{"role":"user","content":"hi"}]}`, tools), "", false},
		{"string none → drop tool_choice and tools", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":"none","messages":[{"role":"user","content":"hi"}]}`, tools), "", false},
		{"string auto kept", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":"auto","messages":[{"role":"user","content":"hi"}]}`, tools), "auto", true},
		{"unknown object dropped", fmt.Sprintf(`{"model":"deepseek-v4.1-flash",%s,"tool_choice":{"type":"mystery"},"messages":[{"role":"user","content":"hi"}]}`, tools), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := transformCodeBuddyRequestBody([]byte(tc.in), nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTC, gjson.GetBytes(out, "tool_choice").String(), "tool_choice 必须是 string 或删除")
			assert.Equal(t, tc.wantTools, gjson.GetBytes(out, "tools").Exists(), "tools 保留/删除按规则")
		})
	}
}

// TestTransformCodeBuddyRequestBody_DeepSeekThinking 思考开关与档位，
// 逐条对齐官方 CLI 规则链（codebuddy.js）：deepseek 分支出站恒带 thinking.type
// （enabled/disabled），档位与 thinking.type 并存；默认档来自模型目录 reasoning.effort
// （codeBuddyModelDefaultEfforts 快照）。本端点实测：reasoning_effort 才是开关，
// type="disabled" 是真关闸。
func TestTransformCodeBuddyRequestBody_DeepSeekThinking(t *testing.T) {
	t.Run("absent thinking injects default effort and enabled type", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", gjson.GetBytes(out, "reasoning_effort").String(), "目录默认档（deepseek = high）")
		assert.Equal(t, "enabled", gjson.GetBytes(out, "thinking.type").String(), "官方 deepseek 分支：type 未定义则置 enabled")
	})

	t.Run("level carried in thinking.type becomes reasoning_effort", func(t *testing.T) {
		for _, lvl := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
			want := lvl
			if lvl == "xhigh" {
				want = "high" // 官方 LegacyXhighFallbackRule：CN 模型无档位映射表，xhigh 回落 high
			}
			body := []byte(fmt.Sprintf(`{"model":"deepseek-v4.1-flash","thinking":{"type":"%s"},"messages":[{"role":"user","content":"hi"}]}`, lvl))
			out, err := transformCodeBuddyRequestBody(body, nil)
			require.NoError(t, err)
			assert.Equal(t, want, gjson.GetBytes(out, "reasoning_effort").String(),
				"客户端放在 type 里的档位要转成 effort 才真正生效（type 单独不开关）")
			assert.Equal(t, lvl, gjson.GetBytes(out, "thinking.type").String(), "type 已定义则不改写（官方口径）")
		}
	})

	t.Run("explicit effort kept and type filled", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","thinking":{"type":"enabled"},"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "low", gjson.GetBytes(out, "reasoning_effort").String())

		camel := []byte(`{"model":"deepseek-v4-flash","reasoningEffort":"max","messages":[{"role":"user","content":"hi"}]}`)
		out2, err := transformCodeBuddyRequestBody(camel, nil)
		require.NoError(t, err)
		assert.Equal(t, "max", gjson.GetBytes(out2, "reasoningEffort").String(), "camel 档位原样保留")
		assert.False(t, gjson.GetBytes(out2, "reasoning_effort").Exists(), "已有 camel 档位时不额外补 snake")
		assert.Equal(t, "enabled", gjson.GetBytes(out2, "thinking.type").String(), "deepseek 补 type=enabled")
	})

	t.Run("disabled drops effort and keeps disabled", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.False(t, gjson.GetBytes(out, "reasoning_effort").Exists(), "关思考时不带 effort")
		assert.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
	})

	t.Run("catalog default effort applied to non-deepseek models", func(t *testing.T) {
		// 官方客户端对目录声明了 reasoning.effort 的模型一律用默认档兜底；
		// 实测 glm-5.2 / hy3 属"默认关但能推理"，不注入则永远不出思维链。
		body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "medium", gjson.GetBytes(out, "reasoning_effort").String(), "glm-5.2 目录默认档 = medium")
		assert.False(t, gjson.GetBytes(out, "thinking").Exists(), "非 deepseek 不写 thinking.type（官方仅 deepseek 分支写）")

		hy3 := []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)
		out2, err := transformCodeBuddyRequestBody(hy3, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", gjson.GetBytes(out2, "reasoning_effort").String())
	})

	t.Run("xhigh falls back to high when no level map", func(t *testing.T) {
		// 官方 LegacyXhighFallbackRule：无 thinkingLevelMap 的模型 xhigh → high（CN 模型均无 map）。
		body := []byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", gjson.GetBytes(out, "reasoning_effort").String())

		viaType := []byte(`{"model":"deepseek-v4-pro","thinking":{"type":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`)
		out2, err := transformCodeBuddyRequestBody(viaType, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", gjson.GetBytes(out2, "reasoning_effort").String(), "type 里的 xhigh 同样回落 high")
	})

	t.Run("sdk fields cleaned", func(t *testing.T) {
		// 官方 SdkFieldCleanupRule：verbosity / reasoning_summary 一律剥离。
		body := []byte(`{"model":"deepseek-v4.1-flash","verbosity":"medium","reasoning_summary":"auto","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.False(t, gjson.GetBytes(out, "verbosity").Exists())
		assert.False(t, gjson.GetBytes(out, "reasoning_summary").Exists())
	})

	t.Run("small output budget disables thinking", func(t *testing.T) {
		// 上游思考无上限：预算不足时思考会把正文挤没（实测 mt=2048 → 正文 0 字符、finish=length）。
		body := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String(), "预算 <4096 强制关思考")
		assert.False(t, gjson.GetBytes(out, "reasoning_effort").Exists())

		// 预算充足 → 保留客户端档位并补 type=enabled。
		big := []byte(`{"model":"deepseek-v4.1-flash","max_completion_tokens":8192,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
		out2, err := transformCodeBuddyRequestBody(big, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", gjson.GetBytes(out2, "reasoning_effort").String())
		assert.Equal(t, "enabled", gjson.GetBytes(out2, "thinking.type").String())

		// 未设置预算（视为不限）→ 保持客户端意图。
		unset := []byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
		out3, err := transformCodeBuddyRequestBody(unset, nil)
		require.NoError(t, err)
		assert.Equal(t, "medium", gjson.GetBytes(out3, "reasoning_effort").String())
	})

	t.Run("camelCase xhigh falls back too", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":8192,"reasoningEffort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", gjson.GetBytes(out, "reasoningEffort").String(), "camelCase 的 xhigh 同样回落 high")
	})

	t.Run("model without catalog default untouched", func(t *testing.T) {
		body := []byte(`{"model":"glm-5.0","messages":[{"role":"user","content":"hi"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.False(t, gjson.GetBytes(out, "reasoning_effort").Exists())
		assert.False(t, gjson.GetBytes(out, "thinking").Exists())
	})
}

// TestTransformCodeBuddyRequestBody_ReasoningContentBackfill DeepSeek 多轮一致性：会话里出现
// reasoning 痕迹后，上游要求所有 assistant 消息都带 reasoning_content（官方客户端
// requiresReasoningContentOnAssistantMessages）。无痕迹时零改动。
func TestTransformCodeBuddyRequestBody_ReasoningContentBackfill(t *testing.T) {
	t.Run("trace triggers backfill on all assistant messages", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"assistant","content":"a1","reasoning":"think-1"},` +
			`{"role":"user","content":"u"},` +
			`{"role":"assistant","content":"a2"},` +
			`{"role":"assistant","content":"a3","reasoning_content":"keep-me"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "think-1", gjson.GetBytes(out, "messages.0.reasoning_content").String(), "有 reasoning → 复制其值")
		assert.Equal(t, "", gjson.GetBytes(out, "messages.2.reasoning_content").String(), "无痕迹的 assistant → 补空串")
		assert.Equal(t, "keep-me", gjson.GetBytes(out, "messages.3.reasoning_content").String(), "已有值不覆盖")
		assert.False(t, gjson.GetBytes(out, "messages.1.reasoning_content").Exists(), "user 消息不补")
	})

	t.Run("thinking enabled triggers backfill even without traces", func(t *testing.T) {
		// 官方触发条件：thinkingEnabled || 存在 reasoning 痕迹——deepseek 因目录默认档被开了思考，
		// 历史里即使没有 reasoning 也要给所有 assistant 补 reasoning_content（上游要求）。
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":"a1"},{"role":"user","content":"u"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, "", gjson.GetBytes(out, "messages.0.reasoning_content").String(), "开思考下 assistant 补空串")
	})

	t.Run("thinking disabled and no trace leaves body unchanged", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":"a1"},{"role":"user","content":"u"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "reasoning_content")
	})

	t.Run("non-deepseek model untouched", func(t *testing.T) {
		body := []byte(`{"model":"glm-5.2","messages":[{"role":"assistant","content":"a1","reasoning":"t"}]}`)
		out, err := transformCodeBuddyRequestBody(body, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "reasoning_content")
	})
}

// TestTransformCodeBuddyRequestBody_GlobalRealmConsoleSystem 全局域（workbuddy.ai）首条非 system
// 时补 fallback system（对齐参考实现吸收的上游 PR #45：防 console 域 11128）；CN 域零改动。
func TestTransformCodeBuddyRequestBody_GlobalRealmConsoleSystem(t *testing.T) {
	globalAccount := &Account{ID: 1, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u", "domain": "www.workbuddy.ai",
	}}
	cnAccount := &Account{ID: 2, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"uid": "u", "domain": "www.codebuddy.cn",
	}}
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)

	out, err := transformCodeBuddyRequestBody(body, globalAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{"system", "user"}, collectMessageRoles(out), "global 域首条非 system 时补兜底 system")
	assert.Contains(t, string(out), "You are a helpful assistant.")

	outCN, err := transformCodeBuddyRequestBody(body, cnAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{"user"}, collectMessageRoles(outCN), "CN 域不注入兜底 system")

	alreadySystem := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	out2, err := transformCodeBuddyRequestBody(alreadySystem, globalAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{"system", "user"}, collectMessageRoles(out2), "首条已是 system 不重复注入")
}

// TestCodeBuddyUpstreamEnvelopeMessage 上游拒因（如 11128 安全策略拦截）只存在于
// {code,msg} 信封里，通用提取器不认顶层 msg；该辅助用于把客户端文案从不可诊断的
// "Upstream error: 400" 补全为带码与原文的形态。
func TestCodeBuddyUpstreamEnvelopeMessage(t *testing.T) {
	envelope := `{"code":11128,"displayMsg":{"en":"blocked"},"msg":"Illegal API invocation from an unapproved channel","requestId":"c7df365e"}`
	assert.Equal(t, "code 11128: Illegal API invocation from an unapproved channel",
		codebuddy.CodeBuddyUpstreamEnvelopeMessage([]byte(envelope)))

	assert.Equal(t, "session expired",
		codebuddy.CodeBuddyUpstreamEnvelopeMessage([]byte(`{"code":0,"msg":"session expired"}`)))

	assert.Equal(t, "", codebuddy.CodeBuddyUpstreamEnvelopeMessage([]byte(`{"error":{"message":"boom"}}`)),
		"非 CodeBuddy 信封形态返回空串，避免覆盖通用提取结果")
	assert.Equal(t, "", codebuddy.CodeBuddyUpstreamEnvelopeMessage(nil))
}
