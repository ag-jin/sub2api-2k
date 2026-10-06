//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bigmodel"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 票 04 的 handler 级测试：四个管理端点（login-url / exchange / create-from-login /
// relogin）在真实 gin 路由上的 HTTP 状态与 ZHIPU_OAUTH_* 错误码映射。
// 服务层用真实 ZhipuOAuthService + canned 上游桩，保证断言的是接缝行为而非替身。

// zhipuHandlerEnvelope 是管理端统一响应壳（pkg/response.Response）。
type zhipuHandlerEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Reason  string          `json:"reason"`
	Data    json.RawMessage `json:"data"`
}

func zhipuHandlerPtrInt(value int) *int { return &value }

// zhipuHandlerProxyRepoStub 只实现登录链路用到的 GetByID，其余由嵌入接口兜底。
type zhipuHandlerProxyRepoStub struct {
	service.ProxyRepository
	proxies map[int64]*service.Proxy
	err     error
}

func (r *zhipuHandlerProxyRepoStub) GetByID(_ context.Context, id int64) (*service.Proxy, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.proxies[id], nil
}

// zhipuHandlerUpstreamStub 是按 URL 路由的 HTTPUpstream 桩：记录调用并返回 canned
// 应答，绝不打真实上游。
type zhipuHandlerUpstreamStub struct {
	mu      sync.Mutex
	calls   int
	respond func(req *http.Request) (*http.Response, error)
}

func (s *zhipuHandlerUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.respond == nil {
		return nil, context.DeadlineExceeded
	}
	return s.respond(req)
}

func (s *zhipuHandlerUpstreamStub) DoWithTLS(
	req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

func (s *zhipuHandlerUpstreamStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// 兑换 + api_key 解析的官方报文形状（research FINAL-REPORT §六），期望值在本测试里
// 独立写成字面量，不复用实现里的拼接逻辑。
const (
	zhipuHandlerTokenBody = `{"code":0,"msg":"ok","data":{"token":"jwt-token","expires_in":3600,` +
		`"bigmodel":{"access_token":"at-token","refresh_token":"rt-token"}}}`
	zhipuHandlerCustomerBody = `{"code":0,"data":{"organizations":[{"organizationId":"org-1",` +
		`"organizationName":"默认机构","projects":[{"projectId":"proj-1","projectName":"默认项目"}]}]}}`
	zhipuHandlerAPIKeysBody = `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"key-1"}]}`
	zhipuHandlerCopyBody    = `{"code":0,"data":{"secretKey":"secret-1"}}`
)

func zhipuHandlerJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// zhipuHandlerResponder 还原登录链路的四段应答；某条路径可用 override 替换。
func zhipuHandlerResponder(override func(req *http.Request) *http.Response) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		if override != nil {
			if resp := override(req); resp != nil {
				return resp, nil
			}
		}
		switch {
		case strings.Contains(req.URL.Path, "/oauth/token"):
			return zhipuHandlerJSONResponse(http.StatusOK, zhipuHandlerTokenBody), nil
		case strings.Contains(req.URL.Path, "/getCustomerInfo"):
			return zhipuHandlerJSONResponse(http.StatusOK, zhipuHandlerCustomerBody), nil
		case strings.Contains(req.URL.Path, "/copy/"):
			return zhipuHandlerJSONResponse(http.StatusOK, zhipuHandlerCopyBody), nil
		case strings.Contains(req.URL.Path, "/api_keys"):
			return zhipuHandlerJSONResponse(http.StatusOK, zhipuHandlerAPIKeysBody), nil
		default:
			return nil, fmt.Errorf("unexpected upstream URL: %s", req.URL)
		}
	}
}

// zhipuHandlerTestRouter 按冻结契约的路径挂载端点（路径本身由
// routes 包的 registerZhipuOAuthRoutes 测试断言）。
func zhipuHandlerTestRouter(h *ZhipuOAuthHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/admin/zhipu/oauth/login-url", h.GenerateLoginURL)
	router.POST("/admin/zhipu/oauth/exchange", h.Exchange)
	router.POST("/admin/zhipu/oauth/create-from-login", h.CreateAccountFromLogin)
	router.POST("/admin/zhipu/accounts/:id/relogin", h.ReloginAccount)
	return router
}

