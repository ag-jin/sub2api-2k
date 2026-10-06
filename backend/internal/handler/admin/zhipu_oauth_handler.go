package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ZhipuOAuthHandler 暴露智谱（bigmodel）登录链路的管理端点（design M1 / 票 04）：
// 生成登录链接、兑换授权码、登录建号、重登。
//
// 鉴权不是本文件的职责：四条路由一律挂在 routes/admin.go 的 admin 分组下，
// 由既有管理端中间件（adminAuth + 面板限流 + 审计 + 合规守卫）保护（R 风险项）。
// R0：本文件与登记的路由都不存在任何重置卡「使用」入口。
type ZhipuOAuthHandler struct {
	zhipuOAuthService *service.ZhipuOAuthService
	adminService      service.AdminService
}

func NewZhipuOAuthHandler(
	zhipuOAuthService *service.ZhipuOAuthService,
	adminService service.AdminService,
) *ZhipuOAuthHandler {
	return &ZhipuOAuthHandler{
		zhipuOAuthService: zhipuOAuthService,
		adminService:      adminService,
	}
}

// ZhipuLoginURLRequest 是登录链接请求体：两个字段都可省（前端 05 逐字对齐）。
type ZhipuLoginURLRequest struct {
	ProxyID     *int64 `json:"proxy_id"`
	RedirectURI string `json:"redirect_uri"`
}

// GenerateLoginURL 生成一次登录的授权地址并把 state/session 登记进会话存储。
func (h *ZhipuOAuthHandler) GenerateLoginURL(c *gin.Context) {
	var req ZhipuLoginURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// 空 body 合法：等价于「不指定代理 + 使用默认回调」。
		req = ZhipuLoginURLRequest{}
	}
	result, err := h.zhipuOAuthService.GenerateLoginURL(c.Request.Context(), req.ProxyID, req.RedirectURI)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// ZhipuExchangeRequest 是兑换与重登共用的请求体（前端 05 的 ZhipuExchangeRequest
// 逐字对齐：session_id / state / auth_code）。
type ZhipuExchangeRequest struct {
	SessionID string `json:"session_id" binding:"required"`
	State     string `json:"state"`
	AuthCode  string `json:"auth_code" binding:"required"`
}

// Exchange 用授权码兑换登录态 token 并解析 wire api_key（一次性：成功后会话被消费）。
func (h *ZhipuOAuthHandler) Exchange(c *gin.Context) {
	var req ZhipuExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	credential, err := h.zhipuOAuthService.ExchangeAndResolve(c.Request.Context(), req.SessionID, req.State, req.AuthCode)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, credential)
}

// ZhipuCreateAccountFromLoginRequest 是登录建号请求体：与 POST /admin/accounts 同形状
// （前端 06 直接提交 CreateAccountRequest）。platform/type 不接受请求覆盖：
// 登录托管的智谱账号恒为 platform=zhipu + type=apikey。extra /
// confirm_mixed_channel_risk / upstream_billing_probe_enabled 不在登录建号契约内
// （06 不发送），刻意不声明、不落库。
type ZhipuCreateAccountFromLoginRequest struct {
	Name               string         `json:"name"`
	Notes              *string        `json:"notes"`
	Credentials        map[string]any `json:"credentials" binding:"required"`
	ProxyID            *int64         `json:"proxy_id"`
	Concurrency        int            `json:"concurrency"`
	LoadFactor         *int           `json:"load_factor"`
	Priority           int            `json:"priority"`
	RateMultiplier     *float64       `json:"rate_multiplier"`
	GroupIDs           []int64        `json:"group_ids"`
	ExpiresAt          *int64         `json:"expires_at"`
	AutoPauseOnExpired *bool          `json:"auto_pause_on_expired"`
}

// zhipuLoginAccountName 是登录建号未指定名称时的兜底账号名。
const zhipuLoginAccountName = "Zhipu Login Account"

// CreateAccountFromLogin 复用既有建号链路（admin_account.CreateAccount）把一次登录的
// 产物落成托管账号：extra 归一 / 默认分组 zhipu-default / 凭据净化 / BindGroups 全部继承。
// 凭据一律经 BuildAccountCredentials 重建 03 冻结白名单——请求里的额外键（含
// password/cookie 等敏感明文）全部丢弃；缺 wire api_key 的产物直接拒绝，不建出不可用账号。
func (h *ZhipuOAuthHandler) CreateAccountFromLogin(c *gin.Context) {
	var req ZhipuCreateAccountFromLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	credential := zhipuLoginCredentialFromRequest(req.Credentials)
	if strings.TrimSpace(credential.APIKey) == "" {
		response.ErrorFrom(c, infraerrors.New(http.StatusBadRequest,
			"ZHIPU_OAUTH_INVALID_INPUT", "api_key is required in the login credentials"))
		return
	}
	credentials := h.zhipuOAuthService.BuildAccountCredentials(
		credential, zhipuRequestCredentialString(req.Credentials, zhipuRequestAPIProtocolKey))

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = zhipuLoginAccountName
	}
	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:               name,
		Notes:              req.Notes,
		Platform:           service.PlatformZhipu,
		Type:               service.AccountTypeAPIKey,
		Credentials:        credentials,
		ProxyID:            req.ProxyID,
		Concurrency:        req.Concurrency,
		LoadFactor:         req.LoadFactor,
		Priority:           req.Priority,
		RateMultiplier:     req.RateMultiplier,
		GroupIDs:           append([]int64(nil), req.GroupIDs...),
		ExpiresAt:          req.ExpiresAt,
		AutoPauseOnExpired: req.AutoPauseOnExpired,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromService(account))
}

// 登录请求 credentials 里唯一被读取的键：wire key 与三个 token 是登录产物，
// api_protocol 是管理员的协议选择。其余键（auth_flow/account_mode/任意自定义键）
// 统统由服务端重建，请求不可覆盖。
const (
	zhipuRequestAPIKeyKey       = "api_key"
	zhipuRequestAccessTokenKey  = "access_token"
	zhipuRequestZCodeJWTKey     = "zcodejwttoken"
	zhipuRequestRefreshTokenKey = "refresh_token"
	zhipuRequestAPIProtocolKey  = "api_protocol"
)

// zhipuLoginCredentialFromRequest 把请求 credentials 收敛为登录产物结构体。
func zhipuLoginCredentialFromRequest(credentials map[string]any) *service.ZhipuLoginCredential {
	return &service.ZhipuLoginCredential{
		APIKey:        zhipuRequestCredentialString(credentials, zhipuRequestAPIKeyKey),
		AccessToken:   zhipuRequestCredentialString(credentials, zhipuRequestAccessTokenKey),
		ZCodeJWTToken: zhipuRequestCredentialString(credentials, zhipuRequestZCodeJWTKey),
		RefreshToken:  zhipuRequestCredentialString(credentials, zhipuRequestRefreshTokenKey),
	}
}

func zhipuRequestCredentialString(credentials map[string]any, key string) string {
	value, ok := credentials[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// ReloginAccount 对既有托管账号执行一次重登（design M1/B2）：兑换 + 重解析 wire api_key
// → 合并式写回凭据（不清除既有键）→ 清除 zhipu_needs_relogin 标记。账号级语义
// （存在性、状态、凭据合并）全部由 ZhipuOAuthService.ReloginAccount 持有。
func (h *ZhipuOAuthHandler) ReloginAccount(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req ZhipuExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.zhipuOAuthService.ReloginAccount(c.Request.Context(), accountID, req.SessionID, req.State, req.AuthCode); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, nil)
}
