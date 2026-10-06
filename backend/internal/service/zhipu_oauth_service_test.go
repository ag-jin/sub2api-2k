//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bigmodel"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// 登录态判定的真值表（票 03 验收标准 5 / design B1）：
// platform=zhipu && type=apikey && credentials["auth_flow"]=="bigmodel_oauth"，
// 三个维度任一不符即 false（缺失键、空值、type=oauth、其它平台）。
func TestAccountIsZhipuLoginManaged(t *testing.T) {
	managedCredentials := map[string]any{"auth_flow": "bigmodel_oauth"}

	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{
			name:    "nil account",
			account: nil,
			want:    false,
		},
		{
			name: "zhipu apikey with bigmodel_oauth marker",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: managedCredentials,
			},
			want: true,
		},
		{
			name: "zhipu apikey without auth_flow key",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "12345.secret"},
			},
			want: false,
		},
		{
			name: "zhipu apikey with empty auth_flow",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"auth_flow": ""},
			},
			want: false,
		},
		{
			name: "zhipu apikey with foreign auth_flow",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"auth_flow": "api_key"},
			},
			want: false,
		},
		{
			name: "zhipu apikey with nil credentials",
			account: &Account{
				Platform: PlatformZhipu,
				Type:     AccountTypeAPIKey,
			},
			want: false,
		},
		{
			name: "zhipu oauth account keeps the marker but is not managed",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeOAuth,
				Credentials: managedCredentials,
			},
			want: false,
		},
		{
			name: "openai apikey with the marker is not managed",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: managedCredentials,
			},
			want: false,
		},
		{
			name: "kimi apikey with the marker is not managed",
			account: &Account{
				Platform:    PlatformKimi,
				Type:        AccountTypeAPIKey,
				Credentials: managedCredentials,
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.account.IsZhipuLoginManaged())
		})
	}
}

// 登录态 token getter（票 03 验收标准 5）：托管账号返回凭据值，
// 非托管账号或缺键返回空串（不 panic，调用方无需先做 nil 判定）。
func TestAccountZhipuLoginTokenGetters(t *testing.T) {
	managedCredentials := map[string]any{
		"auth_flow":     "bigmodel_oauth",
		"access_token":  "at-token",
		"zcodejwttoken": "jwt-token",
	}

	cases := []struct {
		name      string
		account   *Account
		wantToken string
		wantJWT   string
	}{
		{
			name:      "nil account",
			account:   nil,
			wantToken: "",
			wantJWT:   "",
		},
		{
			name: "managed account exposes both tokens",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: managedCredentials,
			},
			wantToken: "at-token",
			wantJWT:   "jwt-token",
		},
		{
			name: "managed account with both keys missing returns empty strings",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"auth_flow": "bigmodel_oauth"},
			},
			wantToken: "",
			wantJWT:   "",
		},
		{
			name: "managed account with nil-valued tokens returns empty strings",
			account: &Account{
				Platform: PlatformZhipu,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"auth_flow":     "bigmodel_oauth",
					"access_token":  nil,
					"zcodejwttoken": nil,
				},
			},
			wantToken: "",
			wantJWT:   "",
		},
		{
			name: "non-managed zhipu account does not expose token credentials",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"access_token": "at-token", "zcodejwttoken": "jwt-token"},
			},
			wantToken: "",
			wantJWT:   "",
		},
		{
			name: "zhipu oauth account does not expose token credentials",
			account: &Account{
				Platform:    PlatformZhipu,
				Type:        AccountTypeOAuth,
				Credentials: managedCredentials,
			},
			wantToken: "",
			wantJWT:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantToken, tc.account.GetZhipuAccessToken())
			require.Equal(t, tc.wantJWT, tc.account.GetZhipuZCodeJWTToken())
		})
	}
}