func zhipuHandlerPost(t *testing.T, router *gin.Engine, path, body string) (*httptest.ResponseRecorder, zhipuHandlerEnvelope) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	envelope := zhipuHandlerEnvelope{}
	if recorder.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	}
	return recorder, envelope
}

type zhipuLoginURLPayload struct {
	LoginURL  string `json:"login_url"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

func TestZhipuOAuthHandlerGenerateLoginURL(t *testing.T) {
	const redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"

	t.Run("returns the frozen login-url payload and registers the session", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)
		handler := NewZhipuOAuthHandler(svc, nil)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler),
			"/admin/zhipu/oauth/login-url", `{"redirect_uri":"`+redirectURI+`"}`)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, 0, envelope.Code)
		var payload zhipuLoginURLPayload
		require.NoError(t, json.Unmarshal(envelope.Data, &payload))
		require.NotEmpty(t, payload.SessionID)
		require.Len(t, payload.State, 32, "state 为 16 字节 hex")
		require.Contains(t, payload.LoginURL, "state="+url.QueryEscape(payload.State))
		require.Contains(t, payload.LoginURL, "redirect="+url.QueryEscape(redirectURI))

		session, ok := store.Get(payload.SessionID)
		require.True(t, ok, "登录会话必须按返回的 session_id 登记")
		require.Equal(t, payload.State, session.State)
	})

	t.Run("accepts an empty body as no proxy and default redirect", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)
		handler := NewZhipuOAuthHandler(svc, nil)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler),
			"/admin/zhipu/oauth/login-url", `{}`)

		require.Equal(t, http.StatusOK, recorder.Code)
		var payload zhipuLoginURLPayload
		require.NoError(t, json.Unmarshal(envelope.Data, &payload))
		require.Contains(t, payload.LoginURL, "redirect="+url.QueryEscape(redirectURI))
	})

	t.Run("maps a missing proxy to 400 ZHIPU_OAUTH_PROXY_NOT_FOUND", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)
		handler := NewZhipuOAuthHandler(svc, nil)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler),
			"/admin/zhipu/oauth/login-url", `{"proxy_id":404}`)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, "ZHIPU_OAUTH_PROXY_NOT_FOUND", envelope.Reason)
	})
}

func TestZhipuOAuthHandlerExchange(t *testing.T) {
	const (
		state      = "state-abcdef0123456789abcdef0123456789"
		sessionID  = "session-abcdef0123456789abcdef01234567"
		redirect   = "http://127.0.0.1:53699/oauth/callback/bigmodel"
		postTarget = "/admin/zhipu/oauth/exchange"
	)
	liveSession := func(store *bigmodel.SessionStore) {
		store.Set(sessionID, &bigmodel.OAuthSession{State: state, RedirectURI: redirect})
	}

	cases := []struct {
		name string
		// upstream 为 nil 时服务不注入 HTTP 上游（防御分支）。
		upstream func() *zhipuHandlerUpstreamStub
		// wantUpstreamCalls 非 nil 时断言上游调用次数（本地校验失败必须为 0）。
		wantUpstreamCalls *int
		seed              func(store *bigmodel.SessionStore)
		body              string
		wantStatus        int
		wantReason        string
	}{
		{
			name: "exchanges the auth code for the frozen credential payload",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
			},
			seed:       liveSession,
			body:       `{"session_id":"` + sessionID + `","state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "rejects a missing session_id before touching upstream",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(0),
			seed:              liveSession,
			body:              `{"state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus:        http.StatusBadRequest,
		},
		{
			name: "rejects a missing auth_code before touching upstream",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(0),
			seed:              liveSession,
			body:              `{"session_id":"` + sessionID + `","state":"` + state + `"}`,
			wantStatus:        http.StatusBadRequest,
		},
		{
			name: "maps an unknown session to 400 ZHIPU_OAUTH_SESSION_NOT_FOUND",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(0),
			body:              `{"session_id":"session-missing","state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus:        http.StatusBadRequest,
			wantReason:        "ZHIPU_OAUTH_SESSION_NOT_FOUND",
		},
		{
			name: "maps an expired session to 400 ZHIPU_OAUTH_SESSION_NOT_FOUND",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(0),
			seed: func(store *bigmodel.SessionStore) {
				store.Set(sessionID, &bigmodel.OAuthSession{
					State:       state,
					RedirectURI: redirect,
					CreatedAt:   time.Now().Add(-bigmodel.SessionTTL - time.Minute),
				})
			},
			body:       `{"session_id":"` + sessionID + `","state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: "ZHIPU_OAUTH_SESSION_NOT_FOUND",
		},
		{
			name: "maps a state mismatch to 400 ZHIPU_OAUTH_STATE_MISMATCH",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(0),
			seed:              liveSession,
			body:              `{"session_id":"` + sessionID + `","state":"state-other","auth_code":"auth-code-1"}`,
			wantStatus:        http.StatusBadRequest,
			wantReason:        "ZHIPU_OAUTH_STATE_MISMATCH",
		},
		{
			name: "maps a risk-control exchange rejection to 502 ZHIPU_OAUTH_EXCHANGE_FAILED",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(func(req *http.Request) *http.Response {
					if strings.Contains(req.URL.Path, "/oauth/token") {
						return zhipuHandlerJSONResponse(http.StatusOK, `{"code":3012,"msg":"unusual activity"}`)
					}
					return nil
				})}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(1), // 兑换被拒后不再解析 api_key
			seed:              liveSession,
			body:              `{"session_id":"` + sessionID + `","state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus:        http.StatusBadGateway,
			wantReason:        "ZHIPU_OAUTH_EXCHANGE_FAILED",
		},
		{
			name: "maps an upstream 401 during api key resolution to 502 ZHIPU_OAUTH_APIKEY_RESOLVE_FAILED",
			upstream: func() *zhipuHandlerUpstreamStub {
				return &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(func(req *http.Request) *http.Response {
					if strings.Contains(req.URL.Path, "/getCustomerInfo") {
						return zhipuHandlerJSONResponse(http.StatusUnauthorized, `{"code":1001,"msg":"invalid token"}`)
					}
					return nil
				})}
			},
			wantUpstreamCalls: zhipuHandlerPtrInt(2), // 兑换成功 + 解析首个调用被 401 拒绝
			seed:              liveSession,
			body:              `{"session_id":"` + sessionID + `","state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus:        http.StatusBadGateway,
			wantReason:        "ZHIPU_OAUTH_APIKEY_RESOLVE_FAILED",
		},
		{
			name:       "maps a missing http upstream to 500 ZHIPU_OAUTH_HTTP_UPSTREAM_UNAVAILABLE",
			seed:       liveSession,
			body:       `{"session_id":"` + sessionID + `","state":"` + state + `","auth_code":"auth-code-1"}`,
			wantStatus: http.StatusInternalServerError,
			wantReason: "ZHIPU_OAUTH_HTTP_UPSTREAM_UNAVAILABLE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := bigmodel.NewSessionStore()
			t.Cleanup(store.Stop)
			if tc.seed != nil {
				tc.seed(store)
			}
			// 未配置上游时必须传真正的 nil 接口（typed nil 会绕过服务的防御分支）。
			var upstream service.HTTPUpstream
			var stub *zhipuHandlerUpstreamStub
			if tc.upstream != nil {
				stub = tc.upstream()
				upstream = stub
			}
			svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
			handler := NewZhipuOAuthHandler(svc, nil)

			recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget, tc.body)

			require.Equal(t, tc.wantStatus, recorder.Code)
			require.Equal(t, tc.wantReason, envelope.Reason, "响应 reason 必须与票面错误码一致")
			if tc.wantUpstreamCalls != nil {
				require.NotNil(t, stub)
				require.Equal(t, *tc.wantUpstreamCalls, stub.callCount())
			}
		})
	}

	t.Run("credential payload uses the frozen json field names", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		liveSession(store)
		upstream := &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
		svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
		handler := NewZhipuOAuthHandler(svc, nil)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget,
			`{"session_id":"`+sessionID+`","state":"`+state+`","auth_code":"auth-code-1"}`)

		require.Equal(t, http.StatusOK, recorder.Code)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(envelope.Data, &raw))
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		// 前端 05 的类型声明逐字依赖这五个字段名。
		require.ElementsMatch(t,
			[]string{"api_key", "access_token", "zcodejwttoken", "refresh_token", "plan_level"}, keys)

		var credential service.ZhipuLoginCredential
		require.NoError(t, json.Unmarshal(envelope.Data, &credential))
		require.Equal(t, service.ZhipuLoginCredential{
			APIKey:        "key-1.secret-1",
			AccessToken:   "at-token",
			ZCodeJWTToken: "jwt-token",
			RefreshToken:  "rt-token",
			PlanLevel:     service.AccountModeCoding,
		}, credential)
	})
}

// zhipuHandlerGroupRepoStub 只实现默认分组解析需要的 ListActiveByPlatform。
type zhipuHandlerGroupRepoStub struct {
	service.AdminGroupRepository
	groups []service.Group
}

func (r *zhipuHandlerGroupRepoStub) ListActiveByPlatform(_ context.Context, _ string) ([]service.Group, error) {
	return r.groups, nil
}

// zhipuHandlerAccountRepoStub 记录建号落库与分组绑定两个动作。
type zhipuHandlerAccountRepoStub struct {
	service.AdminAccountRepository
	created  []*service.Account
	bindings map[int64][]int64
	err      error
}

func (r *zhipuHandlerAccountRepoStub) Create(_ context.Context, account *service.Account) error {
	if r.err != nil {
		return r.err
	}
	account.ID = int64(900 + len(r.created))
	r.created = append(r.created, account)
	return nil
}

func (r *zhipuHandlerAccountRepoStub) BindGroups(_ context.Context, accountID int64, groupIDs []int64) error {
	if r.bindings == nil {
		r.bindings = make(map[int64][]int64)
	}
	r.bindings[accountID] = groupIDs
	return nil
}

// zhipuHandlerTestAdminService 用真实 AdminService 复用 admin_account.CreateAccount
// 全链路（默认分组解析 / 凭据净化 / 账号构造），只把落库接缝换成记录型桩。
func zhipuHandlerTestAdminService(groupRepo *zhipuHandlerGroupRepoStub, accountRepo *zhipuHandlerAccountRepoStub) service.AdminService {
	return service.NewAdminService(
		nil, nil, groupRepo, accountRepo, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
}

type zhipuHandlerAccountPayload struct {
	ID                int64           `json:"id"`
	Name              string          `json:"name"`
	Platform          string          `json:"platform"`
	Type              string          `json:"type"`
	Credentials       map[string]any  `json:"credentials"`
	CredentialsStatus map[string]bool `json:"credentials_status"`
	Concurrency       int             `json:"concurrency"`
	Priority          int             `json:"priority"`
}

func TestZhipuOAuthHandlerCreateAccountFromLogin(t *testing.T) {
	const (
		postTarget = "/admin/zhipu/oauth/create-from-login"
		loginBody  = `{"name":"我的智谱账号","credentials":{` +
			`"auth_flow":"attacker-controlled","api_key":"key-1.secret-1","access_token":"at-token",` +
			`"zcodejwttoken":"jwt-token","refresh_token":"rt-token","account_mode":"payg",` +
			`"api_protocol":"anthropic","password":"p@ssw0rd","cookie":"session=abc",` +
			`"base_url":"https://evil.example","sso_token":"sso-1"},` +
			`"proxy_id":3,"concurrency":4,"priority":5,"group_ids":[7,8]}`
	)

	// 03 冻结白名单：请求里额外的键（含敏感明文）一律不得落库。
	frozenCredentials := map[string]any{
		"auth_flow":         "bigmodel_oauth",
		"api_key":           "key-1.secret-1",
		"access_token":      "at-token",
		"zcodejwttoken":     "jwt-token",
		"refresh_token":     "rt-token",
		"account_mode":      "coding",
		"api_protocol":      "anthropic",
		"zcode_client_sign": "v4",
	}

	t.Run("creates a zhipu apikey account with the frozen credential whitelist", func(t *testing.T) {
		groupRepo := &zhipuHandlerGroupRepoStub{groups: []service.Group{{ID: 42, Name: "zhipu-default"}}}
		accountRepo := &zhipuHandlerAccountRepoStub{}
		handler := NewZhipuOAuthHandler(service.NewZhipuOAuthService(nil, nil, nil, nil), zhipuHandlerTestAdminService(groupRepo, accountRepo))

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget, loginBody)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Len(t, accountRepo.created, 1)
		account := accountRepo.created[0]
		require.Equal(t, service.PlatformZhipu, account.Platform)
		require.Equal(t, service.AccountTypeAPIKey, account.Type)
		require.Equal(t, "我的智谱账号", account.Name)
		require.Equal(t, frozenCredentials, account.Credentials, "凭据必须由 BuildAccountCredentials 重建为冻结白名单")
		require.Equal(t, []int64{7, 8}, accountRepo.bindings[account.ID], "显式分组原样透传")

		// 净化是幂等的：CreateAccount 内部的 SanitizeStoredCredentials 不得再删掉白名单键。
		require.Equal(t, frozenCredentials,
			service.SanitizeStoredCredentials(service.PlatformZhipu, cloneZhipuHandlerMap(account.Credentials)))

		var payload zhipuHandlerAccountPayload
		require.NoError(t, json.Unmarshal(envelope.Data, &payload))
		require.Equal(t, service.PlatformZhipu, payload.Platform)
		require.Equal(t, service.AccountTypeAPIKey, payload.Type)
		require.Equal(t, 4, payload.Concurrency)
		require.Equal(t, 5, payload.Priority)
		require.True(t, payload.CredentialsStatus["has_api_key"], "响应只暴露敏感键的存在性")
		require.NotContains(t, payload.Credentials, "api_key")
		require.NotContains(t, payload.Credentials, "access_token")
		require.NotContains(t, recorder.Body.String(), "secret-1", "响应不得回显明文 wire key")
	})

	t.Run("binds the zhipu-default group when none is requested", func(t *testing.T) {
		groupRepo := &zhipuHandlerGroupRepoStub{groups: []service.Group{
			{ID: 41, Name: "kimi-default"},
			{ID: 42, Name: "zhipu-default"},
		}}
		accountRepo := &zhipuHandlerAccountRepoStub{}
		handler := NewZhipuOAuthHandler(service.NewZhipuOAuthService(nil, nil, nil, nil), zhipuHandlerTestAdminService(groupRepo, accountRepo))

		recorder, _ := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget,
			`{"credentials":{"api_key":"key-1.secret-1","access_token":"at-token","zcodejwttoken":"jwt-token","api_protocol":"adaptive"}}`)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Len(t, accountRepo.created, 1)
		require.Equal(t, []int64{42}, accountRepo.bindings[accountRepo.created[0].ID],
			"空 group_ids 走既有平台默认分组 zhipu-default")
		require.NotEmpty(t, accountRepo.created[0].Name, "空 name 必须有兜底账号名")
	})

	t.Run("rejects a payload without credentials", func(t *testing.T) {
		groupRepo := &zhipuHandlerGroupRepoStub{}
		accountRepo := &zhipuHandlerAccountRepoStub{}
		handler := NewZhipuOAuthHandler(service.NewZhipuOAuthService(nil, nil, nil, nil), zhipuHandlerTestAdminService(groupRepo, accountRepo))

		recorder, _ := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget, `{"name":"无名"}`)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Empty(t, accountRepo.created)
	})

	t.Run("rejects login credentials without a wire api key", func(t *testing.T) {
		for _, body := range []string{
			`{"credentials":{"access_token":"at-token","zcodejwttoken":"jwt-token"}}`,
			`{"credentials":{"api_key":"   ","access_token":"at-token"}}`,
		} {
			groupRepo := &zhipuHandlerGroupRepoStub{}
			accountRepo := &zhipuHandlerAccountRepoStub{}
			handler := NewZhipuOAuthHandler(service.NewZhipuOAuthService(nil, nil, nil, nil), zhipuHandlerTestAdminService(groupRepo, accountRepo))

			recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget, body)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "ZHIPU_OAUTH_INVALID_INPUT", envelope.Reason)
			require.Empty(t, accountRepo.created, "缺 wire key 的登录产物绝不建号")
		}
	})

	t.Run("surfaces a persistence failure", func(t *testing.T) {
		groupRepo := &zhipuHandlerGroupRepoStub{}
		accountRepo := &zhipuHandlerAccountRepoStub{err: errors.New("db down")}
		handler := NewZhipuOAuthHandler(service.NewZhipuOAuthService(nil, nil, nil, nil), zhipuHandlerTestAdminService(groupRepo, accountRepo))

		recorder, _ := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), postTarget, loginBody)

		require.Equal(t, http.StatusInternalServerError, recorder.Code)
	})
}

func cloneZhipuHandlerMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// zhipuHandlerAccountStoreStub 记录重登的两条写路径（凭据合并写 + extra 标记清除）。
type zhipuHandlerAccountStoreStub struct {
	service.AccountRepository
	account           *service.Account
	lookupErr         error
	credentialsUpdate map[string]any
	extraUpdate       map[string]any
	updateCalls       int
}

func (r *zhipuHandlerAccountStoreStub) GetByID(_ context.Context, _ int64) (*service.Account, error) {
	if r.lookupErr != nil {
		return nil, r.lookupErr
	}
	return r.account, nil
}

func (r *zhipuHandlerAccountStoreStub) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.updateCalls++
	r.credentialsUpdate = credentials
	return nil
}

func (r *zhipuHandlerAccountStoreStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraUpdate = updates
	return nil
}

func TestZhipuOAuthHandlerReloginAccount(t *testing.T) {
	const (
		state    = "state-abcdef0123456789abcdef0123456789"
		redirect = "http://127.0.0.1:53699/oauth/callback/bigmodel"
	)
	newHandler := func(t *testing.T, upstream service.HTTPUpstream, repo service.AccountRepository) (*ZhipuOAuthHandler, *bigmodel.SessionStore) {
		t.Helper()
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		store.Set("session-1", &bigmodel.OAuthSession{State: state, RedirectURI: redirect})
		svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		return NewZhipuOAuthHandler(svc, nil), store
	}
	loginBody := `{"session_id":"session-1","state":"` + state + `","auth_code":"auth-code-1"}`
	existingAccount := func() *service.Account {
		return &service.Account{
			ID:       7,
			Platform: service.PlatformZhipu,
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"auth_flow":     "bigmodel_oauth",
				"api_key":       "old-key.old-secret",
				"access_token":  "old-at",
				"zcodejwttoken": "old-jwt",
				"account_mode":  "coding",
				"api_protocol":  "adaptive",
				"custom_key":    "keep-me",
			},
			Extra: map[string]any{"zhipu_needs_relogin": true},
		}
	}

	t.Run("merges the fresh login credentials and clears the relogin flag", func(t *testing.T) {
		repo := &zhipuHandlerAccountStoreStub{account: existingAccount()}
		handler, store := newHandler(t, &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}, repo)
		router := zhipuHandlerTestRouter(handler)

		recorder, _ := zhipuHandlerPost(t, router, "/admin/zhipu/accounts/7/relogin", loginBody)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, 1, repo.updateCalls)
		require.Equal(t, "key-1.secret-1", repo.credentialsUpdate["api_key"])
		require.Equal(t, "at-token", repo.credentialsUpdate["access_token"])
		require.Equal(t, "jwt-token", repo.credentialsUpdate["zcodejwttoken"])
		require.Equal(t, "rt-token", repo.credentialsUpdate["refresh_token"])
		// 管理员配置（account_mode/api_protocol/自定义键）在合并写里必须保留。
		require.Equal(t, "coding", repo.credentialsUpdate["account_mode"])
		require.Equal(t, "adaptive", repo.credentialsUpdate["api_protocol"])
		require.Equal(t, "keep-me", repo.credentialsUpdate["custom_key"])
		require.Equal(t, map[string]any{"zhipu_needs_relogin": false}, repo.extraUpdate)
		_, stillThere := store.Get("session-1")
		require.False(t, stillThere, "重登成功后会话被消费")
	})

	t.Run("maps an unknown account to 404 ZHIPU_OAUTH_ACCOUNT_NOT_FOUND", func(t *testing.T) {
		repo := &zhipuHandlerAccountStoreStub{lookupErr: service.ErrAccountNotFound}
		handler, _ := newHandler(t, &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}, repo)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), "/admin/zhipu/accounts/7/relogin", loginBody)

		require.Equal(t, http.StatusNotFound, recorder.Code)
		require.Equal(t, "ZHIPU_OAUTH_ACCOUNT_NOT_FOUND", envelope.Reason)
	})

	t.Run("maps a state mismatch to 400 ZHIPU_OAUTH_STATE_MISMATCH", func(t *testing.T) {
		repo := &zhipuHandlerAccountStoreStub{account: existingAccount()}
		handler, _ := newHandler(t, &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}, repo)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), "/admin/zhipu/accounts/7/relogin",
			`{"session_id":"session-1","state":"state-other","auth_code":"auth-code-1"}`)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, "ZHIPU_OAUTH_STATE_MISMATCH", envelope.Reason)
		require.Zero(t, repo.updateCalls, "校验失败不写凭据")
	})

	t.Run("rejects a missing auth_code and a non-numeric account id", func(t *testing.T) {
		repo := &zhipuHandlerAccountStoreStub{account: existingAccount()}
		handler, _ := newHandler(t, &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}, repo)
		router := zhipuHandlerTestRouter(handler)

		recorder, _ := zhipuHandlerPost(t, router, "/admin/zhipu/accounts/7/relogin",
			`{"session_id":"session-1","state":"`+state+`"}`)
		require.Equal(t, http.StatusBadRequest, recorder.Code)

		recorder, _ = zhipuHandlerPost(t, router, "/admin/zhipu/accounts/abc/relogin", loginBody)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Zero(t, repo.updateCalls)
	})

	t.Run("maps a missing account repository to 500 ZHIPU_OAUTH_ACCOUNT_REPO_UNAVAILABLE", func(t *testing.T) {
		handler, _ := newHandler(t, &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}, nil)

		recorder, envelope := zhipuHandlerPost(t, zhipuHandlerTestRouter(handler), "/admin/zhipu/accounts/7/relogin", loginBody)

		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		require.Equal(t, "ZHIPU_OAUTH_ACCOUNT_REPO_UNAVAILABLE", envelope.Reason)
	})
}

// TestZhipuOAuthHandlerLoginFlowEndToEnd 串起 design M1 关键数据流 1：
// login-url → exchange → create-from-login（真实 service + 真实 CreateAccount），
// 产出账号必须能被既有转发链的 GetOpenAIProtocolAPIKey() 直接取到 wire key。
func TestZhipuOAuthHandlerLoginFlowEndToEnd(t *testing.T) {
	groupRepo := &zhipuHandlerGroupRepoStub{groups: []service.Group{{ID: 42, Name: "zhipu-default"}}}
	accountRepo := &zhipuHandlerAccountRepoStub{}
	store := bigmodel.NewSessionStore()
	t.Cleanup(store.Stop)
	upstream := &zhipuHandlerUpstreamStub{respond: zhipuHandlerResponder(nil)}
	svc := service.NewZhipuOAuthService(&zhipuHandlerProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
	handler := NewZhipuOAuthHandler(svc, zhipuHandlerTestAdminService(groupRepo, accountRepo))
	router := zhipuHandlerTestRouter(handler)

	// 1) 生成登录链接并登记会话。
	recorder, envelope := zhipuHandlerPost(t, router, "/admin/zhipu/oauth/login-url", `{}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	var login zhipuLoginURLPayload
	require.NoError(t, json.Unmarshal(envelope.Data, &login))

	// 2) 用授权码兑换登录产物。
	recorder, envelope = zhipuHandlerPost(t, router, "/admin/zhipu/oauth/exchange",
		`{"session_id":"`+login.SessionID+`","state":"`+login.State+`","auth_code":"auth-code-1"}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	var credential service.ZhipuLoginCredential
	require.NoError(t, json.Unmarshal(envelope.Data, &credential))

	// 3) 前端（06）持凭据提交建号：body 与 buildZhipuLoginCredentials 的输出同形。
	credentials := map[string]any{
		"auth_flow":     "bigmodel_oauth",
		"api_key":       credential.APIKey,
		"access_token":  credential.AccessToken,
		"zcodejwttoken": credential.ZCodeJWTToken,
		"account_mode":  "coding",
		"api_protocol":  "anthropic",
	}
	if credential.RefreshToken != "" {
		credentials["refresh_token"] = credential.RefreshToken
	}
	body, err := json.Marshal(map[string]any{"name": "端到端智谱账号", "credentials": credentials})
	require.NoError(t, err)
	recorder, _ = zhipuHandlerPost(t, router, "/admin/zhipu/oauth/create-from-login", string(body))
	require.Equal(t, http.StatusOK, recorder.Code)

	require.Len(t, accountRepo.created, 1)
	account := accountRepo.created[0]
	require.Equal(t, "key-1.secret-1", account.GetOpenAIProtocolAPIKey(),
		"建号后可被既有转发链取到 wire key（不新增转发面改动）")
	require.True(t, account.IsZhipuLoginManaged())
	require.Equal(t, []int64{42}, accountRepo.bindings[account.ID])
}
