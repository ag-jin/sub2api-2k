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

	"github.com/Wei-Shaw/sub2api/internal/pkg/codebuddyqr"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// --- 测试基建 ---

type codebuddyAdminTestRepo struct {
	AccountRepository
	accounts map[int64]*Account
	casFail  bool
}

func newCodebuddyAdminTestRepo(accounts ...*Account) *codebuddyAdminTestRepo {
	repo := &codebuddyAdminTestRepo{accounts: map[int64]*Account{}}
	for _, account := range accounts {
		repo.accounts[account.ID] = account
	}
	return repo
}

func (r *codebuddyAdminTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if account, ok := r.accounts[id]; ok {
		return account, nil
	}
	return nil, ErrAccountNotFound
}

func (r *codebuddyAdminTestRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	out := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, *account)
		}
	}
	return out, nil
}

func (r *codebuddyAdminTestRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	account, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	for key, value := range updates {
		account.Extra[key] = value
	}
	return nil
}

// UpdateCodeBuddyCredentialsIfUnchanged 测试 CAS：期望凭据以 access_token 全文比对。
func (r *codebuddyAdminTestRepo) UpdateCodeBuddyCredentialsIfUnchanged(
	_ context.Context, id int64, expected map[string]any, _ *int64, credentials map[string]any,
) (bool, error) {
	account, ok := r.accounts[id]
	if !ok {
		return false, nil
	}
	if r.casFail || account.Credentials["access_token"] != expected["access_token"] {
		return false, nil
	}
	account.Credentials = credentials
	return true, nil
}

type codebuddyAdminStubAdmin struct {
	AdminService
	repo    *codebuddyAdminTestRepo
	nextID  int64
	created []CreateAccountInput
}

func (a *codebuddyAdminStubAdmin) CreateAccount(_ context.Context, input *CreateAccountInput) (*Account, error) {
	a.nextID++
	account := &Account{
		ID: a.nextID, Platform: input.Platform, Type: input.Type,
		Credentials: input.Credentials, Extra: map[string]any{},
	}
	a.repo.accounts[account.ID] = account
	a.created = append(a.created, *input)
	return account, nil
}

// codebuddyUpstreamStub 可编程的上游信封模拟器（按路径路由）。
type codebuddyUpstreamStub struct {
	handlers map[string]http.HandlerFunc
}

func (m *codebuddyUpstreamStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler, ok := m.handlers[r.URL.Path]; ok {
		handler(w, r)
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, `{"code":500,"msg":"no stub"}`)
}

type codebuddyAdminTestEnv struct {
	svc    *CodeBuddyAdminService
	repo   *codebuddyAdminTestRepo
	stub   *codebuddyAdminStubAdmin
	mux    *codebuddyUpstreamStub
	server *httptest.Server
	store  codebuddyqr.Store
}

func newCodebuddyAdminTestEnv(t *testing.T, accounts ...*Account) *codebuddyAdminTestEnv {
	t.Helper()
	mux := &codebuddyUpstreamStub{handlers: map[string]http.HandlerFunc{}}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	redisServer := miniredis.RunT(t)
	store := codebuddyqr.NewRedisStore(redis.NewClient(&redis.Options{Addr: redisServer.Addr()}))
	repo := newCodebuddyAdminTestRepo(accounts...)
	stub := &codebuddyAdminStubAdmin{repo: repo, nextID: 100}
	svc := NewCodeBuddyAdminService(stub, repo, store).WithTestBaseURL(server.URL)
	return &codebuddyAdminTestEnv{svc: svc, repo: repo, stub: stub, mux: mux, server: server, store: store}
}

func (e *codebuddyAdminTestEnv) stubState(path, envelope string) {
	e.mux.handlers[path] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, envelope)
	}
}

const (
	testQROkTokenEnvelope = `{"code":0,"msg":"OK","data":{"accessToken":"qr-access-token","refreshToken":"qr-refresh-token","expiresIn":7200,"domain":"www.codebuddy.cn"}}`
	testQRAccountEnvelope = `{"code":0,"msg":"OK","data":{"uid":"10086","nickname":"测试昵称","enterpriseId":null}}`
)