// zhipuLoginCredentialKeyWhitelist 是本票冻结的凭据键契约（design M1）：
// auth_flow / api_key / access_token / zcodejwttoken / account_mode / api_protocol，
// refresh_token 仅在兑换返回非空值时出现；zcode_client_sign=v4 是 B 裁决的默认签名章
// （150% 验证实测教训：缺章 → fail-open 静默无签名 → 0.67 折扣失效）。05/06/07 前端按同一集合构造凭据，
// 键名漂移会静默产生「登录了但转发取不到 key」，故此处逐键锁死。
var zhipuLoginCredentialKeyWhitelist = []string{
	"auth_flow",
	"api_key",
	"access_token",
	"zcodejwttoken",
	"account_mode",
	"api_protocol",
	"zcode_client_sign"}

func TestZhipuOAuthServiceBuildAccountCredentialsFrozenKeySet(t *testing.T) {
	fullCredential := &ZhipuLoginCredential{
		APIKey:        "12345.secret",
		AccessToken:   "at-token",
		ZCodeJWTToken: "jwt-token",
		RefreshToken:  "rt-token",
		PlanLevel:     "coding",
	}

	t.Run("complete credential emits the frozen key set plus refresh_token", func(t *testing.T) {
		credentials := (&ZhipuOAuthService{}).BuildAccountCredentials(fullCredential, APIProtocolAnthropic)

		require.Equal(t, map[string]any{
			"auth_flow":     ZhipuLoginAuthFlow,
			"api_key":       "12345.secret",
			"access_token":  "at-token",
			"zcodejwttoken": "jwt-token",
			"refresh_token": "rt-token",
			"account_mode":  AccountModeCoding,
			"api_protocol":  APIProtocolAnthropic,
			// B 裁决：登录账号默认启用签名 V4（缺章 = fail-open 静默无签名，0.67 失效）
			"zcode_client_sign": "v4",
		}, credentials)
		require.ElementsMatch(t,
			append(append([]string{}, zhipuLoginCredentialKeyWhitelist...), "refresh_token"),
			keysOf(credentials))
		// plan_level 是兑换响应字段（供管理端展示），不是凭据键：不得落库。
		require.NotContains(t, credentials, "plan_level")
	})

	t.Run("empty refresh_token stays out of the key set", func(t *testing.T) {
		credential := *fullCredential
		credential.RefreshToken = ""

		credentials := (&ZhipuOAuthService{}).BuildAccountCredentials(&credential, APIProtocolChatCompletions)

		require.Equal(t, map[string]any{
			"auth_flow":         ZhipuLoginAuthFlow,
			"api_key":           "12345.secret",
			"access_token":      "at-token",
			"zcodejwttoken":     "jwt-token",
			"account_mode":      AccountModeCoding,
			"api_protocol":      APIProtocolChatCompletions,
			"zcode_client_sign": "v4",
		}, credentials)
		require.ElementsMatch(t, zhipuLoginCredentialKeyWhitelist, keysOf(credentials))
	})

	t.Run("nil credential still emits the frozen key set", func(t *testing.T) {
		credentials := (&ZhipuOAuthService{}).BuildAccountCredentials(nil, APIProtocolAnthropic)

		require.ElementsMatch(t, zhipuLoginCredentialKeyWhitelist, keysOf(credentials))
		require.Equal(t, ZhipuLoginAuthFlow, credentials["auth_flow"])
		require.Equal(t, AccountModeCoding, credentials["account_mode"])
		require.Equal(t, APIProtocolAnthropic, credentials["api_protocol"])
	})
}

