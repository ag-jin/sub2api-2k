package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// codeBuddyRefreshAuthError 是 CodeBuddy 刷新端点返回的确认性凭证拒绝
// （401/403、grant 失效类）。经 isNonRetryableRefreshError 判定后账号进入
// StatusError，提示管理员重新粘贴 auth JSON；网络/5xx 等瞬态错误不使用该类型。
type codeBuddyRefreshAuthError struct {
	status  int
	message string
}

func newCodeBuddyRefreshAuthError(status int, message string) *codeBuddyRefreshAuthError {
	return &codeBuddyRefreshAuthError{status: status, message: message}
}

func (e *codeBuddyRefreshAuthError) Error() string {
	msg := errCodeBuddyRefreshRejected.Error()
	if logredactSuffix := logredact.RedactText(e.message); strings.TrimSpace(logredactSuffix) != "" {
		msg += ": " + logredactSuffix
	}
	return fmt.Sprintf("%s (HTTP %d)", msg, e.status)
}

// Unwrap 保底旧文本判定链路（兼容 isNonRetryableRefreshError 的字符串匹配）。
func (e *codeBuddyRefreshAuthError) Unwrap() error { return errCodeBuddyRefreshRejected }

// errCodeBuddyRefreshRejected 保持向后兼容的错误标识（InvalidGrant race 恢复等
// 文本匹配会命中 "invalid" 不成立；此 sentinel 仅语义命名）。
var errCodeBuddyRefreshRejected = errors.New("codebuddy refresh credentials rejected: invalid_refresh_token (re-import auth JSON)")

// CodeBuddyTokenRefresher 处理 CodeBuddy 平台 auth JSON 账号的 token 轮换。
// 注册进 TokenRefreshService 框架（对齐 grok_token_refresher）：复用
// OAuthRefreshAPI 的 CacheKey 分布锁（多副本防击穿）、刷新前 DB reread、
// CAS-if-unchanged 持久化、瞬态重试耗尽 → temp-unsched、确认性 401/403 → StatusError。
type CodeBuddyTokenRefresher struct {
	client *http.Client
	// testBaseURL 仅供测试注入 httptest 端点；生产恒为空。
	testBaseURL string
}

