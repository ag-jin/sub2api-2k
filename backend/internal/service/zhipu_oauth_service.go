package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bigmodel"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ZhipuLoginAuthFlow 是登录托管智谱账号的凭据标记值：platform=zhipu + type=apikey
// + credentials["auth_flow"]="bigmodel_oauth"（design B1）。前端（06/07）逐字对齐。
const ZhipuLoginAuthFlow = "bigmodel_oauth"

// 登录态凭据键（design M1 冻结白名单，键名与 05/06/07 的前端构造逐字一致）。
const (
	zhipuCredentialAuthFlow     = "auth_flow"
	zhipuCredentialAPIKey       = "api_key"
	zhipuCredentialAccessToken  = "access_token"
	zhipuCredentialZCodeJWT     = "zcodejwttoken"
	zhipuCredentialRefreshToken = "refresh_token"
	zhipuCredentialAccountMode  = "account_mode"
	zhipuCredentialAPIProtocol  = "api_protocol"
)

// zhipuLoginPlanLevel 是兑换响应的展示档位。兑换报文只回 token，不回档位；
// 登录链路固定面向 Coding Plan（凭据 account_mode="coding" 是冻结契约），
// 故此处如实标注 "coding"。该字段只出现在 HTTP 响应里，不是凭据键（不落库）。
const zhipuLoginPlanLevel = AccountModeCoding

// ZhipuNeedsReloginExtraKey 是登录态失效的运行态标记（design M2）：探针 401/403 或
// 签名自愈失败后由 keeper / 网关写 accounts.extra，重登成功后由本服务清除。
// 读取方一律判 `== true`（07 徽标、11 快照、30 告警条目同源）。
const ZhipuNeedsReloginExtraKey = "zhipu_needs_relogin"

// ZhipuLoginURLResult 是 GenerateLoginURL 的返回值：登录页地址、会话 id 与
// 回调必须回显的 state（design M1）。
type ZhipuLoginURLResult struct {
	LoginURL  string `json:"login_url"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

// ZhipuLoginCredential 是一次登录兑换 + api_key 解析的产物：托管账号的 wire key
// 与登录态 token（design M1）。RefreshToken 可能为空（兑换报文允许不带）；
// PlanLevel 供管理端展示。
type ZhipuLoginCredential struct {
	APIKey        string `json:"api_key"`
	AccessToken   string `json:"access_token"`
	ZCodeJWTToken string `json:"zcodejwttoken"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	PlanLevel     string `json:"plan_level,omitempty"`
}

// zhipuLoginDefaultRedirectURI 是管理员未提供 redirect_uri 时的回调地址。
// 登录姿势是「打开链接 → 粘贴 authCode」（design M1），redirect 只需在登录页与
// 兑换请求之间往返一致：会话里存的就是这个生效值，兑换时原样重放。
// 取值对齐 research FINAL-REPORT §六 观测到的本机回调。
const zhipuLoginDefaultRedirectURI = "http://127.0.0.1:53633/oauth/callback/bigmodel"

// ZhipuOAuthService 编排智谱登录全流程（design M1）：授权 URL、兑换、API key
// 自动解析、建号凭据构造、重登。不负责运行期健康（M2）与转发（既有网关）。
//
// 依赖均为既有接缝：sessions 是进程内 10 分钟 TTL 会话存储（跨实例 Redis 化见 P4）；
// proxyRepo 解析登录链接使用的代理；httpUpstream 是唯一的出站 HTTP 通道
// （bigmodel.cn 管理面直连，zcode.z.ai 兑换走其上的 RiskClient 风控闸）；
// accountRepo 供重登读取既有凭据并按账号级 JSONB 合并写回（design M1）。
type ZhipuOAuthService struct {
	sessions     *bigmodel.SessionStore
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	accountRepo  AccountRepository
	cfg          *config.Config

	// riskMu/riskClients 按代理缓存风控客户端：zcode.z.ai 的最小调用间隔与
	// 3012/3001/429 负缓存必须跨多次管理操作累积，重建客户端会把这些闸门清零。
	riskMu      sync.Mutex
	riskClients map[string]*bigmodel.RiskClient
}

// NewZhipuOAuthService 构造登录编排服务；会话存储由 WithSessionStore 覆盖（测试接缝）。
func NewZhipuOAuthService(
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	accountRepo AccountRepository,
	cfg *config.Config,
) *ZhipuOAuthService {
	return &ZhipuOAuthService{
		sessions:     bigmodel.NewSessionStore(),
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		accountRepo:  accountRepo,
		cfg:          cfg,
	}
}