// keysOf 返回 map 的顶层键集合（用于「键集合等于白名单」的显式断言）。
func keysOf(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// zhipuOAuthProxyRepoStub 只实现获取代理所必需的方法，其余由嵌入接口兜底。
type zhipuOAuthProxyRepoStub struct {
	ProxyRepository
	proxies map[int64]*Proxy
	err     error
}

func (r *zhipuOAuthProxyRepoStub) GetByID(_ context.Context, id int64) (*Proxy, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.proxies[id], nil
}

// 独立事实源：research FINAL-REPORT §六 记录的登录页形状
// （bigmodel.cn/login?appId=zcode&redirect=...&state=...）；本测试不复用 bigmodel.BuildLoginURL。
func expectedZhipuLoginURL(redirectURI, state string) string {
	return "https://bigmodel.cn/login?appId=zcode&redirect=" +
		url.QueryEscape(redirectURI) + "&state=" + url.QueryEscape(state)
}

func TestZhipuOAuthServiceGenerateLoginURL(t *testing.T) {
	const redirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
	proxy := &Proxy{ID: 7, Protocol: "http", Host: "127.0.0.1", Port: 3128}

	t.Run("binds state and session id into the session store", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{proxies: map[int64]*Proxy{7: proxy}}, nil, nil, nil).
			WithSessionStore(store)

		result, err := svc.GenerateLoginURL(context.Background(), ptrInt64(7), redirectURI)

		require.NoError(t, err)
		require.NotEmpty(t, result.SessionID)
		require.Len(t, result.State, 32, "state 为 16 字节 hex")
		require.Equal(t, expectedZhipuLoginURL(redirectURI, result.State), result.LoginURL)

		session, ok := store.Get(result.SessionID)
		require.True(t, ok, "session 必须按返回的 session_id 落库")
		require.Equal(t, result.State, session.State, "会话与返回的 state 必须是同一值")
		require.Equal(t, redirectURI, session.RedirectURI)
		require.Equal(t, proxy.URL(), session.ProxyURL)
	})

	t.Run("falls back to the local callback redirect when none is given", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)

		result, err := svc.GenerateLoginURL(context.Background(), nil, "   ")

		require.NoError(t, err)
		// 53699：刻意避开 ZCode 客户端占用的 53633 回调口（票 34，实测 2007 的根因）。
		require.Contains(t, result.LoginURL,
			"redirect="+url.QueryEscape("http://127.0.0.1:53699/oauth/callback/bigmodel"))

		session, ok := store.Get(result.SessionID)
		require.True(t, ok)
		require.Equal(t, "http://127.0.0.1:53699/oauth/callback/bigmodel", session.RedirectURI)
		require.Empty(t, session.ProxyURL, "无 proxy_id 时不走代理")
	})

	t.Run("issues a distinct session per call", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)

		first, err := svc.GenerateLoginURL(context.Background(), nil, redirectURI)
		require.NoError(t, err)
		second, err := svc.GenerateLoginURL(context.Background(), nil, redirectURI)
		require.NoError(t, err)

		require.NotEqual(t, first.SessionID, second.SessionID)
		require.NotEqual(t, first.State, second.State)
	})

	t.Run("reports a missing configured proxy", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)

		_, err := svc.GenerateLoginURL(context.Background(), ptrInt64(404), redirectURI)

		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_PROXY_NOT_FOUND", infraerrors.Reason(err))
	})

	t.Run("reports a proxy lookup failure", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := NewZhipuOAuthService(
			&zhipuOAuthProxyRepoStub{err: errors.New("db down")}, nil, nil, nil,
		).WithSessionStore(store)

		_, err := svc.GenerateLoginURL(context.Background(), ptrInt64(7), redirectURI)

		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_PROXY_LOOKUP_FAILED", infraerrors.Reason(err))
	})
}

// zhipuOAuthUpstreamStub 是按 URL 路由的 HTTPUpstream 桩：记录每次调用（含代理）
// 并返回 canned 响应，绝不打真实上游。
type zhipuOAuthUpstreamStub struct {
	mu       sync.Mutex
	requests []*http.Request
	proxies  []string
	respond  func(req *http.Request) (*http.Response, error)
}

func (s *zhipuOAuthUpstreamStub) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.proxies = append(s.proxies, proxyURL)
	s.mu.Unlock()
	if s.respond == nil {
		return nil, errors.New("unexpected upstream call")
	}
	return s.respond(req)
}