func (e *codebuddyAdminTestEnv) start(t *testing.T, actor string) string {
	t.Helper()
	result, err := e.svc.Start(context.Background(), actor)
	require.NoError(t, err)
	require.NotEmpty(t, result.State)
	require.NotEmpty(t, result.AuthURL)
	return result.State
}

func codebuddyErrReason(err error) string {
	if appErr, ok := err.(*infraerrors.ApplicationError); ok {
		return appErr.Reason
	}
	return err.Error()
}

// --- A1 状态机 ---

// Scenario: 未扫码 11217 → waiting；他人/未知 state 一律 404（不区分态）。
func TestCodeBuddyQRPollWaitingAndActorBinding(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t)
	env.stubState("/v2/plugin/auth/state", `{"code":0,"data":{"state":"st-wait","authUrl":"https://copilot.tencent.com/login?platform=CLI&state=st-wait"}}`)
	env.stubState("/v2/plugin/auth/token", `{"code":11217,"msg":"11217:login ing...","data":null}`)
	state := env.start(t, "actor-1")

	// 他人 → 404。
	_, err := env.svc.Poll(context.Background(), "actor-2", state)
	require.True(t, infraerrors.IsNotFound(err), codebuddyErrReason(err))
	// 未知 state → 同样 404。
	_, err = env.svc.Poll(context.Background(), "actor-1", "no-such-state")
	require.True(t, infraerrors.IsNotFound(err))

	// 发起者本人 → waiting（11217）。
	result, err := env.svc.Poll(context.Background(), "actor-1", state)
	require.NoError(t, err)
	require.Equal(t, "waiting", result.Status)
	require.Nil(t, result.Account)
	require.Nil(t, result.Error)
}

// Scenario: 扫码成功 → 当刻焚 state（第二次 poll 404）+ 建号双分支之 Create。
func TestCodeBuddyQRPollSuccessCreatesAndBurnsState(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t)
	env.stubState("/v2/plugin/auth/state", `{"code":0,"data":{"state":"st-ok","authUrl":"u"}}`)
	env.stubState("/v2/plugin/auth/token", testQROkTokenEnvelope)
	env.stubState("/v2/plugin/login/account", testQRAccountEnvelope)
	state := env.start(t, "actor-1")
	time.Sleep(2100 * time.Millisecond) // 过 first-poll 节流窗

	result, err := env.svc.Poll(context.Background(), "actor-1", state)
	require.NoError(t, err)
	require.Equal(t, "ok", result.Status)
	require.NotNil(t, result.Account)
	require.Equal(t, "10086", result.Account.UID)
	require.Equal(t, "测试昵称", result.Account.Nickname)
	require.True(t, result.Account.Created)
	require.Equal(t, int64(101), result.Account.ID)
	require.Len(t, env.stub.created, 1)
	require.Equal(t, PlatformCodeBuddy, env.stub.created[0].Platform)
	require.Equal(t, AccountTypeAPIKey, env.stub.created[0].Type, "Create 强制 type=apikey")
	credentials := env.stub.created[0].Credentials
	require.Equal(t, "qr-access-token", credentials["access_token"])
	require.Equal(t, "10086", credentials["uid"])
	require.NotContains(t, credentials, "nickname", "凭据白名单不含 nickname")

	// state 当刻已焚（写库之前即焚）：再次轮询 → 404。
	time.Sleep(2100 * time.Millisecond)
	_, err = env.svc.Poll(context.Background(), "actor-1", state)
	require.True(t, infraerrors.IsNotFound(err))
}

