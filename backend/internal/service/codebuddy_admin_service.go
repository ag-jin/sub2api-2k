package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	logredact "github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// CodeBuddy 后台扫码纳管（A1）与每日签到（A3）服务。
//
// 上游协议事实（absorb-verify.md V1/V2/V4 + 只读参考实现 workbuddy-manager
// server/services/tencent.py）：
//   - 扫码发起：POST {qrBase}/v2/plugin/auth/state?platform=CLI，body {}，无鉴权
//     → 信封 code=0，data.state / data.authUrl；
//   - 扫码轮询：GET {qrBase}/v2/plugin/auth/token?state=，未扫码 code=11217
//     （HTTP 200，"未扫码"判定以信封 code 为准）；成功 data 含
//     accessToken/refreshToken/expiresIn(秒)/domain；
//   - 账号信息：GET {qrBase}/v2/plugin/login/account?state=（Bearer）→
//     uid/nickname/enterpriseId；
//   - 签到：POST TENCENT_BILLING_BASE+v2/billing/meter/daily-checkin，body {}。
//
// 安全语义（设计 R1 全裁决）：state 无鉴权是上游固有属性（QR 被第三方扫走 =
// 入库"扫码者账号"，后台渲染+审计可查）；poll 只答发起者（他人 404 不区分态）；
// per-state 节流 ≥2s；每 actor 并发 state ≤2；取到 token 当刻即焚 state。
const (
	codeBuddyQRStatePath   = "/v2/plugin/auth/state"
	codeBuddyQRTokenPath   = "/v2/plugin/auth/token"
	codeBuddyQRAccountPath = "/v2/plugin/login/account"

	codeBuddyQRStateTTL        = 300 * time.Second
	codeBuddyQRPollMinInterval = 2 * time.Second
	codeBuddyQRActorLimit      = 2
)

// CodeBuddyAdminService 管理面 CodeBuddy 专属操作（扫码纳管 / 每日签到）。
type CodeBuddyAdminService struct {
	admin       AdminService
	accountRepo AccountRepository
	qrStore     codebuddy.Store
	qrFlight    singleflight.Group // 上游 token 调用 single-flight（per-state）
	// qrBase 扫码流程上游 base（chat 网关同域 copilot.tencent.com）；
	// qr 无账号上下文 → plain client + testBaseURL 注入字段（A6）。
	qrBase      string
	testBaseURL string
	client      *http.Client
	// creditsFetcher 解冻闭环（4.5）用：签到成功后查一次实时积分，判是否解除
	// 积分耗尽冷却。可空 —— 未注入时解冻判定整体跳过（宁可不解除，不误解除）。
	creditsFetcher *CodeBuddyCreditsFetcher
}

func NewCodeBuddyAdminService(
	admin AdminService,
	accountRepo AccountRepository,
	qrStore codebuddy.Store,
) *CodeBuddyAdminService {
	return &CodeBuddyAdminService{
		admin:       admin,
		accountRepo: accountRepo,
		qrStore:     qrStore,
		qrBase:      "https://copilot.tencent.com",
		client:      &http.Client{Timeout: 15 * time.Second},
	}
}

// WithTestBaseURL 注入 httptest 端点覆盖 qrBase（测试专用；生产恒为空）。
func (s *CodeBuddyAdminService) WithTestBaseURL(baseURL string) *CodeBuddyAdminService {
	s.testBaseURL = strings.TrimRight(baseURL, "/")
	return s
}

// SetCreditsFetcher 注入积分查询器（解冻闭环 4.5 用，A2 同款追加式 DI）。
func (s *CodeBuddyAdminService) SetCreditsFetcher(fetcher *CodeBuddyCreditsFetcher) {
	if s == nil {
		return
	}
	s.creditsFetcher = fetcher
}

func (s *CodeBuddyAdminService) qrBaseURL() string {
	if s.testBaseURL != "" {
		return s.testBaseURL
	}
	return s.qrBase
}

// --- A1 扫码纳管 ---