func (s *zhipuOAuthUpstreamStub) DoWithTLS(
	req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

func (s *zhipuOAuthUpstreamStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// zhipuOAuthTestFixtures 是兑换 + api_key 解析的官方报文形状（research FINAL-REPORT §六），
// 期望值在本测试里独立写成字面量，不复用实现里的拼接逻辑。
const (
	zhipuOAuthTestTokenBody = `{"code":0,"msg":"ok","data":{"token":"jwt-token","expires_in":3600,` +
		`"bigmodel":{"access_token":"at-token","refresh_token":"rt-token"}}}`
	zhipuOAuthTestCustomerBody = `{"code":0,"data":{"organizations":[{"organizationId":"org-1",` +
		`"organizationName":"默认机构","projects":[{"projectId":"proj-1","projectName":"默认项目"}]}]}}`
	zhipuOAuthTestAPIKeysBody = `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"key-1"}]}`
	zhipuOAuthTestCopyBody    = `{"code":0,"data":{"secretKey":"secret-1"}}`
	zhipuOAuthTestRedirectURI = "http://127.0.0.1:53699/oauth/callback/bigmodel"
)

func zhipuOAuthTestJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// zhipuOAuthTestResponder 还原登录链路的四段应答；某个路径可用 override 替换。
func zhipuOAuthTestResponder(override func(req *http.Request) *http.Response) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		if override != nil {
			if resp := override(req); resp != nil {
				return resp, nil
			}
		}
		switch {
		case strings.Contains(req.URL.Path, "/oauth/token"):
			return zhipuOAuthTestJSONResponse(http.StatusOK, zhipuOAuthTestTokenBody), nil
		case strings.Contains(req.URL.Path, "/getCustomerInfo"):
			return zhipuOAuthTestJSONResponse(http.StatusOK, zhipuOAuthTestCustomerBody), nil
		case strings.Contains(req.URL.Path, "/copy/"):
			return zhipuOAuthTestJSONResponse(http.StatusOK, zhipuOAuthTestCopyBody), nil
		case strings.Contains(req.URL.Path, "/api_keys"):
			return zhipuOAuthTestJSONResponse(http.StatusOK, zhipuOAuthTestAPIKeysBody), nil
		default:
			return nil, fmt.Errorf("unexpected upstream URL: %s", req.URL)
		}
	}
}

func zhipuOAuthTestSession(store *bigmodel.SessionStore, sessionID, state, proxyURL string, createdAt time.Time) {
	store.Set(sessionID, &bigmodel.OAuthSession{
		State:       state,
		RedirectURI: zhipuOAuthTestRedirectURI,
		ProxyURL:    proxyURL,
		CreatedAt:   createdAt,
	})
}