// Scenario: 同 uid 重复纳管 → CAS 更新（updated:true；管理员配置键继承）。
func TestCodeBuddyQRPollUpdateExistingUID(t *testing.T) {
	t.Parallel()
	existing := &Account{
		ID:          55,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "old-token", "uid": "10086", "base_url": "https://copilot.tencent.com"},
	}
	env := newCodebuddyAdminTestEnv(t, existing)
	env.stubState("/v2/plugin/auth/state", `{"code":0,"data":{"state":"st-upd","authUrl":"u"}}`)
	env.stubState("/v2/plugin/auth/token", testQROkTokenEnvelope)
	env.stubState("/v2/plugin/login/account", testQRAccountEnvelope)
	state := env.start(t, "actor-1")
	time.Sleep(2100 * time.Millisecond)

	result, err := env.svc.Poll(context.Background(), "actor-1", state)
	require.NoError(t, err)
	require.Equal(t, "ok", result.Status)
	require.False(t, result.Account.Created)
	require.True(t, result.Account.Updated)
	require.Equal(t, int64(55), result.Account.ID)
	require.Empty(t, env.stub.created, "同 uid 不得新建账号")
	require.Equal(t, "qr-access-token", existing.Credentials["access_token"])
	require.Equal(t, "https://copilot.tencent.com", existing.Credentials["base_url"],
		"CAS upsert 继承管理员配置键（base_url 等）")
}

// Scenario: CAS 失败（凭据被并发改写）→ 409 Conflict，不造第二份账号。
func TestCodeBuddyQRPollCASLost(t *testing.T) {
	t.Parallel()
	existing := &Account{
		ID:          55,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "old-token", "uid": "10086"},
	}
	env := newCodebuddyAdminTestEnv(t, existing)
	env.stubState("/v2/plugin/auth/state", `{"code":0,"data":{"state":"st-cas","authUrl":"u"}}`)
	env.stubState("/v2/plugin/auth/token", testQROkTokenEnvelope)
	// login/account 返回 uid=99999（与 stub 中已有 uid=10086 不同账号）→
	// 走查询-不存在 → Create 分支；改测 CAS 失败需 uid 相同 + 凭据被并发改写。
	env.stubState("/v2/plugin/login/account", testQRAccountEnvelope)
	state := env.start(t, "actor-1")
	time.Sleep(2100 * time.Millisecond)

	// 模拟并发改写：CAS 恒失败（expected ≠ 实际 → 409 Conflict，不造二号账号）。
	env.repo.casFail = true
	_, pollErr := env.svc.Poll(context.Background(), "actor-1", state)
	require.Error(t, pollErr)
	require.EqualValues(t, http.StatusConflict, codebuddyHTTPStatus(pollErr))
	require.Empty(t, env.stub.created)
}

// Scenario: state 过期（createdAt+300s）→ status=expired 且焚毁（再 poll 404）。
func TestCodeBuddyQRPollExpired(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t)
	// 直接写入逻辑已过期的 state 记录（绕过 Start 时序）。
	err := env.store.Create(context.Background(), "st-expired", codebuddyqr.StateRecord{
		ActorID:   "actor-1",
		CreatedAt: time.Now().Add(-301 * time.Second).UnixMilli(),
	})
	require.NoError(t, err)

	result, err := env.svc.Poll(context.Background(), "actor-1", "st-expired")
	require.NoError(t, err)
	require.Equal(t, "expired", result.Status)
	require.Nil(t, result.Account)
	// expired 即焚：再来一次 → 404。
	_, err = env.svc.Poll(context.Background(), "actor-1", "st-expired")
	require.True(t, infraerrors.IsNotFound(err))
}

// Scenario: per-state 节流 <2s → 429。
func TestCodeBuddyQRPollThrottled(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t)
	env.stubState("/v2/plugin/auth/state", `{"code":0,"data":{"state":"st-t","authUrl":"u"}}`)
	env.stubState("/v2/plugin/auth/token", `{"code":11217}`)
	state := env.start(t, "actor-1")
	_, err := env.svc.Poll(context.Background(), "actor-1", state)
	require.NoError(t, err)
	_, err = env.svc.Poll(context.Background(), "actor-1", state)
	require.True(t, infraerrors.IsTooManyRequests(err), codebuddyErrReason(err))
}