// NewCodeBuddyTokenRefresher 创建 CodeBuddy token 刷新器。
func NewCodeBuddyTokenRefresher() *CodeBuddyTokenRefresher {
	return &CodeBuddyTokenRefresher{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// CodeBuddyTokenCacheKey 生成 CodeBuddy 账号的刷新分布锁缓存键。
// 格式: "codebuddy:account:{account_id}"
func CodeBuddyTokenCacheKey(account *Account) string {
	return "codebuddy:account:" + strconv.FormatInt(account.ID, 10)
}

// CacheKey 返回用于分布式锁的缓存键（OAuthRefreshExecutor 接口）。
func (r *CodeBuddyTokenRefresher) CacheKey(account *Account) string {
	return CodeBuddyTokenCacheKey(account)
}

// CanRefresh 报告账号是否能由本刷新器处理：codebuddy 的 auth JSON 账号为
// AccountTypeAPIKey（非 OAuth），凭是否存在 refresh_token 判定。
func (r *CodeBuddyTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.IsCodeBuddy() &&
		strings.TrimSpace(account.GetCodeBuddyRefreshToken()) != ""
}

// NeedsRefresh 判断刷新是否需要：auth.expiresAt（毫秒存储为 RFC3339）在刷新
// 窗口内、或 access_token 缺失 / 无过期信息（保守触发一次，成功后即写入）。
// 上游提前 60s 判过期：auth JSON expiresAt 毫秒值已含该语义余量，
// 由 AuthManager 提前量处理，这里按 expires_at 直接判定。
func (r *CodeBuddyTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil || strings.TrimSpace(account.GetCodeBuddyRefreshToken()) == "" {
		return false
	}
	if strings.TrimSpace(account.GetCodeBuddyAccessToken()) == "" {
		return true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	return time.Until(*expiresAt) < refreshWindow
}

// Refresh 执行 token 刷新（OAuthRefreshAPI 已在锁内完成 DB reread 与二次判期）。
// 端点 POST {base}/v2/plugin/auth/token/refresh，头 X-Refresh-Token +
// X-Auth-Refresh-Source: plugin，body {}，响应 code==0 data 为新 auth。
// 返回写回全字段：accessToken / refreshToken（轮换）/
// expiresAt（expiresIn*1000 → RFC3339）/ refreshExpiresAt /
// lastRefreshTime（毫秒）/ domain 继承（响应缺失时沿用刷新前值）。
func (r *CodeBuddyTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("codebuddy token refresher unconfigured")
	}
	baseURL := strings.TrimRight(account.GetOpenAIBaseURL(), "/")
	if r.testBaseURL != "" {
		baseURL = strings.TrimRight(r.testBaseURL, "/")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("codebuddy refresh: base URL unavailable for account %d", account.ID)
	}
	refreshURL := baseURL + codeBuddyTokenRefreshPath

	data, err := r.callRefreshEndpoint(ctx, account, refreshURL)
	if err != nil {
		return nil, err
	}
	nowMs := time.Now().UnixMilli()
	newCreds, err := BuildCodeBuddyRefreshedCredentials(nowMs, account.GetCodeBuddyDomain(), data)
	if err != nil {
		return nil, err
	}
	return MergeCredentials(account.Credentials, newCreds), nil
}

// callRefreshEndpoint 调上游刷新端点。
//   - body 恒 {}（原型实证）；
//   - 请求头保留鉴权身份集 + X-Refresh-Token + X-Auth-Refresh-Source: plugin；
//   - HTTP 401/403 或 code!=0 且 msg 含拒绝/失效语义 → errCodeBuddyRefreshRejected
//     (不可重试,账号进 StatusError);网络/5xx 等瞬态错误原样带出。
func (r *CodeBuddyTokenRefresher) callRefreshEndpoint(ctx context.Context, account *Account, refreshURL string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codeBuddyUpstreamUserAgent)
	req.Header.Set("Authorization", "Bearer "+account.GetCodeBuddyAccessToken())
	for k, v := range BuildCodeBuddyUpstreamHeaders(account) {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Refresh-Token", account.GetCodeBuddyRefreshToken())
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codebuddy read refresh response: %w", err)
	}

	msg := firstNonEmpty(
		gjson.GetBytes(body, "msg").String(),
		gjson.GetBytes(body, "message").String(),
	)
	codeValue := gjson.GetBytes(body, "code")

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		logger.L().Warn("codebuddy token refresh rejected",
			zap.Int64("account_id", account.ID),
			zap.Int("status", resp.StatusCode),
			zap.String("msg", logredact.RedactText(msg)),
		)
		return nil, newCodeBuddyRefreshAuthError(resp.StatusCode, msg)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 5xx / 429 视为瞬态（走 TokenRefreshService 重试与 temp-unsched 链）。
		return nil, fmt.Errorf("codebuddy refresh transient HTTP %d: %s",
			resp.StatusCode, logredact.RedactText(truncateString(string(body), 256)))
	}
	if codeValue.Exists() && codeValue.Int() != 0 {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "expired") || strings.Contains(lower, "invalid") ||
			strings.Contains(lower, "grant") || strings.Contains(lower, "token") {
			return nil, newCodeBuddyRefreshAuthError(resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("codebuddy refresh failed: code %d %s", codeValue.Int(),
			CodeBuddyBizCodeMessage(int(codeValue.Int()), logredact.RedactText(msg))) // 管理面日志附业务码说明（A4）
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsObject() {
		return nil, fmt.Errorf("codebuddy refresh response missing data")
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(data.Raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse codebuddy refresh data: %w", err)
	}
	return parsed, nil
}
