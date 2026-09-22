package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// CodeBuddy 业务码冷却的**网关路径**与**装配可达性**（A4 批 4.6）。
//
// 为什么单独一组：`ApplyCodeBuddyBizCodeCooldown` 单测全绿**不代表 4.6 生效**。
// 网关侧消费的是一个注入进来的窄接口 `codeBuddyCooldownApplier`，
// 只要没人调用 `SetCodeBuddyCooldownApplier`，该字段恒为 nil，
// `handleOpenAIAccountUpstreamError` 里整段业务码分支就**永不执行**——
// 4.6 静默变成死代码，而所有单测仍然绿。本轮已实测到过这个状态
// （`grep -rn SetCodeBuddyCooldownApplier` 全仓只命中定义处）。
//
// 所以这里断言两件事：
//  1. 未注入时**不**施加冷却（nil 安全，且证明注入确实是开关）；
//  2. 注入后网关真的按业务码走到冷却施加（端到端，不只是看 decision 结构）。

// recordingCooldownApplier 记录网关交给冷却执行者的参数。
type recordingCooldownApplier struct {
	calls []struct {
		accountID int64
		bizCode   int
		model     string
	}
	result bool
}

func (r *recordingCooldownApplier) ApplyCodeBuddyBizCodeCooldown(_ context.Context, account *Account, bizCode int, modelName string) bool {
	if account != nil {
		r.calls = append(r.calls, struct {
			accountID int64
			bizCode   int
			model     string
		}{account.ID, bizCode, modelName})
	}
	return r.result
}

func newCodeBuddyGatewayForCooldownTest(applier codeBuddyCooldownApplier) *OpenAIGatewayService {
	svc := &OpenAIGatewayService{}
	if applier != nil {
		svc.SetCodeBuddyCooldownApplier(applier)
	}
	return svc
}

// Scenario：**未注入时业务码冷却分支不得执行**。
// 这既是 nil 安全，也把"注入 = 开关"这件事钉死：没注入就是没冷却。
func TestCodeBuddyBizCodeCooldownWithoutApplierIsInert(t *testing.T) {
	svc := newCodeBuddyGatewayForCooldownTest(nil)
	account := &Account{ID: 61, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}

	body := []byte(`{"code":14018,"msg":"Credits exhausted"}`)
	require.False(t, svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, http.Header{}, body, "hy3"))

	// 账号不得被摘出池子（没有执行者，谁都不该动它）。
	require.Nil(t, account.TempUnschedulableUntil)
}

// Scenario：注入后，网关按业务码把冷却交给执行者，并带上**请求模型**。
// 模型名必须传下去：6004 是模型级限流，丢了模型名就只能放弃冷却而不是降级为整号冷却。
func TestCodeBuddyBizCodeCooldownRoutesToApplierWithModel(t *testing.T) {
	applier := &recordingCooldownApplier{result: true}
	svc := newCodeBuddyGatewayForCooldownTest(applier)
	account := &Account{ID: 62, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}

	body := []byte(`{"code":6004,"msg":"usage limit"}`)
	handled := svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, http.Header{}, body, "hy3-preview")

	require.True(t, handled, "命中冷却执行者时应视为已处理")
	require.Len(t, applier.calls, 1)
	require.Equal(t, int64(62), applier.calls[0].accountID)
	require.Equal(t, 6004, applier.calls[0].bizCode)
	require.Equal(t, "hy3-preview", applier.calls[0].model,
		"请求模型必须传到冷却决策（6004 靠它做模型级冷却）")
}

// Scenario：业务码是**字符串**形态时同样解析出来（L3）。
// gjson .Int() 对 "6004" 这类字符串返回 0，与真实 code=0（成功）不可区分；
// 必须走 CodeBuddyNumericBizCode，否则字符串码的冷却全部失效。
func TestCodeBuddyBizCodeCooldownParsesStringBizCode(t *testing.T) {
	applier := &recordingCooldownApplier{result: true}
	svc := newCodeBuddyGatewayForCooldownTest(applier)
	account := &Account{ID: 63, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}

	body := []byte(`{"code":"6004","msg":"usage limit"}`)
	require.True(t, svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, http.Header{}, body, "hy3"))

	require.Len(t, applier.calls, 1)
	require.Equal(t, 6004, applier.calls[0].bizCode, "字符串形态的业务码必须解析出来")
}