func TestZhipuOAuthServiceExchangeAndResolve(t *testing.T) {
	const state = "state-abcdef0123456789abcdef0123456789"

	t.Run("exchanges the auth code and resolves the wire api key", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "http://127.0.0.1:3128", time.Time{})

		credential, err := svc.ExchangeAndResolve(context.Background(), "session-1", state, "auth-code-1")

		require.NoError(t, err)
		require.Equal(t, &ZhipuLoginCredential{
			APIKey:        "key-1.secret-1",
			AccessToken:   "at-token",
			ZCodeJWTToken: "jwt-token",
			RefreshToken:  "rt-token",
			PlanLevel:     AccountModeCoding,
		}, credential)
		require.Equal(t, 4, upstream.callCount(), "兑换 + 组织/项目 + 列 key + 取 secret")

		// 兑换请求必须原样重放会话里的 redirect_uri 与 state（research §六 报文形状）。
		exchangeReq := upstream.requests[0]
		require.Contains(t, exchangeReq.URL.String(), "zcode.z.ai")
		require.Equal(t, "application/json", exchangeReq.Header.Get("Content-Type"))
		var payload map[string]string
		require.NoError(t, json.NewDecoder(exchangeReq.Body).Decode(&payload))
		require.Equal(t, map[string]string{
			"provider":     "bigmodel",
			"code":         "auth-code-1",
			"redirect_uri": zhipuOAuthTestRedirectURI,
			"state":        state,
		}, payload)

		// 会话代理是唯一出站通道参数（登记时选的代理必须贯穿兑换与解析）。
		for _, proxyURL := range upstream.proxies {
			require.Equal(t, "http://127.0.0.1:3128", proxyURL)
		}

		// 成功兑换后会话被消费：同一个 authCode 不可重复兑换。
		_, stillThere := store.Get("session-1")
		require.False(t, stillThere)
	})

	t.Run("rejects an unknown session without touching upstream", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)

		_, err := svc.ExchangeAndResolve(context.Background(), "session-missing", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_SESSION_NOT_FOUND", infraerrors.Reason(err))
		require.Zero(t, upstream.callCount())
	})

	t.Run("rejects an expired session without touching upstream", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Now().Add(-bigmodel.SessionTTL-time.Minute))

		_, err := svc.ExchangeAndResolve(context.Background(), "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_SESSION_NOT_FOUND", infraerrors.Reason(err))
		require.Zero(t, upstream.callCount())
	})

	t.Run("rejects a mismatched or empty state without touching upstream", func(t *testing.T) {
		for _, callerState := range []string{"state-other", "", "   "} {
			store := bigmodel.NewSessionStore()
			upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
			svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
			zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

			_, err := svc.ExchangeAndResolve(context.Background(), "session-1", callerState, "auth-code-1")

			require.Error(t, err, "caller state %q must be rejected", callerState)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Equal(t, "ZHIPU_OAUTH_STATE_MISMATCH", infraerrors.Reason(err))
			require.Zero(t, upstream.callCount())
			_, stillThere := store.Get("session-1")
			require.True(t, stillThere, "拒绝后会话保留，管理员可带正确 state 重试")
			store.Stop()
		}
	})

	t.Run("rejects empty session id and auth code as invalid input", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)

		for _, input := range []struct{ sessionID, code string }{
			{"", "auth-code-1"},
			{"   ", "auth-code-1"},
			{"session-1", ""},
			{"session-1", "   "},
		} {
			_, err := svc.ExchangeAndResolve(context.Background(), input.sessionID, state, input.code)
			require.Error(t, err, "session=%q code=%q", input.sessionID, input.code)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Equal(t, "ZHIPU_OAUTH_INVALID_INPUT", infraerrors.Reason(err))
		}
		require.Zero(t, upstream.callCount())
	})

	t.Run("surfaces a risk-control exchange rejection with its cause", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(func(req *http.Request) *http.Response {
			if strings.Contains(req.URL.Path, "/oauth/token") {
				return zhipuOAuthTestJSONResponse(http.StatusOK, `{"code":3012,"msg":"unusual activity"}`)
			}
			return nil
		})}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		_, err := svc.ExchangeAndResolve(context.Background(), "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_EXCHANGE_FAILED", infraerrors.Reason(err))
		require.True(t, bigmodel.IsRiskBlocked(err), "风控分类必须能从 cause 链上读到")
		require.Equal(t, 1, upstream.callCount(), "兑换被拒后不再解析 api_key")
	})

	t.Run("fails when the resolved api key is rejected", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(func(req *http.Request) *http.Response {
			if strings.Contains(req.URL.Path, "/getCustomerInfo") {
				return zhipuOAuthTestJSONResponse(http.StatusUnauthorized, `{"code":1001,"msg":"invalid token"}`)
			}
			return nil
		})}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		_, err := svc.ExchangeAndResolve(context.Background(), "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_APIKEY_RESOLVE_FAILED", infraerrors.Reason(err))
		require.True(t, bigmodel.IsAuthRejected(err), "401 分类必须能从 cause 链上读到")
	})

	t.Run("reports a missing http upstream instead of panicking", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, nil, nil, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		_, err := svc.ExchangeAndResolve(context.Background(), "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_HTTP_UPSTREAM_UNAVAILABLE", infraerrors.Reason(err))
	})
}