// CodeBuddyQRStartResult qr/start 响应（冻结白名单 {state, authUrl}）。
type CodeBuddyQRStartResult struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// Start 发起扫码：调上游取 state，登记发起者绑定（每 actor 并发 ≤2）。
func (s *CodeBuddyAdminService) Start(ctx context.Context, actorID string) (*CodeBuddyQRStartResult, error) {
	if s == nil || s.qrStore == nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_QR_STORE_UNAVAILABLE", "codebuddy qr state store not configured")
	}
	count, err := s.qrStore.CountActorStates(ctx, actorID)
	if err != nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_QR_STORE_ERROR", "codebuddy qr state store error")
	}
	if count >= codeBuddyQRActorLimit {
		return nil, infraerrors.TooManyRequests("CODEBUDDY_QR_ACTOR_LIMIT", "operator has too many pending qr states (limit 2)")
	}

	raw, err := s.upstreamCall(ctx, http.MethodPost, s.qrBaseURL()+codeBuddyQRStatePath+"?platform=CLI", "{}", "")
	if err != nil {
		return nil, err
	}
	if codebuddyEnvelopeCode(raw) != 0 {
		return nil, codebuddyUpstreamBizError(raw)
	}
	state := strings.TrimSpace(gjson.GetBytes(raw, "data.state").String())
	authURL := strings.TrimSpace(gjson.GetBytes(raw, "data.authUrl").String())
	if state == "" {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_QR_STATE_MISSING", "upstream qr state missing")
	}
	rec := codebuddy.StateRecord{
		ActorID:   actorID,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.qrStore.Create(ctx, state, rec); err != nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_QR_STORE_ERROR", "codebuddy qr state store error")
	}
	return &CodeBuddyQRStartResult{State: state, AuthURL: authURL}, nil
}

// --- qr/poll 响应（冻结白名单）---
// {status: waiting|ok|expired|error, account?: {id, uid, nickname, created, updated},
//  error?: {code, msg}（上游错误仅 code+msg，永不含凭据）}

// CodeBuddyQRPollResult qr/poll 响应。
type CodeBuddyQRPollResult struct {
	Status  string                     `json:"status"`
	Account *CodeBuddyQRAccountSummary `json:"account,omitempty"`
	Error   *CodeBuddyQREnvelopeError  `json:"error,omitempty"`
}