// WithSessionStore 替换内存会话存储（跨实例 Redis 化与测试注入的同一接缝，
// 与 GrokOAuthService.WithSessionStore 同形）。
func (s *ZhipuOAuthService) WithSessionStore(store *bigmodel.SessionStore) *ZhipuOAuthService {
	if s != nil && store != nil {
		if s.sessions != nil {
			s.sessions.Stop()
		}
		s.sessions = store
	}
	return s
}

// Stop 停止会话存储的过期清扫。
func (s *ZhipuOAuthService) Stop() {
	if s == nil || s.sessions == nil {
		return
	}
	s.sessions.Stop()
}

// BuildAccountCredentials 把一次登录的产物构造为落库凭据（design M1 冻结白名单）：
// auth_flow="bigmodel_oauth"、api_key、access_token、zcodejwttoken、account_mode="coding"、
// api_protocol；refresh_token 仅在非空时写入。键集合固定——06/07 前端按同一集合构造，
// 键名漂移会静默产生「登录了但转发取不到 key」。
func (s *ZhipuOAuthService) BuildAccountCredentials(cred *ZhipuLoginCredential, protocol string) map[string]any {
	credentials := map[string]any{
		zhipuCredentialAuthFlow:    ZhipuLoginAuthFlow,
		zhipuCredentialAPIKey:      "",
		zhipuCredentialAccessToken: "",
		zhipuCredentialZCodeJWT:    "",
		zhipuCredentialAccountMode: AccountModeCoding,
		zhipuCredentialAPIProtocol: protocol,
	}
	if cred != nil {
		credentials[zhipuCredentialAPIKey] = cred.APIKey
		credentials[zhipuCredentialAccessToken] = cred.AccessToken
		credentials[zhipuCredentialZCodeJWT] = cred.ZCodeJWTToken
		if cred.RefreshToken != "" {
			credentials[zhipuCredentialRefreshToken] = cred.RefreshToken
		}
	}
	return credentials
}

// GenerateLoginURL 生成一次登录的授权地址并把 state/redirect/代理绑定进会话：
// 返回的 state 与 session_id 一一对应地写入 SessionStore，回调必须回显该 state。
func (s *ZhipuOAuthService) GenerateLoginURL(ctx context.Context, proxyID *int64, redirectURI string) (*ZhipuLoginURLResult, error) {
	state, err := bigmodel.GenerateState()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError,
			"ZHIPU_OAUTH_STATE_FAILED", "failed to generate login state: %v", err)
	}
	// session_id 与 state 是两次独立抽取的不透明随机值（32 hex）；state 是回调
	// 必须回显的 CSRF 值，session_id 只是会话索引。
	sessionID, err := bigmodel.GenerateState()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError,
			"ZHIPU_OAUTH_SESSION_FAILED", "failed to generate session id: %v", err)
	}

	proxyURL, err := s.proxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}

	effectiveRedirect := zhipuEffectiveRedirectURI(redirectURI)
	s.sessions.Set(sessionID, &bigmodel.OAuthSession{
		State:       state,
		RedirectURI: effectiveRedirect,
		ProxyURL:    proxyURL,
	})

	return &ZhipuLoginURLResult{
		LoginURL:  bigmodel.BuildLoginURL(effectiveRedirect, state),
		SessionID: sessionID,
		State:     state,
	}, nil
}

// zhipuEffectiveRedirectURI 归一回调地址：留空时用本机回调，保证登录页与兑换
// 请求携带同一个值。
func zhipuEffectiveRedirectURI(override string) string {
	trimmed := strings.TrimSpace(override)
	if trimmed == "" {
		return zhipuLoginDefaultRedirectURI
	}
	return trimmed
}

// ExchangeAndResolve 校验会话与 state 后兑换授权码并解析 wire api_key（design M1）。
// 会话缺失/过期、state 不符都在调用上游之前拒绝；兑换成功后会话被消费
// （授权码一次性，不可二次兑换）。
func (s *ZhipuOAuthService) ExchangeAndResolve(ctx context.Context, sessionID, state, authCode string) (*ZhipuLoginCredential, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(authCode) == "" {
		return nil, infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_INVALID_INPUT", "session_id and auth_code are required")
	}

	session, err := s.authorizeSession(sessionID, state)
	if err != nil {
		return nil, err
	}

	credential, err := s.exchangeAndResolve(ctx, session, authCode)
	if err != nil {
		return nil, err
	}
	s.sessions.Delete(sessionID)
	return credential, nil
}