// Scenario: 每 actor 并发 state ≤2（第 3 个 start → 429）。
func TestCodeBuddyQRStartActorLimit(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t)
	starts := 0
	env.mux.handlers["/v2/plugin/auth/state"] = func(w http.ResponseWriter, r *http.Request) {
		starts++
		_, _ = io.WriteString(w, fmt.Sprintf(`{"code":0,"data":{"state":"st-limit-%d","authUrl":"u"}}`, starts))
	}
	_, err := env.svc.Start(context.Background(), "actor-1")
	require.NoError(t, err)
	_, err = env.svc.Start(context.Background(), "actor-1")
	require.NoError(t, err)
	_, err = env.svc.Start(context.Background(), "actor-1")
	require.True(t, infraerrors.IsTooManyRequests(err), codebuddyErrReason(err))
}

// Scenario: 上游非 0 码（code=12153）仅透传 code+msg（status=error + error{code,msg}）。
func TestCodeBuddyQRPollUpstreamErrorPassthrough(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t)
	env.stubState("/v2/plugin/auth/state", `{"code":0,"data":{"state":"st-err","authUrl":"u"}}`)
	env.stubState("/v2/plugin/auth/token", `{"code":12153,"msg":"session invalid"}`)
	state := env.start(t, "actor-1")
	result, pollErr := env.svc.Poll(context.Background(), "actor-1", state)
	require.NoError(t, pollErr)
	require.Equal(t, "error", result.Status)
	require.NotNil(t, result.Error)
	require.Equal(t, 12153, result.Error.Code)
	require.Equal(t, "session invalid", result.Error.Msg)
	require.Nil(t, result.Account)
}

// --- A3 每日签到 ---

// Scenario: code=0 成功 → {already:false, credit, streak_days}；Extra 写入
// last_checkin_at / streak_days；credentials 不动。
func TestCodeBuddyCheckinSuccess(t *testing.T) {
	t.Parallel()
	account := &Account{
		ID:          11,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at-x", "uid": "u11"},
	}
	env := newCodebuddyAdminTestEnv(t, account)
	env.stubState("/v2/billing/meter/daily-checkin", `{"code":0,"msg":"OK","data":{"credit":100,"streak_days":2,"is_streak_day":false}}`)

	result, err := env.svc.Checkin(context.Background(), 11)
	require.NoError(t, err)
	require.False(t, result.AlreadyCheckedIn)
	require.InDelta(t, 100, result.Credit, 1e-9)
	require.Equal(t, int64(2), result.StreakDays)
	require.NotEmpty(t, account.Extra["last_checkin_at"])
	require.Equal(t, int64(2), account.Extra["streak_days"])
	// 签到留痕禁写 credentials。
	require.NotContains(t, account.Credentials, "last_checkin_at")
	require.NotContains(t, account.Credentials, "streak_days")
}

// Scenario: code=10001 → 幂等成功 {already:true,...}，Extra 同样写入。
func TestCodeBuddyCheckinAlreadyCheckedIn(t *testing.T) {
	t.Parallel()
	account := &Account{
		ID:          11,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at-x"},
	}
	env := newCodebuddyAdminTestEnv(t, account)
	env.stubState("/v2/billing/meter/daily-checkin", `{"code":10001,"msg":"102 前已签到","data":{"credit":0,"streak_days":3}}`)

	result, err := env.svc.Checkin(context.Background(), 11)
	require.NoError(t, err)
	require.True(t, result.AlreadyCheckedIn)
	require.Equal(t, int64(3), result.StreakDays)
	require.Equal(t, int64(3), account.Extra["streak_days"])
}

// Scenario: 其他码 → 4xx（语义化 message 带码表说明）。
func TestCodeBuddyCheckinRejected(t *testing.T) {
	t.Parallel()
	account := &Account{
		ID:          11,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at-x"},
	}
	env := newCodebuddyAdminTestEnv(t, account)
	env.stubState("/v2/billing/meter/daily-checkin", `{"code":12153,"msg":"session expired"}`)
	_, err := env.svc.Checkin(context.Background(), 11)
	require.Error(t, err)
	require.EqualValues(t, http.StatusBadRequest, codebuddyHTTPStatus(err))
	require.Contains(t, err.Error(), "12153")
	require.Contains(t, err.Error(), "会话已失效", "码表 12153 说明被拼接进 message")
}