// CodeBuddyQRAccountSummary 账号摘要（响应白名单字段）。
type CodeBuddyQRAccountSummary struct {
	ID       int64  `json:"id"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Created  bool   `json:"created"`
	Updated  bool   `json:"updated"`
}

// CodeBuddyQREnvelopeError 上游信封错误（仅 code+msg）。
type CodeBuddyQREnvelopeError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// Poll 轮询扫码结果：绑定发起者（他人 404 不区分态）；per-state ≥2s 节流；
// createdAt+300s → expired 且焚毁；信封 code=11217 → waiting；code=0 且
// accessToken 在场 → 当刻焚毁 state → single-flight（取账号信息 + CAS upsert）；
// 其他码 → status=error 透传 code+msg（state 保留，客户端可重试）。
func (s *CodeBuddyAdminService) Poll(ctx context.Context, actorID, state string) (*CodeBuddyQRPollResult, error) {
	if s == nil || s.qrStore == nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_QR_STORE_UNAVAILABLE", "codebuddy qr state store not configured")
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, infraerrors.NotFound("CODEBUDDY_QR_STATE_UNKNOWN", "qr state unknown")
	}
	rec, loadErr := s.qrStore.Load(ctx, state)
	if loadErr != nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_QR_STORE_ERROR", "codebuddy qr state store error")
	}
	// 你人 404 不区分态（未知/过期/他人均同响应）。
	if rec == nil || rec.ActorID != actorID {
		return nil, infraerrors.NotFound("CODEBUDDY_QR_STATE_UNKNOWN", "qr state unknown")
	}

	now := time.Now()
	if now.UnixMilli()-rec.CreatedAt >= codeBuddyQRStateTTL.Milliseconds() {
		// 过期即焚（含发起者活跃集合登记）。
		_ = s.qrStore.Drop(ctx, state)
		return &CodeBuddyQRPollResult{Status: "expired"}, nil
	}
	if rec.LastPolledAt > 0 && now.UnixMilli()-rec.LastPolledAt < codeBuddyQRPollMinInterval.Milliseconds() {
		return nil, infraerrors.TooManyRequests("CODEBUDDY_QR_POLL_THROTTLED", "poll interval too short (min 2s)")
	}
	// lastPolledAt 记账：保持剩余 TTL（ouncill 刷新由逻辑过期控制）。
	remaining := time.Until(time.UnixMilli(rec.CreatedAt).Add(codebuddy.RecordTTL))
	if remaining > 0 {
		rec.LastPolledAt = now.UnixMilli()
		_ = s.qrStore.Save(ctx, state, *rec, remaining)
	}

	raw, err := s.upstreamCall(ctx, http.MethodGet, s.qrBaseURL()+codeBuddyQRTokenPath+"?state="+state, "", "")
	if err != nil {
		// 网络/上游瞬态：不焚毁、不区分语义，按 waiting 让客户端继续轮询。
		slog.Warn("codebuddy qr poll upstream failed", slog.String("error", logredact.RedactText(err.Error())))
		return &CodeBuddyQRPollResult{Status: "waiting"}, nil
	}
	code := codebuddyEnvelopeCode(raw)
	switch code {
	case 0:
		data := gjson.GetBytes(raw, "data")
		if !data.IsObject() || strings.TrimSpace(data.Get("accessToken").String()) == "" {
			return &CodeBuddyQRPollResult{Status: "waiting"}, nil
		}
		// 上游 token 调用 single-flight（per-state 防并发双取账号 / 双建号）。
		result, ferr, _ := s.qrFlight.Do("codebuddy:qr:poll:"+state, func() (any, error) {
			return s.finalizeQRLogin(ctx, state, rec, []byte(data.Raw))
		})
		if ferr != nil {
			return nil, ferr
		}
		finished, ok := result.(*CodeBuddyQRPollResult)
		if !ok || finished == nil {
			return &CodeBuddyQRPollResult{Status: "waiting"}, nil
		}
		return finished, nil
	case 11217:
		// 11217 = 未扫码/登录进行中（实测口径）。
		return &CodeBuddyQRPollResult{Status: "waiting"}, nil
	default:
		return &CodeBuddyQRPollResult{
			Status: "error",
			Error: &CodeBuddyQREnvelopeError{
				Code: int(code),
				Msg:  codebuddyEnvelopeMsg(raw),
			},
		}, nil
	}
}

// finalizeQRLogin 焚毁 state → Bearer 取账号信息 → CAS upsert 落库。
func (s *CodeBuddyAdminService) finalizeQRLogin(ctx context.Context, state string, rec *codebuddy.StateRecord, tokenData []byte) (*CodeBuddyQRPollResult, error) {
	// 取到 token 当刻即焚（写库之前）。
	if dropErr := s.qrStore.Drop(ctx, state); dropErr != nil {
		slog.Warn("codebuddy qr state drop failed (best-effort)", slog.String("error", logredact.RedactText(dropErr.Error())))
	}

	accessToken := strings.TrimSpace(gjson.GetBytes(tokenData, "accessToken").String())
	refreshToken := strings.TrimSpace(gjson.GetBytes(tokenData, "refreshToken").String())
	expiresInSec := gjson.GetBytes(tokenData, "expiresIn").Int()
	domain := strings.TrimSpace(gjson.GetBytes(tokenData, "domain").String())

	raw, err := s.upstreamCall(ctx, http.MethodGet, s.qrBaseURL()+codeBuddyQRAccountPath+"?state="+state, "", "Bearer "+accessToken)
	if err != nil {
		return nil, err
	}
	if codebuddyEnvelopeCode(raw) != 0 {
		return nil, codebuddyUpstreamBizError(raw)
	}
	uid := strings.TrimSpace(gjson.GetBytes(raw, "data.uid").String())
	if uid == "" {
		// state 已焚；账号信息不全时如实报错，客户端发起重新扫码。
		return nil, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_QR_ACCOUNT_INFO_MISSING",
			"upstream login account info missing uid")
	}
	nickname := strings.TrimSpace(gjson.GetBytes(raw, "data.nickname").String())
	credentials := normalizeCodeBuddyQRLoginCredentials(
		accessToken, refreshToken, expiresInSec, domain, uid,
		nickname,
		strings.TrimSpace(gjson.GetBytes(raw, "data.enterpriseId").String()),
	)

	// 风险声明见设计（state 无鉴权是上游固有属性）；审计（actor/uid/nickname/
	// create|update）由 handler 层显式写审计上下文（SetAuditAction/SetAuditExtra）。
	account, created, err := s.upsertAccountByUID(ctx, uid, nickname, credentials)
	if err != nil {
		return nil, err
	}
	return &CodeBuddyQRPollResult{
		Status: "ok",
		Account: &CodeBuddyQRAccountSummary{
			ID:       account.ID,
			UID:      uid,
			Nickname: nickname,
			Created:  created,
			Updated:  !created,
		},
	}, nil
}

// normalizeCodeBuddyQRLoginCredentials 上游 token + 账号信息 →
// NormalizeCodeBuddyCredentials 白名单。expiresIn（秒）→ expiresAt 毫秒 =
// now + expiresIn*1000（换算单测；.粘贴 auth JSON 的毫秒 expiresAt 路径不回归）。
func normalizeCodeBuddyQRLoginCredentials(accessToken, refreshToken string, expiresInSec int64, domain, uid, nickname, enterpriseID string) map[string]any {
	auth := map[string]any{
		"accessToken": accessToken,
	}
	if refreshToken != "" {
		auth["refreshToken"] = refreshToken
	}
	if expiresInSec > 0 {
		// expiresIn 秒 → expires_at 毫秒（now + expiresIn*1000）。
		auth["expiresAt"] = time.Now().UnixMilli() + codeBuddyExpiresAtFromExpiresInMs(expiresInSec)
	}
	if domain != "" {
		auth["domain"] = domain
	}
	account := map[string]any{"uid": uid}
	if nickname != "" {
		account["nickname"] = nickname
	}
	if enterpriseID == "" {
		account["enterpriseId"] = nil // 个人账号 enterpriseId=null → 归一空串
	} else {
		account["enterpriseId"] = enterpriseID
	}
	return NormalizeCodeBuddyCredentials(map[string]any{"auth": auth, "account": account})
}

// codeBuddyExpiresAtFromExpiresInMs expiresIn 秒 → 毫秒差值（显式独立纯函数）。
// 与 NormalizeCodeBuddyCredentials 的 .info 毫秒 expiresAt 路径（expiresAt
// 原值优先）隔离不回归。
func codeBuddyExpiresAtFromExpiresInMs(expiresInSec int64) int64 {
	return expiresInSec * 1000
}

// codeBuddyExpiresAt expiresIn 秒 → 从 now 起算的到期时间。
func codeBuddyExpiresAt(now time.Time, expiresInSec int64) time.Time {
	return now.Add(time.Duration(expiresInSec) * time.Second)
}

// upsertAccountByUID 同 uid 重复纳管 = CAS upsert：按 platform+credentials.uid
// 查；已存在走 UpdateCodeBuddyCredentialsIfUnchanged（响应 updated:true），
// 否则 CreateAccount（强制 type=apikey，CreateAccount 既有校验兜底）。
func (s *CodeBuddyAdminService) upsertAccountByUID(ctx context.Context, uid, nickname string, credentials map[string]any) (*Account, bool, error) {
	existing, err := s.findCodeBuddyAccountByUID(ctx, uid)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		input := &CreateAccountInput{
			Name:        sanitizeCodeBuddyDisplayName(nickname, uid),
			Platform:    PlatformCodeBuddy,
			Type:        AccountTypeAPIKey, // 强制 apikey（CreateAccount 既有校验兜底）
			Credentials: credentials,
		}
		created, createErr := s.admin.CreateAccount(ctx, input)
		if createErr != nil {
			return nil, false, createErr
		}
		return created, true, nil
	}

	// CAS：期望凭据 = 最新快照；新凭据 = 既有管理员配置（base_url 等保留键 Merge 继承）。
	fresh, err := s.accountRepo.GetByID(ctx, existing.ID)
	if err != nil {
		return nil, false, err
	}
	merged := MergeCredentials(fresh.Credentials, credentials)
	conditionalRepo, ok := s.accountRepo.(CodeBuddyOAuthRefreshSuccessRepository)
	if !ok {
		return nil, false, infraerrors.InternalServer("CODEBUDDY_QR_CAS_UNAVAILABLE",
			"codebuddy credentials CAS repository is not configured")
	}
	applied, applyErr := conditionalRepo.UpdateCodeBuddyCredentialsIfUnchanged(
		ctx, fresh.ID, fresh.Credentials, fresh.ProxyID, merged)
	if applyErr != nil {
		return nil, false, applyErr
	}
	if !applied {
		return nil, false, infraerrors.Conflict("CODEBUDDY_QR_CAS_LOST",
			"account credentials changed concurrently during qr import; retry")
	}
	updated, err := s.accountRepo.GetByID(ctx, existing.ID)
	if err != nil {
		return nil, false, err
	}
	return updated, false, nil
}

// findCodeBuddyAccountByUID 按 platform=codebuddy + credentials.uid 查既有账号
// （同 uid 多个时取 ID 最小者，并发建号兜底）。
func (s *CodeBuddyAdminService) findCodeBuddyAccountByUID(ctx context.Context, uid string) (*Account, error) {
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformCodeBuddy)
	if err != nil {
		return nil, err
	}
	var found *Account
	for i := range accounts {
		account := &accounts[i]
		if !account.IsCodeBuddy() {
			continue
		}
		if strings.TrimSpace(account.GetCredential("uid")) == uid {
			if found == nil || account.ID < found.ID {
				found = account
			}
		}
	}
	return found, nil
}

func sanitizeCodeBuddyDisplayName(nickname, uid string) string {
	name := strings.TrimSpace(nickname)
	if name != "" {
		return name
	}
	return "codebuddy-" + uid
}

// --- A3 每日签到 ---

// CodeBuddyCheckinResult 签到响应（冻结白名单 {already_checked_in, credit, streak_days}）。
type CodeBuddyCheckinResult struct {
	AlreadyCheckedIn bool    `json:"already_checked_in"`
	Credit           float64 `json:"credit"`
	StreakDays       int64   `json:"streak_days"`
	// CreditsRecovered 本次签到是否触发了"积分耗尽冷却"的解除（4.5 解冻闭环）。
	CreditsRecovered bool `json:"credits_recovered"`
}

// Checkin 每日签到（POST /admin/codebuddy/accounts/:id/checkin 手动触发）。
// 上游 code=0 → {already:false, credit, streak_days}；code=10001 →
// {already:true,...}（幂等成功）；其他码 → 4xx 语义化（业务码表说明）。
// 留痕仅写 Account.Extra（last_checkin_at RFC3339 / streak_days，UpdateExtra
// 先例），禁写 credentials（归一化白名单会吞 + CAS 冲突）。
func (s *CodeBuddyAdminService) Checkin(ctx context.Context, accountID int64) (*CodeBuddyCheckinResult, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.NotFound("CODEBUDDY_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsCodeBuddy() {
		return nil, infraerrors.BadRequest("CODEBUDDY_CHECKIN_PLATFORM_UNSUPPORTED",
			"account is not a codebuddy account")
	}
	accessToken := account.GetCodeBuddyAccessToken()
	if accessToken == "" {
		return nil, infraerrors.BadRequest("CODEBUDDY_CHECKIN_NO_TOKEN",
			"codebuddy account has no access token")
	}

	// 计费 base 与路径族按账号 realm 分发（P0-3）：CN → www.codebuddy.cn +
	// /v2/billing/meter/daily-checkin；global → www.workbuddy.ai +
	// /billing/meter/daily-checkin，404 回落 /v2/...（两域顺序相反，见
	// codebuddy_realm_endpoints.go）。base 不跟 base_url（base_url 仅 chat）。
	// ⚠️ global 分支**未经真机验证**（本轮无 global 测试账号）。
	baseURL := CodeBuddyBillingBase(account)
	if s.testBaseURL != "" {
		baseURL = s.testBaseURL
	}
	raw, err := s.checkinWithFallback(ctx, baseURL, account, accessToken)
	if err != nil {
		return nil, err
	}
	code := codebuddyEnvelopeCode(raw)
	switch code {
	case 0, 10001:
		result := &CodeBuddyCheckinResult{
			AlreadyCheckedIn: code == 10001,
			Credit:           gjson.GetBytes(raw, "data.credit").Float(),
			StreakDays:       gjson.GetBytes(raw, "data.streak_days").Int(),
		}
		if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
			"last_checkin_at": time.Now().UTC().Format(time.RFC3339),
			"streak_days":     result.StreakDays,
		}); err != nil {
			slog.Warn("codebuddy checkin extra write failed",
				slog.String("error", logredact.RedactText(err.Error())))
		}
		// 解冻闭环（4.5）：签到正是"积分耗尽"账号补回积分的路径，签到成功
		// 就该顺势解除本模块写入的硬冷却，否则账号要一直等到人工恢复。
		s.unfreezeAfterCheckin(ctx, account, result)
		return result, nil
	default:
		return nil, infraerrors.Newf(http.StatusBadRequest, "CODEBUDDY_CHECKIN_REJECTED",
			"codebuddy checkin rejected (code %v): %s", codebuddyEnvelopeCodeRaw(raw),
			codebuddy.CodeBuddyBizCodeMessage(codebuddyEnvelopeCodeRaw(raw), codebuddyEnvelopeMsg(raw)))
	}
}

// unfreezeAfterCheckin 签到成功后按最新余额决定是否解除积分耗尽冷却。
//
// 只有**真的拿到余额**才判定：查不到余额时保持现状。用签到返回的 credit 字段
// 猜余额是错的 —— 它只是"本次签到得了多少分"，账号总分可能仍是 0。
func (s *CodeBuddyAdminService) unfreezeAfterCheckin(
	ctx context.Context,
	account *Account,
	result *CodeBuddyCheckinResult,
) {
	if account == nil || result == nil || !account.HasCodeBuddyCreditsExhaustedCooldown() {
		return
	}
	remain, ok := s.queryCodeBuddyCreditsRemain(ctx, account)
	if !ok {
		return
	}
	result.CreditsRecovered = s.ReenableCodeBuddyIfCreditsRecovered(ctx, account, remain)
}

// queryCodeBuddyCreditsRemain 查一次实时积分余额（只读，不改任何账号状态）。
// 未注入 fetcher 时返回 ok=false（解冻判定跳过，而不是误判为 0 分）。
func (s *CodeBuddyAdminService) queryCodeBuddyCreditsRemain(ctx context.Context, account *Account) (float64, bool) {
	if s == nil || s.creditsFetcher == nil || account == nil {
		return 0, false
	}
	token := account.GetCodeBuddyAccessToken()
	if strings.TrimSpace(token) == "" {
		return 0, false
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, codeBuddyBillingRequestTimeout)
	defer cancel()
	snapshot, err := s.creditsFetcher.FetchCredits(fetchCtx, &CodeBuddyCreditsFetchOptions{
		AccessToken: token,
		ProxyURL:    proxyURL,
		AccountID:   account.ID,
		Account:     account,
	})
	// Balance 是指针：nil 表示"余额不可用"，与"余额为 0"是两件事，
	// 不能混为一谈（把 nil 当 0 会让本可解冻的账号继续停调）。
	if err != nil || snapshot == nil || snapshot.Status != "ok" || snapshot.Balance == nil {
		return 0, false
	}
	return *snapshot.Balance, true
}

// CodeBuddyCheckinBatchResult 批量签到汇总。
// 四态互斥：Succeeded / AlreadyCheckedIn / Failed / Skipped，
// Total = 四态之和（即本轮考虑过的全部候选账号）。
type CodeBuddyCheckinBatchResult struct {
	Total            int     `json:"total"`
	Succeeded        int     `json:"succeeded"`
	AlreadyCheckedIn int     `json:"already_checked_in"`
	Failed           int     `json:"failed"`
	Skipped          int     `json:"skipped"`
	CreditEarned     float64 `json:"credit_earned"`
}

// CodeBuddyCheckinAccountNote 单账号的非成功明细（失败原因 / 跳过原因）。
// 对外只带可读信息，不带凭据。
type CodeBuddyCheckinAccountNote struct {
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	Message     string `json:"message"`
}

// CodeBuddyCheckinBatchResponse 批量签到响应：汇总 + 失败明细 + 跳过明细。
type CodeBuddyCheckinBatchResponse struct {
	CodeBuddyCheckinBatchResult
	Errors       []CodeBuddyCheckinAccountNote `json:"errors"`
	SkippedNotes []CodeBuddyCheckinAccountNote `json:"skipped_notes"`
}

// CodeBuddyCheckinCandidate 一个签到候选账号。
type CodeBuddyCheckinCandidate struct {
	AccountID int64
	Name      string
	// SkipReason 非空 = 本轮不发上游请求，直接计入 skipped。
	// 与"失败"区分开：跳过是我们**故意**不发的（账号已被停调 / 压根没凭据），
	// 不是签到本身出错，混进 Failed 会让失败率虚高、掩盖真实故障。
	SkipReason string
}

const (
	// codeBuddyCheckinBatchConcurrency 批量签到的并发上限（参考实现默认 5）。
	// 上游对同 IP 的并发敏感，串行又太慢（签到账号可能上百）。
	codeBuddyCheckinBatchConcurrency = 5

	// codeBuddyCheckinBatchMaxAccounts 单次批量请求处理的账号上限。
	codeBuddyCheckinBatchMaxAccounts = 500
)

// ListCodeBuddyCheckinCandidates 列出签到候选账号（含跳过标记）。
//
// 复用既有的 AccountRepository.ListByPlatform（它已经只返回 StatusActive 账号），
// 不新增仓储方法。候选一律返回，"不该发"的用 SkipReason 标出来而不是就地丢掉：
// 调度的汇总需要如实反映"这个账号为什么没签上"。
func (s *CodeBuddyAdminService) ListCodeBuddyCheckinCandidates(ctx context.Context, limit int) ([]CodeBuddyCheckinCandidate, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.InternalServer("CODEBUDDY_ACCOUNT_REPO_UNAVAILABLE", "codebuddy account repository not configured")
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformCodeBuddy)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > codeBuddyCheckinBatchMaxAccounts {
		limit = codeBuddyCheckinBatchMaxAccounts
	}

	now := time.Now()
	candidates := make([]CodeBuddyCheckinCandidate, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !account.IsCodeBuddy() {
			continue
		}
		candidate := CodeBuddyCheckinCandidate{AccountID: account.ID, Name: account.Name}
		switch {
		case !account.IsSchedulable():
			candidate.SkipReason = "账号已停调"
		case account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil):
			candidate.SkipReason = "账号处于临时停调冷却期"
		case strings.TrimSpace(account.GetCodeBuddyAccessToken()) == "":
			candidate.SkipReason = "账号缺少 access token"
		}
		candidates = append(candidates, candidate)
		if len(candidates) >= limit {
			break
		}
	}
	return candidates, nil
}

// CheckinAll 批量签到：并发执行 + 逐账号汇总（成功 / 已签到 / 失败 / 跳过）。
//
// 复用 Checkin 的既有实现，不重复实现签到逻辑（端点、realm 分流、幂等判定全在那边）。
// 单账号失败不影响其他账号；失败明细回传给调用方，而不是只给一个计数。
func (s *CodeBuddyAdminService) CheckinAll(ctx context.Context) (*CodeBuddyCheckinBatchResponse, error) {
	candidates, err := s.ListCodeBuddyCheckinCandidates(ctx, codeBuddyCheckinBatchMaxAccounts)
	if err != nil {
		return nil, err
	}
	return s.checkinCandidates(ctx, candidates), nil
}

// checkinCandidates 对已取好的候选批量签到并汇总（调度器与端点共用，
// 避免两处各写一份并发与汇总逻辑）。
func (s *CodeBuddyAdminService) checkinCandidates(
	ctx context.Context,
	candidates []CodeBuddyCheckinCandidate,
) *CodeBuddyCheckinBatchResponse {
	response := &CodeBuddyCheckinBatchResponse{}
	response.Total = len(candidates)
	if len(candidates) == 0 {
		return response
	}

	type outcome struct {
		accountID int64
		name      string
		already   bool
		credit    float64
		err       error
	}

	// 跳过的候选不进并发池：它们本来就不该发请求，占额度会拖慢真正要签的那批。
	toCheck := make([]CodeBuddyCheckinCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.SkipReason == "" {
			toCheck = append(toCheck, candidate)
			continue
		}
		response.Skipped++
		response.SkippedNotes = append(response.SkippedNotes, CodeBuddyCheckinAccountNote{
			AccountID:   candidate.AccountID,
			AccountName: candidate.Name,
			Message:     candidate.SkipReason,
		})
	}
	if len(toCheck) == 0 {
		return response
	}

	sem := make(chan struct{}, codeBuddyCheckinBatchConcurrency)
	results := make(chan outcome, len(toCheck))
	var wg sync.WaitGroup
	for _, candidate := range toCheck {
		wg.Add(1)
		go func(c CodeBuddyCheckinCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result, checkinErr := s.Checkin(ctx, c.AccountID)
			item := outcome{accountID: c.AccountID, name: c.Name, err: checkinErr}
			if checkinErr == nil && result != nil {
				item.already = result.AlreadyCheckedIn
				item.credit = result.Credit
			}
			results <- item
		}(candidate)
	}
	wg.Wait()
	close(results)

	for item := range results {
		switch {
		case item.err != nil:
			response.Failed++
			response.Errors = append(response.Errors, CodeBuddyCheckinAccountNote{
				AccountID:   item.accountID,
				AccountName: item.name,
				Message:     codeBuddyCheckinErrorMessage(item.err),
			})
		case item.already:
			response.AlreadyCheckedIn++
			response.CreditEarned += item.credit
		default:
			response.Succeeded++
			response.CreditEarned += item.credit
		}
	}
	return response
}

// codeBuddyCheckinErrorMessage 把单账号失败折算成可读文案。
// 批量汇总会把明细回给管理员，错误体可能回显请求内容 → 统一脱敏。
func codeBuddyCheckinErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return logredact.RedactText(err.Error())
}

// checkinWithFallback 按 realm 的路径候选序列发签到请求，**仅 HTTP 404**
// 换下一候选（上游"该域没有无 /v2 形式"的确切信号）；其他状态码/网络故障
// 立即返回，不做无谓重试（与计费查询同款回落口径）。
//
// 注意：`upstreamCall` 把非 2xx/网络故障都折算成 502 语义错误，因此这里需要
// 一层薄封装把「HTTP 404」与「其他失败」区分开——用 rawStatusCode 旁路通道
// 拿真实状态码，不改 upstreamCall 既有错误语义。
func (s *CodeBuddyAdminService) checkinWithFallback(ctx context.Context, baseURL string, account *Account, accessToken string) ([]byte, error) {
	paths := CodeBuddyDailyCheckinPaths(account)
	var lastErr error
	for i, path := range paths {
		if i > 0 {
			// 候选路径共享同一个请求 ctx：这里不额外加超时（调用方已有）。
			slog.Debug("codebuddy checkin falling back to versioned path",
				slog.Int64("account_id", account.ID), slog.String("path", path))
		}
		raw, status, err := s.upstreamCallWithStatus(ctx, http.MethodPost, baseURL+path, "{}", "Bearer "+accessToken)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if i < len(paths)-1 && status == http.StatusNotFound {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

// upstreamCallWithStatus 同 upstreamCall，但额外返回上游 HTTP 状态码（供路径回落
// 判定消费）。返回的 err 语义与 upstreamCall 完全一致。
func (s *CodeBuddyAdminService) upstreamCallWithStatus(ctx context.Context, method, requestURL string, body string, bearer string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, requestURL, strings.NewReader(body))
	if err != nil {
		return nil, 0, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_REQUEST_BUILD",
			"build codebuddy upstream request failed")
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header[k] = []string{v}
	}
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, doErr := s.client.Do(req)
	if doErr != nil {
		return nil, 0, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_UNREACHABLE",
			"codebuddy upstream request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_UNREACHABLE",
			"read codebuddy upstream response failed")
	}
	// 上游也可能用 HTTP 4xx 承载业务信封（与参考实现 _envelope 口径一致）：
	// 只有 404 是"路径不存在"的确定性信号，其余一律按原样返回给调用方解析信封。
	if resp.StatusCode == http.StatusNotFound {
		return raw, resp.StatusCode, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_PATH_NOT_FOUND",
			"codebuddy upstream path not found")
	}
	return raw, resp.StatusCode, nil
}

// --- 共用上游调用 ---

// upstreamCall 发起上游请求并返回完整响应体字节（信封 {code,msg,data}）。
// HTTP 4xx 也可能是业务信封（与参考实现 _envelope 口径一致）。
func (s *CodeBuddyAdminService) upstreamCall(ctx context.Context, method, requestURL string, body string, bearer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, requestURL, strings.NewReader(body))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_REQUEST_BUILD",
			"build codebuddy upstream request failed")
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header[k] = []string{v}
	}
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, doErr := s.client.Do(req)
	if doErr != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_UNREACHABLE",
			"codebuddy upstream request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_UNREACHABLE",
			"read codebuddy upstream response failed")
	}
	return raw, nil
}

func codebuddyEnvelopeCode(raw []byte) int64 {
	return gjson.GetBytes(raw, "code").Int()
}

// codebuddyEnvelopeCodeRaw 取信封 code 的**原值**（保留字符串/数字区别）。
//
// ⚠️ 上游的 code 不保证是数字：实测 "11-128" 是字符串形态，而 gjson .Int()
// 对非数字串返回 0——用它查码表/拼接文案会把真实拒因吞成 "code 0"。
// 凡是要展示或查码表的地方，都必须用这个函数而不是 codebuddyEnvelopeCode。
func codebuddyEnvelopeCodeRaw(raw []byte) any {
	res := gjson.GetBytes(raw, "code")
	if !res.Exists() {
		return int64(0)
	}
	switch res.Type {
	case gjson.String:
		return res.String()
	case gjson.Number:
		return res.Num
	default:
		return res.String()
	}
}

// codebuddyEnvelopeMsg 提取信封 msg 并脱敏（上游错误体可能回显请求内容）。
func codebuddyEnvelopeMsg(raw []byte) string {
	msg := strings.TrimSpace(gjson.GetBytes(raw, "msg").String())
	if msg == "" {
		msg = strings.TrimSpace(gjson.GetBytes(raw, "message").String())
	}
	return logredact.RedactText(msg)
}

// codebuddyUpstreamBizError 上游非 0 业务码 → 502 报错（仅 code+msg 语义）。
func codebuddyUpstreamBizError(raw []byte) error {
	return infraerrors.Newf(http.StatusBadGateway, "CODEBUDDY_UPSTREAM_REJECTED",
		"codebuddy upstream rejected (code %v): %s", codebuddyEnvelopeCodeRaw(raw),
		codebuddy.CodeBuddyBizCodeMessage(codebuddyEnvelopeCodeRaw(raw), codebuddyEnvelopeMsg(raw)))
}