// authorizeSession 取出会话并校验 state。会话索引错误（不存在或过期）与 state
// 不符返回不同错误码，两者都绝不触碰上游：zcode.z.ai 不因猜测被调用。
func (s *ZhipuOAuthService) authorizeSession(sessionID, state string) (*bigmodel.OAuthSession, error) {
	session, ok := s.sessions.Get(sessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_SESSION_NOT_FOUND", "login session not found or expired")
	}
	if trimmed := strings.TrimSpace(state); trimmed == "" || trimmed != session.State {
		return nil, infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_STATE_MISMATCH", "state does not match the login session")
	}
	return session, nil
}

// exchangeAndResolve 在已校验的会话上执行「兑换 → 解析 api_key」。两段必须都成功：
// wire 层用 api_key 转发（design B1），只有 token 的登录产物建不出可用账号。
func (s *ZhipuOAuthService) exchangeAndResolve(
	ctx context.Context, session *bigmodel.OAuthSession, authCode string,
) (*ZhipuLoginCredential, error) {
	risk, err := s.riskClientFor(session.ProxyURL)
	if err != nil {
		return nil, err
	}

	token, err := bigmodel.ExchangeCode(ctx, risk, "", authCode, session.RedirectURI, session.State)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway,
			"ZHIPU_OAUTH_EXCHANGE_FAILED", "authorization-code exchange failed").WithCause(err)
	}

	keyID, secret, err := bigmodel.ResolveZCodeAPIKey(ctx, s.doerFor(session.ProxyURL), token.AccessToken)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway,
			"ZHIPU_OAUTH_APIKEY_RESOLVE_FAILED", "zcode api key resolution failed").WithCause(err)
	}

	return &ZhipuLoginCredential{
		APIKey:        zhipuWireAPIKey(keyID, secret),
		AccessToken:   token.AccessToken,
		ZCodeJWTToken: token.ZCodeJWTToken,
		RefreshToken:  token.RefreshToken,
		PlanLevel:     zhipuLoginPlanLevel,
	}, nil
}

// zhipuWireAPIKey 拼出 wire 层使用的智谱 key：`id.secret`（research §六）。
// copy 端点允许不返回 secretKey，此时裸 id 就是凭据。
func zhipuWireAPIKey(keyID, secret string) string {
	if secret == "" {
		return keyID
	}
	return keyID + "." + secret
}

// doerFor 把 HTTPUpstream 适配成 bigmodel 需要的单参数 doer，固定本次登录的代理
// 与账号占位参数（登录链路没有账号上下文）。这是登录链路唯一的出站通道。
func (s *ZhipuOAuthService) doerFor(proxyURL string) bigmodel.HTTPDoer {
	return zhipuHTTPDoer{upstream: s.httpUpstream, proxyURL: proxyURL}
}

// zhipuHTTPDoer 是 HTTPUpstream → bigmodel.HTTPDoer 的适配器。
type zhipuHTTPDoer struct {
	upstream HTTPUpstream
	proxyURL string
}

func (d zhipuHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	return d.upstream.Do(req, d.proxyURL, 0, 0)
}

// riskClientFor 返回该代理对应的风控客户端（首次使用时构造）。
func (s *ZhipuOAuthService) riskClientFor(proxyURL string) (*bigmodel.RiskClient, error) {
	if s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError,
			"ZHIPU_OAUTH_HTTP_UPSTREAM_UNAVAILABLE", "http upstream is not configured")
	}

	s.riskMu.Lock()
	defer s.riskMu.Unlock()
	if client, ok := s.riskClients[proxyURL]; ok {
		return client, nil
	}
	if s.riskClients == nil {
		s.riskClients = make(map[string]*bigmodel.RiskClient)
	}
	client := bigmodel.NewRiskClient(s.doerFor(proxyURL), s.zcodeMinCallInterval())
	s.riskClients[proxyURL] = client
	return client, nil
}