// zhipuOAuthAccountRepoStub 记录凭据/extra 的两条写路径，用于证明重登走的是
// 「合并式凭据写 + 运行态标记清除」，而不是账号整体替换。
type zhipuOAuthAccountRepoStub struct {
	AccountRepository
	account     *Account
	getErr      error
	lookupErr   error
	writeErr    error
	extraErr    error
	writes      []map[string]any
	extraWrites []map[string]any
	fullUpdates int
}

func (r *zhipuOAuthAccountRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	if r.lookupErr != nil {
		return nil, r.lookupErr
	}
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.account, nil
}

func (r *zhipuOAuthAccountRepoStub) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	r.writes = append(r.writes, credentials)
	return nil
}

func (r *zhipuOAuthAccountRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.extraErr != nil {
		return r.extraErr
	}
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

// Update 是账号整体替换路径：重登必须永不走到这里（会覆盖 extra 等运行态键）。
func (r *zhipuOAuthAccountRepoStub) Update(_ context.Context, _ *Account) error {
	r.fullUpdates++
	return nil
}

func managedZhipuTestAccount(credentials map[string]any, extra map[string]any) *Account {
	return &Account{
		ID:          42,
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Credentials: credentials,
		Extra:       extra,
	}
}

func TestZhipuOAuthServiceReloginAccount(t *testing.T) {
	const state = "state-abcdef0123456789abcdef0123456789"

	t.Run("merges fresh tokens and clears the relogin flag", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		repo := &zhipuOAuthAccountRepoStub{account: managedZhipuTestAccount(
			map[string]any{
				"auth_flow":     ZhipuLoginAuthFlow,
				"api_key":       "old-key",
				"access_token":  "old-at-token",
				"zcodejwttoken": "old-jwt-token",
				"account_mode":  AccountModeCoding,
				"api_protocol":  APIProtocolAnthropic,
				"custom_key":    "keep-me",
			},
			map[string]any{ZhipuNeedsReloginExtraKey: true, "keep_extra": "yes"},
		)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		err := svc.ReloginAccount(context.Background(), 42, "session-1", state, "auth-code-1")

		require.NoError(t, err)
		require.Len(t, repo.writes, 1, "重登只走一次凭据合并写")
		merged := repo.writes[0]
		require.Equal(t, "key-1.secret-1", merged[zhipuCredentialAPIKey], "api_key 替换为新解析值")
		require.Equal(t, "at-token", merged[zhipuCredentialAccessToken])
		require.Equal(t, "jwt-token", merged[zhipuCredentialZCodeJWT])
		require.Equal(t, "rt-token", merged[zhipuCredentialRefreshToken])
		require.Equal(t, ZhipuLoginAuthFlow, merged[zhipuCredentialAuthFlow])
		// 既有键一个都不能少：account_mode/api_protocol 是管理员配置，自定义键不得清除。
		require.Equal(t, AccountModeCoding, merged[zhipuCredentialAccountMode])
		require.Equal(t, APIProtocolAnthropic, merged[zhipuCredentialAPIProtocol])
		require.Equal(t, "keep-me", merged["custom_key"])
		require.Zero(t, repo.fullUpdates, "禁止整体替换账号（会覆盖 extra 运行态键）")

		require.Equal(t, []map[string]any{{ZhipuNeedsReloginExtraKey: false}}, repo.extraWrites)
		_, stillThere := store.Get("session-1")
		require.False(t, stillThere, "重登成功后会话被消费")
	})

	t.Run("keeps an existing refresh token when the exchange returns none", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(func(req *http.Request) *http.Response {
			if strings.Contains(req.URL.Path, "/oauth/token") {
				return zhipuOAuthTestJSONResponse(http.StatusOK,
					`{"code":0,"data":{"token":"jwt-token","bigmodel":{"access_token":"at-token"}}}`)
			}
			return nil
		})}
		repo := &zhipuOAuthAccountRepoStub{account: managedZhipuTestAccount(
			map[string]any{
				"auth_flow":     ZhipuLoginAuthFlow,
				"api_key":       "old-key",
				"refresh_token": "old-rt-token",
			},
			map[string]any{ZhipuNeedsReloginExtraKey: true},
		)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		err := svc.ReloginAccount(context.Background(), 42, "session-1", state, "auth-code-1")

		require.NoError(t, err)
		require.Len(t, repo.writes, 1)
		require.Equal(t, "old-rt-token", repo.writes[0][zhipuCredentialRefreshToken],
			"新兑换没带 refresh_token 时不得清除既有键")
	})

	t.Run("rejects mismatched state before any write", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		repo := &zhipuOAuthAccountRepoStub{account: managedZhipuTestAccount(
			map[string]any{"auth_flow": ZhipuLoginAuthFlow}, map[string]any{ZhipuNeedsReloginExtraKey: true})}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		err := svc.ReloginAccount(context.Background(), 42, "session-1", "state-other", "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_STATE_MISMATCH", infraerrors.Reason(err))
		require.Empty(t, repo.writes)
		require.Empty(t, repo.extraWrites)
		require.Zero(t, upstream.callCount())
	})

	t.Run("rejects an unknown session before any write", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		repo := &zhipuOAuthAccountRepoStub{account: managedZhipuTestAccount(
			map[string]any{"auth_flow": ZhipuLoginAuthFlow}, map[string]any{ZhipuNeedsReloginExtraKey: true})}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)

		err := svc.ReloginAccount(context.Background(), 42, "session-missing", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_SESSION_NOT_FOUND", infraerrors.Reason(err))
		require.Empty(t, repo.writes)
		require.Empty(t, repo.extraWrites)
		require.Zero(t, upstream.callCount())
	})

	t.Run("reports a missing account", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		repo := &zhipuOAuthAccountRepoStub{getErr: ErrAccountNotFound}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		err := svc.ReloginAccount(context.Background(), 404, "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusNotFound, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_ACCOUNT_NOT_FOUND", infraerrors.Reason(err))
		require.Empty(t, repo.writes)
		require.Zero(t, upstream.callCount())
	})

	t.Run("surfaces a credential write failure without clearing the flag", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		repo := &zhipuOAuthAccountRepoStub{
			account:  managedZhipuTestAccount(map[string]any{"auth_flow": ZhipuLoginAuthFlow}, nil),
			writeErr: errors.New("db down"),
		}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		err := svc.ReloginAccount(context.Background(), 42, "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_CREDENTIALS_PERSIST_FAILED", infraerrors.Reason(err))
		require.Empty(t, repo.extraWrites, "凭据没写成功就不许清标记")
		_, stillThere := store.Get("session-1")
		require.True(t, stillThere, "写入失败时会话保留，管理员可重试")
	})

	t.Run("surfaces a flag-clear failure", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		repo := &zhipuOAuthAccountRepoStub{
			account:  managedZhipuTestAccount(map[string]any{"auth_flow": ZhipuLoginAuthFlow}, nil),
			extraErr: errors.New("db down"),
		}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, repo, nil).WithSessionStore(store)
		zhipuOAuthTestSession(store, "session-1", state, "", time.Time{})

		err := svc.ReloginAccount(context.Background(), 42, "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_NEEDS_RELOGIN_CLEAR_FAILED", infraerrors.Reason(err))
		require.Len(t, repo.writes, 1, "凭据已合并写回")
	})

	t.Run("reports a missing account repository instead of silently succeeding", func(t *testing.T) {
		store := bigmodel.NewSessionStore()
		t.Cleanup(store.Stop)
		upstream := &zhipuOAuthUpstreamStub{respond: zhipuOAuthTestResponder(nil)}
		svc := NewZhipuOAuthService(&zhipuOAuthProxyRepoStub{}, upstream, nil, nil).WithSessionStore(store)

		err := svc.ReloginAccount(context.Background(), 42, "session-1", state, "auth-code-1")

		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, infraerrors.Code(err))
		require.Equal(t, "ZHIPU_OAUTH_ACCOUNT_REPO_UNAVAILABLE", infraerrors.Reason(err))
	})
}