// Scenario: 非 codebuddy 账号 / 缺 token → 4xx 语义化。
func TestCodeBuddyCheckinGuards(t *testing.T) {
	t.Parallel()
	env := newCodebuddyAdminTestEnv(t, &Account{
		ID:          21,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "anthropic-at"},
	})
	_, err := env.svc.Checkin(context.Background(), 21)
	require.True(t, infraerrors.IsBadRequest(err), codebuddyErrReason(err))
	_, err = env.svc.Checkin(context.Background(), 999)
	require.True(t, infraerrors.IsNotFound(err))
}

// --- expiresIn 换算 + 码表 ---

// Scenario: expiresIn 秒 → expiresAt 毫秒（now + expiresIn*1000）；
// 与 .fetchInfo 毫秒 expiresAt 路径（优先 expiresAt 原值）隔离不回归。
func TestCodeBuddyExpiresInConversion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	// 纯函数：expiresIn 秒 → 从 now 起算的到期时刻。
	require.Equal(t, now.Add(7200*time.Second), codeBuddyExpiresAt(now, 7200))
	require.Equal(t, now, codeBuddyExpiresAt(now, 0))
	require.Equal(t, now.Add(-30*time.Second), codeBuddyExpiresAt(now, -30))

	// 换算毫秒值 = nowMs + sec*1000。
	credentials := normalizeCodeBuddyQRLoginCredentials("at", "rt", 60, "", "u1", "nick", "")
	require.NotEmpty(t, credentials["expires_at"])
	expiresAt := requireUpdatedAtRFC3339(t, credentials["expires_at"])
	require.WithinDuration(t, time.Now().Add(60*time.Second), expiresAt, 5*time.Second)

	// 嵌套毫秒 expiresAt 路径不回归：expiresAt 优先，expiresIn 不参与重算。
	raw := map[string]any{
		"auth": map[string]any{
			"accessToken": "atk", "expiresAt": 1700000000000, "expiresIn": 999999,
		},
	}
	normalized := NormalizeCodeBuddyCredentials(raw)
	require.Equal(t, "2023-11-14T22:13:20Z", normalized["expires_at"])
}

func requireUpdatedAtRFC3339(t *testing.T, raw any) time.Time {
	t.Helper()
	text, ok := raw.(string)
	if !ok {
		require.FailNow(t, "expires_at must be RFC3339 string")
	}
	parsed, err := time.Parse(time.RFC3339, text)
	require.NoError(t, err)
	return parsed
}

// Scenario: 码表映射（A4）。
func TestCodeBuddyBizCodeHints(t *testing.T) {
	t.Parallel()

	for _, code := range []int{0, 10001, 11101, 11128, 11217, 12153} {
		require.NotEmpty(t, CodeBuddyBizCodeHint(code))
	}
	require.Empty(t, CodeBuddyBizCodeHint(99999))
	require.Equal(t, "raw msg", CodeBuddyBizCodeMessage(99999, "raw msg"))
	require.Contains(t, CodeBuddyBizCodeMessage(10001, "已签过"), "今日已签到")
}

// 场景守卫：凭据串需不会被打印（走 logredact 由调用处保证；此断言防未来回归）。
func TestCodeBuddyNoCredentialEcho(t *testing.T) {
	t.Parallel()

	err := codebuddyUpstreamBizError([]byte(`{"code":12153,"msg":"access_token=qr-access-token-VALUE"}`))
	require.Error(t, err)
	require.EqualValues(t, http.StatusBadGateway, codebuddyHTTPStatus(err))
	require.True(t, strings.Contains(strings.ToLower(err.Error()), "rejected"))
	// 脱敏（logredact.UIT 类 token 模式）：错误串不得回显凭据原文。
	require.NotContains(t, err.Error(), "qr-access-token-VALUE", "错误信息不得回显凭据原文")
}

// codebuddyHTTPStatus 应用层错误 → HTTP 状态（测试便捷）。
func codebuddyHTTPStatus(err error) int {
	status, _ := infraerrors.ToHTTP(err)
	return status
}