// zcodeMinCallInterval 取 zcode.z.ai 风控端点的全局最小调用间隔
// （gateway.zhipu.zcode_min_call_interval_seconds，0 = 禁用节流）。
// 未注入配置时按 design M1 的默认值 30s 兜底。
func (s *ZhipuOAuthService) zcodeMinCallInterval() time.Duration {
	if s.cfg == nil {
		return 30 * time.Second
	}
	seconds := s.cfg.Gateway.Zhipu.ZCodeMinCallIntervalSeconds
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// ReloginAccount 执行一次管理端重登（design M1/B2）：兑换 + 重解析 api_key →
// 合并式写回既有凭据（复用账号仓储的凭据写路径，既有键一个不清除）→
// 清 extra 的运行态失效标记。任一步失败即整体失败，不产生半写状态。
func (s *ZhipuOAuthService) ReloginAccount(ctx context.Context, accountID int64, sessionID, state, authCode string) error {
	if s.accountRepo == nil {
		return infraerrors.New(http.StatusInternalServerError,
			"ZHIPU_OAUTH_ACCOUNT_REPO_UNAVAILABLE", "account repository is not configured")
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(authCode) == "" {
		return infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_INVALID_INPUT", "session_id and auth_code are required")
	}

	session, err := s.authorizeSession(sessionID, state)
	if err != nil {
		return err
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return infraerrors.New(http.StatusNotFound, "ZHIPU_OAUTH_ACCOUNT_NOT_FOUND", "account not found")
		}
		return infraerrors.New(http.StatusServiceUnavailable,
			"ZHIPU_OAUTH_ACCOUNT_LOOKUP_FAILED", "account lookup is temporarily unavailable").WithCause(err)
	}
	if account == nil {
		return infraerrors.New(http.StatusNotFound, "ZHIPU_OAUTH_ACCOUNT_NOT_FOUND", "account not found")
	}

	credential, err := s.exchangeAndResolve(ctx, session, authCode)
	if err != nil {
		return err
	}

	merged := shallowCopyMap(account.Credentials)
	for key, value := range zhipuReloginCredentialUpdates(credential) {
		merged[key] = value
	}
	if err := persistAccountCredentials(ctx, s.accountRepo, account, merged); err != nil {
		return infraerrors.New(http.StatusInternalServerError,
			"ZHIPU_OAUTH_CREDENTIALS_PERSIST_FAILED", "failed to persist relogin credentials").WithCause(err)
	}

	// 标记按布尔语义清除：仓储的 extra 写是 JSONB key 级合并（不支持删键），
	// 而所有读取方都判 `== true`，写 false 即「已恢复」的等价表达。
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{ZhipuNeedsReloginExtraKey: false}); err != nil {
		return infraerrors.New(http.StatusInternalServerError,
			"ZHIPU_OAUTH_NEEDS_RELOGIN_CLEAR_FAILED", "failed to clear the relogin flag").WithCause(err)
	}

	s.sessions.Delete(sessionID)
	return nil
}

// zhipuReloginCredentialUpdates 是重登写回的键集合：只覆盖登录态标记、wire api_key
// 与三个 token。account_mode / api_protocol 与其它自定义键属管理员配置，合并式保留
// （design M1 风险项：重登覆盖写丢失既有配置）。refresh_token 只在本次兑换带回
// 非空值时才覆盖，避免用空值抹掉既有键。
func zhipuReloginCredentialUpdates(cred *ZhipuLoginCredential) map[string]any {
	updates := map[string]any{
		zhipuCredentialAuthFlow:    ZhipuLoginAuthFlow,
		zhipuCredentialAPIKey:      cred.APIKey,
		zhipuCredentialAccessToken: cred.AccessToken,
		zhipuCredentialZCodeJWT:    cred.ZCodeJWTToken,
	}
	if cred.RefreshToken != "" {
		updates[zhipuCredentialRefreshToken] = cred.RefreshToken
	}
	return updates
}

// proxyURL 解析登录链路使用的代理（与 GrokOAuthService.proxyURL 同形）。
func (s *ZhipuOAuthService) proxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	if s.proxyRepo == nil {
		return "", infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_PROXY_NOT_AVAILABLE", "proxy repository is not available")
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		if errors.Is(err, ErrProxyNotFound) {
			return "", infraerrors.New(http.StatusBadRequest,
				"ZHIPU_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
		}
		return "", infraerrors.New(http.StatusServiceUnavailable,
			"ZHIPU_OAUTH_PROXY_LOOKUP_FAILED", "proxy lookup is temporarily unavailable")
	}
	if proxy == nil {
		return "", infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
	}
	return proxy.URL(), nil
}