// Scenario：非数字业务码（"11-128"）解不出来 → **不**乱猜、不施加冷却。
// 把它当成 0 或强行转数字都会造成误冷却；交给通用错误链处理才是正确行为。
func TestCodeBuddyBizCodeCooldownIgnoresNonNumericCode(t *testing.T) {
	applier := &recordingCooldownApplier{result: true}
	svc := newCodeBuddyGatewayForCooldownTest(applier)
	account := &Account{ID: 64, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}

	body := []byte(`{"code":"11-128","msg":"Illegal API invocation from an unapproved channel"}`)
	svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, http.Header{}, body, "hy3")

	require.Empty(t, applier.calls, "非数字业务码不得触发按码冷却（避免误判）")
}

// Scenario：401/403 仍走"确认性凭据拒绝 → StatusError"分支，**不**被业务码冷却抢管。
// 凭据坏了要重录，不是等一会儿就会好；用冷却掩盖会让账号白等。
func TestCodeBuddyAuthRejectionTakesPrecedenceOverBizCodeCooldown(t *testing.T) {
	applier := &recordingCooldownApplier{result: true}
	repo := &codebuddySetErrorRepo{}
	svc := newCodeBuddyGatewayForCooldownTest(applier)
	svc.accountRepo = repo
	account := &Account{ID: 65, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}

	body := []byte(`{"code":14018,"msg":"Credits exhausted"}`)
	require.True(t, svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusUnauthorized, http.Header{}, body, "hy3"))

	require.Empty(t, applier.calls, "401 应由凭据分支处理，不走业务码冷却")
	require.Equal(t, int64(65), repo.errorAccountID, "确认性凭据拒绝应登记 StatusError")
}

// Scenario：**装配可达性**——ProvideCodeBuddyCheckinScheduler 必须把
// CodeBuddyAdminService 注入网关，否则 4.6 的窄接口恒为 nil（见文件头）。
//
// 这条是"接线真的存在"的回归保护：把 wire.go 里那两行注入删掉，测试立刻变红，
// 而不是等线上发现积分耗尽的账号根本没被停调。
func TestProvideCodeBuddyCheckinSchedulerInjectsCooldownApplier(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	admin := &CodeBuddyAdminService{}
	scheduler := ProvideCodeBuddyCheckinScheduler(admin, newPlatformFeatureTestService(t, newPlatformFeatureTestRepo()), gateway)
	t.Cleanup(scheduler.Stop) // Provider 会 Start，必须收尾，否则遗留 tick goroutine

	require.NotNil(t, scheduler)
	require.NotNil(t, scheduler.cron, "Provide 会 Start（与既有 Provide* 先例一致）")
	require.NotNil(t, gateway.codeBuddyCooldownApplier,
		"装配必须把冷却执行者注入网关，否则 4.6 是死代码")
	_, ok := gateway.codeBuddyCooldownApplier.(*CodeBuddyAdminService)
	require.True(t, ok, "注入的应是 CodeBuddyAdminService")
}

// Scenario：网关为 nil 时装配不得 panic（防御路径）。
func TestProvideCodeBuddyCheckinSchedulerNilGatewayIsSafe(t *testing.T) {
	var scheduler *CodeBuddyCheckinScheduler
	require.NotPanics(t, func() {
		scheduler = ProvideCodeBuddyCheckinScheduler(&CodeBuddyAdminService{}, newPlatformFeatureTestService(t, newPlatformFeatureTestRepo()), nil)
	})
	t.Cleanup(scheduler.Stop)
}

// codebuddySetErrorRepo 记录 SetError 调用（401/403 分支）。
type codebuddySetErrorRepo struct {
	AccountRepository
	errorAccountID int64
	errorMessage   string
}

func (r *codebuddySetErrorRepo) SetError(_ context.Context, id int64, errorMsg string) error {
	r.errorAccountID = id
	r.errorMessage = errorMsg
	return nil
}

func (r *codebuddySetErrorRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return nil, ErrAccountNotFound
}

var _ = time.Second
