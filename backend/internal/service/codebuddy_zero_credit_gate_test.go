package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 票 #37：CodeBuddy 0 积分主动门（周期探测停调 + 自动恢复）。
//
// 与 A7 被动冷却（codebuddy_account_cooldown.go）的分工：
//   - A7：请求**已经打到**该账号、上游回 402/14018 之后才冷；
//   - 本门：周期**主动**查余额，归零即提前摘出，不等第一个失败请求。
//
// 两条通道共用 TempUnschedulableUntil 字段，靠 reason 前缀区分所有权
// （`codebuddy_zero_credit` vs `codebuddy_account_cooldown:`）——本门的 Clear
// 只清自己写的那条，A7 的浅冷却（404/10 分钟）绝不能被顺手清掉。

// zeroCreditRepoStub 记录停调/恢复调用（实现 AccountRepository 的最小面）。
type zeroCreditRepoStub struct {
	AccountRepository
	accounts   []Account
	listErr    error
	setCalls   []zeroCreditSetCall
	clearCalls []int64
	setErr     error
	clearErr   error
}

type zeroCreditSetCall struct {
	accountID int64
	until     time.Time
	reason    string
}

func (r *zeroCreditRepoStub) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.accounts, nil
}

// 桩把「尝试」与「结果」分开记录：写失败（setErr/clearErr）时仍能看到本门确实
// 试过哪几个账号，从而区分"没试"和"试了但没成功"。
func (r *zeroCreditRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.setCalls = append(r.setCalls, zeroCreditSetCall{accountID: id, until: until, reason: reason})
	return r.setErr
}

func (r *zeroCreditRepoStub) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.clearCalls = append(r.clearCalls, id)
	return r.clearErr
}

// zeroCreditProberStub 记录探测调用并返回预置余额/错误。
// 余额是**独立事实源**：测试直接给数值，不重算上游口径。
type zeroCreditProberStub struct {
	balances map[int64]float64
	usages   map[int64]*UpstreamBalanceUsage
	errs     map[int64]error
	calls    []int64
}

func (p *zeroCreditProberStub) FetchCredits(_ context.Context, opts *CodeBuddyCreditsFetchOptions) (*UpstreamBalanceUsage, error) {
	if opts == nil {
		return nil, errors.New("opts is required")
	}
	p.calls = append(p.calls, opts.AccountID)
	if err, ok := p.errs[opts.AccountID]; ok && err != nil {
		return nil, err
	}
	if usage, ok := p.usages[opts.AccountID]; ok {
		return usage, nil
	}
	balance, ok := p.balances[opts.AccountID]
	if !ok {
		return nil, errors.New("no fixture for account")
	}
	return &UpstreamBalanceUsage{Unit: "credits", Status: "ok", Balance: &balance}, nil
}

func zeroCreditAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "token-" + strconv.FormatInt(id, 10)},
	}
}

// newZeroCreditGateForTest 用固定时钟装配本门（生产时钟恒为 time.Now）。
func newZeroCreditGateForTest(repo AccountRepository, prober codeBuddyCreditsProber, now time.Time) *CodeBuddyZeroCreditGate {
	gate := NewCodeBuddyZeroCreditGate(repo, prober, &config.Config{
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{
				ZeroCreditGateEnabled:          true,
				ZeroCreditCheckIntervalMinutes: 30,
			},
		},
	}, 30*time.Minute)
	gate.clock = func() time.Time { return now }
	return gate
}

// Scenario（票面）：余额 0 → 摘出，until = **次日 04:00 UTC+8**（与 A7 硬冷却
// 同时点：签到补回积分后自然可恢复），reason 带本门前缀。
func TestCodeBuddyZeroCreditGate_ZeroCreditsBlocksUntilNextFourAM(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(501)}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{501: 0}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	require.Len(t, repo.setCalls, 1)
	assert.Equal(t, int64(501), repo.setCalls[0].accountID)
	// 独立事实源：12:30 当天 → 2026-10-09 04:00 (UTC+8)。
	assert.Equal(t, time.Date(2026, 10, 9, 4, 0, 0, 0, codeBuddyTimeZone), repo.setCalls[0].until)
	assert.Equal(t, codeBuddyZeroCreditReasonPrefix, repo.setCalls[0].reason)
	assert.Empty(t, repo.clearCalls)
	assert.Equal(t, 1, summary.Checked)
	assert.Equal(t, 1, summary.Blocked)
	assert.Zero(t, summary.Recovered)
}

// Scenario（票面）：深夜 02:00 归零 → **当天** 04:00（签到窗口前，A7 同口径）。
func TestCodeBuddyZeroCreditGate_LateNightBlocksSameDayFourAM(t *testing.T) {
	now := time.Date(2026, 10, 8, 2, 0, 0, 0, codeBuddyTimeZone)
	repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(501)}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{501: 0}}

	newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	require.Len(t, repo.setCalls, 1)
	assert.Equal(t, time.Date(2026, 10, 8, 4, 0, 0, 0, codeBuddyTimeZone), repo.setCalls[0].until)
}

// Scenario（票面）：已在**本门**冷却中且余额仍 0 → 续期（写入的 reason 不变，
// 仍是本门前缀，绝不改成别的理由）。
func TestCodeBuddyZeroCreditGate_StillZeroRenewsWithoutReasonDrift(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	blocked := zeroCreditAccount(501)
	until := time.Date(2026, 10, 8, 4, 0, 0, 0, codeBuddyTimeZone) // 当天 04:00 已过 → 需续到次日
	blocked.TempUnschedulableUntil = &until
	blocked.TempUnschedulableReason = codeBuddyZeroCreditReasonPrefix
	repo := &zeroCreditRepoStub{accounts: []Account{*blocked}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{501: 0}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	require.Len(t, repo.setCalls, 1)
	assert.Equal(t, time.Date(2026, 10, 9, 4, 0, 0, 0, codeBuddyTimeZone), repo.setCalls[0].until, "续期到次日 04:00")
	assert.Equal(t, codeBuddyZeroCreditReasonPrefix, repo.setCalls[0].reason, "续期不得漂移 reason")
	assert.Equal(t, 1, summary.Blocked)
}

// Scenario（票面）：余额 > 0 且账号正被**本门**冷却 → 自动恢复（立即清）。
func TestCodeBuddyZeroCreditGate_CreditsRecoveredClearsOwnBlock(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	blocked := zeroCreditAccount(502)
	until := time.Date(2026, 10, 9, 4, 0, 0, 0, codeBuddyTimeZone)
	blocked.TempUnschedulableUntil = &until
	blocked.TempUnschedulableReason = codeBuddyZeroCreditReasonPrefix
	repo := &zeroCreditRepoStub{accounts: []Account{*blocked}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{502: 12.5}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Equal(t, []int64{502}, repo.clearCalls)
	assert.Empty(t, repo.setCalls)
	assert.Equal(t, 1, summary.Recovered)
	assert.Zero(t, summary.Blocked)
}

// Scenario（红线）：账号带的是 **A7 冷却**（reason 前缀不同）而余额已恢复 →
// 本门**不得**清它的块（清了会让 404 浅冷却静默失效）。
// 这里刻意让 A7 的窗口已过期：账号仍会被探测（Checked=1），所以本断言覆盖的是
// **Clear 路径上的前缀守卫本身**，而不是"因为没探所以没清"。
func TestCodeBuddyZeroCreditGate_DoesNotClearForeignKeyedCooldown(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	cooled := zeroCreditAccount(503)
	expired := now.Add(-10 * time.Minute)
	cooled.TempUnschedulableUntil = &expired
	cooled.TempUnschedulableReason = "codebuddy_account_cooldown:not_found"
	repo := &zeroCreditRepoStub{accounts: []Account{*cooled}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{503: 88}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Equal(t, []int64{503}, prober.calls, "过期窗口的账号照常探测")
	assert.Empty(t, repo.clearCalls, "A7 的冷却只能由 A7 自己到期/清除")
	assert.Empty(t, repo.setCalls)
	assert.Zero(t, summary.Recovered)
	assert.Zero(t, summary.Blocked)
}

// Scenario：余额 > 0 且账号**没有**本门块（正常号）→ 不写任何状态
// （恢复路径只在真的被本门摘过时才动手，避免无意义的 outbox 事件）。
func TestCodeBuddyZeroCreditGate_HealthyAccountUntouched(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(507)}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{507: 3}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Empty(t, repo.setCalls)
	assert.Empty(t, repo.clearCalls)
	assert.Equal(t, 1, summary.Checked)
	assert.Zero(t, summary.Recovered)
}

// Scenario（票面）：余额查询失败 → **不作为**（fail-open；既不摘号也不解冻），
// 且该账号进入 2 个周期的退避（上游故障/单号凭据坏掉时不再每轮重试）。
func TestCodeBuddyZeroCreditGate_ProbeFailureIsInertAndBacksOff(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(504)}}
	prober := &zeroCreditProberStub{errs: map[int64]error{504: errors.New("upstream 502")}}
	gate := newZeroCreditGateForTest(repo, prober, now)

	first := gate.runOnce(context.Background())
	require.Equal(t, 1, first.Failed)
	require.Zero(t, first.Checked, "查失败不算已核对（口径：Checked=拿到余额）")
	assert.Empty(t, repo.setCalls, "查询失败绝不摘账号")
	assert.Empty(t, repo.clearCalls)
	require.Equal(t, []int64{504}, prober.calls)

	// 退避 2 个周期：接下来两轮跳过该账号（不再打上游），第 3 轮才重试。
	second := gate.runOnce(context.Background())
	third := gate.runOnce(context.Background())
	assert.Len(t, prober.calls, 1, "退避期内不得再探测")
	assert.Equal(t, 2, second.Skipped+third.Skipped)

	fourth := gate.runOnce(context.Background())
	require.Equal(t, []int64{504, 504}, prober.calls, "退避结束后重新探测")
	require.Equal(t, 1, fourth.Failed)

	// 探测成功后必须**清零退避**：走完这轮退避（两轮跳过）后的一次成功探测，
	// 其后的下一轮不得再被跳过——否则坏过一次的账号会被残留退避一直跳过。
	prober.errs = nil
	prober.balances = map[int64]float64{504: 0}
	gate.runOnce(context.Background()) // 退避第 1 轮：跳过
	gate.runOnce(context.Background()) // 退避第 2 轮：跳过
	success := gate.runOnce(context.Background())
	require.Equal(t, 1, success.Blocked, "退避走完后重新探测，0 分照样摘出")
	require.Len(t, prober.calls, 3)

	next := gate.runOnce(context.Background())
	require.Len(t, prober.calls, 4, "成功后不得残留退避")
	assert.Zero(t, next.Skipped)
}

// Scenario（红线）：余额**不可用**（查询报错之外的形态：Balance 缺失、
// status=error 的降级快照）一律按"没拿到余额"处理——绝不当作 0 分摘号。
func TestCodeBuddyZeroCreditGate_UnusableUsageIsInert(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	zero := 0.0
	cases := []struct {
		name  string
		usage *UpstreamBalanceUsage
	}{
		{"balance_missing", &UpstreamBalanceUsage{Unit: "credits", Status: "ok"}},
		{"degraded_status", &UpstreamBalanceUsage{Status: "error", Balance: &zero}},
		{"snapshot_nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(508)}}
			prober := &zeroCreditProberStub{usages: map[int64]*UpstreamBalanceUsage{508: tc.usage}}

			summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

			assert.Empty(t, repo.setCalls)
			assert.Empty(t, repo.clearCalls)
			assert.Equal(t, 1, summary.Failed)
			assert.Zero(t, summary.Checked)
		})
	}
}

// Scenario（票面）：非 codebuddy / 非 active / 无 access token 的账号一律不扫
// （红线：本门只动 codebuddy 账号；缺 token 的号探测必然失败，不值得发一次出站）。
func TestCodeBuddyZeroCreditGate_SkipsNonCodeBuddyAndUnprobeable(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	openai := zeroCreditAccount(600)
	openai.Platform = PlatformOpenAI
	disabled := zeroCreditAccount(601)
	disabled.Status = "disabled"
	noToken := zeroCreditAccount(602)
	noToken.Credentials = map[string]any{}
	target := zeroCreditAccount(603)

	repo := &zeroCreditRepoStub{accounts: []Account{*openai, *disabled, *noToken, *target}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{603: 0}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Equal(t, []int64{603}, prober.calls, "只探 codebuddy + active + 有 token 的号")
	require.Len(t, repo.setCalls, 1)
	assert.Equal(t, int64(603), repo.setCalls[0].accountID)
	assert.Equal(t, 1, summary.Checked)
}

// Scenario（票面「未在冷却」）：账号已在**别的子系统**的停调窗口内（A7 冷却 /
// 保活三振 / 运营手动）→ 本轮不探也不动：它已经不在选号池里，去留由那条通道
// 自己决定；本门绝不覆盖它写下的 reason。
// 对照：**本门**自己的块照常探测（续期/解冻都靠它，见其他用例）。
func TestCodeBuddyZeroCreditGate_SkipsAccountsInForeignKeyedCooldown(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	cooled := zeroCreditAccount(509)
	until := now.Add(6 * time.Hour)
	cooled.TempUnschedulableUntil = &until
	cooled.TempUnschedulableReason = "codebuddy_account_cooldown:quota_exhausted"
	repo := &zeroCreditRepoStub{accounts: []Account{*cooled}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{509: 0}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Empty(t, prober.calls, "别人的冷却窗口内不探测")
	assert.Empty(t, repo.setCalls, "绝不覆盖 A7 的 reason")
	assert.Empty(t, repo.clearCalls)
	assert.Equal(t, 1, summary.Skipped)
}

// Scenario（票面）：开关关闭 → 整轮空转（一个上游请求都不发、一次状态都不写）。
func TestCodeBuddyZeroCreditGate_DisabledSwitchIsInert(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(505)}}
	prober := &zeroCreditProberStub{balances: map[int64]float64{505: 0}}
	gate := newZeroCreditGateForTest(repo, prober, now)
	gate.cfg.Gateway.CodeBuddy.ZeroCreditGateEnabled = false

	gate.tick()

	assert.Empty(t, prober.calls)
	assert.Empty(t, repo.setCalls)
	assert.Empty(t, repo.clearCalls)
}

// Scenario：上游列表失败（DB 抖动）→ 本轮整体跳过，不 panic、不写任何状态。
func TestCodeBuddyZeroCreditGate_ListFailureIsInert(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	repo := &zeroCreditRepoStub{listErr: errors.New("db down")}
	prober := &zeroCreditProberStub{}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Zero(t, summary.Checked)
	assert.Empty(t, prober.calls)
}

// Scenario（装配可达性）：真 fetcher（httptest 假上游）走通「0 分摘出 → 恢复解冻」
// 整条链路——证明本门用的是 A2 的积分查询实现，而不是另写一套解析。
func TestCodeBuddyZeroCreditGate_RealFetcherEndToEnd(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	empty := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacityRemainPrecise":"0","CycleCapacityRemain":0,"CapacityRemain":0}
		]}}}}`))
	})
	repo := &zeroCreditRepoStub{accounts: []Account{*zeroCreditAccount(506)}}
	gate := newZeroCreditGateForTest(repo, NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(empty.URL), now)

	blocked := gate.runOnce(context.Background())
	require.Equal(t, 1, blocked.Blocked)
	require.Len(t, repo.setCalls, 1)
	assert.Equal(t, time.Date(2026, 10, 9, 4, 0, 0, 0, codeBuddyTimeZone), repo.setCalls[0].until)

	// 上游积分恢复（签到到账）→ 下一轮立即解冻。
	recovered := codeBuddyCreditsTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacityRemainPrecise":"120","CycleCapacityRemain":120,"CapacityRemain":120}
		]}}}}`))
	})
	gate.prober = NewCodeBuddyCreditsFetcher(nil).WithTestBaseURL(recovered.URL)

	// 模拟「DB 里的本门块被读回」：stub 无条件返回账号行。
	until := time.Date(2026, 10, 9, 4, 0, 0, 0, codeBuddyTimeZone)
	blockedAcct := zeroCreditAccount(506)
	blockedAcct.TempUnschedulableUntil = &until
	blockedAcct.TempUnschedulableReason = codeBuddyZeroCreditReasonPrefix
	repo.accounts = []Account{*blockedAcct}

	summary := gate.runOnce(context.Background())
	assert.Equal(t, 1, summary.Recovered)
	assert.Equal(t, []int64{506}, repo.clearCalls)
}

// Scenario：Start/Stop 生命周期——开关关闭不启动；启动后可 Stop（幂等）。
func TestCodeBuddyZeroCreditGate_StartStopLifecycle(t *testing.T) {
	repo := &zeroCreditRepoStub{}
	prober := &zeroCreditProberStub{}

	disabled := newZeroCreditGateForTest(repo, prober, time.Now())
	disabled.cfg.Gateway.CodeBuddy.ZeroCreditGateEnabled = false
	disabled.Start()
	assert.False(t, disabled.isStarted(), "开关关闭时不启动")
	require.NotPanics(t, disabled.Stop)

	enabled := newZeroCreditGateForTest(repo, prober, time.Now())
	enabled.Start()
	assert.True(t, enabled.isStarted())
	require.NotPanics(t, func() {
		enabled.Stop()
		enabled.Stop()
	})
}

// Scenario：装配可达性——Provider 必须真的把本门启动起来并返回可 Stop 的实例，
// 否则票 #37 会静默变成死代码（同 A4 批 4.6 的装配教训）。
func TestProvideCodeBuddyZeroCreditGateStartsGate(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{
		ZeroCreditGateEnabled:          true,
		ZeroCreditCheckIntervalMinutes: 30,
	}}}
	gate := ProvideCodeBuddyZeroCreditGate(&zeroCreditRepoStub{}, nil, cfg)
	require.NotNil(t, gate)
	t.Cleanup(gate.Stop)

	assert.True(t, gate.isStarted(), "Provider 会 Start（与既有 Provide* 先例一致）")
	assert.Equal(t, 30*time.Minute, gate.interval)
}

// Scenario：间隔配置 <=0 → 回落默认 30 分钟（关门的开关是 enabled，不是把周期
// 设成 0——避免"键没配"与"键配成 0"两种状态无法区分）。
func TestProvideCodeBuddyZeroCreditGateFallsBackToDefaultInterval(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{
		ZeroCreditGateEnabled: true,
	}}}
	gate := ProvideCodeBuddyZeroCreditGate(&zeroCreditRepoStub{}, nil, cfg)
	require.NotNil(t, gate)
	t.Cleanup(gate.Stop)

	assert.Equal(t, codeBuddyZeroCreditDefaultIntervalMinutes*time.Minute, gate.interval)
	assert.True(t, gate.isStarted())
}

// Scenario：配置层关闭 / 依赖缺失时不得启动、不得 panic（回滚与防御路径）。
func TestProvideCodeBuddyZeroCreditGateDisabledAndNilDeps(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{
		ZeroCreditGateEnabled:          false,
		ZeroCreditCheckIntervalMinutes: 30,
	}}}
	gate := ProvideCodeBuddyZeroCreditGate(&zeroCreditRepoStub{}, nil, cfg)
	require.NotNil(t, gate)
	assert.False(t, gate.isStarted(), "开关关闭时不启动")
	require.NotPanics(t, gate.Stop)

	nilRepo := NewCodeBuddyZeroCreditGate(nil, &zeroCreditProberStub{}, cfg, time.Minute)
	assert.False(t, nilRepo.isStarted())
	require.NotPanics(t, func() {
		nilRepo.Start()
		nilRepo.Stop()
	})
	require.NotPanics(t, func() { ProvideCodeBuddyZeroCreditGate(nil, nil, nil).Stop() })
}

// Scenario：reason 前缀判定只认本门（大小写敏感、无 until 不算在冷却）。
func TestCodeBuddyZeroCreditOwnBlockDetection(t *testing.T) {
	until := time.Now().Add(time.Hour)
	own := &Account{Platform: PlatformCodeBuddy, TempUnschedulableUntil: &until,
		TempUnschedulableReason: codeBuddyZeroCreditReasonPrefix}
	assert.True(t, own.HasCodeBuddyZeroCreditBlock())

	other := &Account{Platform: PlatformCodeBuddy, TempUnschedulableUntil: &until,
		TempUnschedulableReason: "codebuddy_account_cooldown:not_found"}
	assert.False(t, other.HasCodeBuddyZeroCreditBlock())

	upper := &Account{Platform: PlatformCodeBuddy, TempUnschedulableUntil: &until,
		TempUnschedulableReason: strings.ToUpper(codeBuddyZeroCreditReasonPrefix)}
	assert.False(t, upper.HasCodeBuddyZeroCreditBlock(), "前缀判定区分大小写")

	noWindow := &Account{Platform: PlatformCodeBuddy, TempUnschedulableReason: codeBuddyZeroCreditReasonPrefix}
	assert.False(t, noWindow.HasCodeBuddyZeroCreditBlock(), "无 until 不算在冷却")

	foreign := &Account{Platform: PlatformOpenAI, TempUnschedulableUntil: &until,
		TempUnschedulableReason: codeBuddyZeroCreditReasonPrefix}
	assert.False(t, foreign.HasCodeBuddyZeroCreditBlock(), "非 codebuddy 平台不认")
}

// Scenario：DB 写失败（停调 / 清除）→ 不虚报计数，也不中断整轮
// （单号写失败不该让后面的账号这一轮都不被处理）。
func TestCodeBuddyZeroCreditGate_WriteFailureDoesNotAbortRound(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, codeBuddyTimeZone)
	blockedAcct := zeroCreditAccount(510)
	until := time.Date(2026, 10, 9, 4, 0, 0, 0, codeBuddyTimeZone)
	blockedAcct.TempUnschedulableUntil = &until
	blockedAcct.TempUnschedulableReason = codeBuddyZeroCreditReasonPrefix

	repo := &zeroCreditRepoStub{
		accounts: []Account{*zeroCreditAccount(511), *blockedAcct, *zeroCreditAccount(512)},
		setErr:   errors.New("db down"),
		clearErr: errors.New("db down"),
	}
	prober := &zeroCreditProberStub{balances: map[int64]float64{511: 0, 510: 42, 512: 0}}

	summary := newZeroCreditGateForTest(repo, prober, now).runOnce(context.Background())

	assert.Equal(t, 3, summary.Checked, "写失败不影响同轮其他账号")
	assert.Zero(t, summary.Blocked, "写失败不得虚报已摘出")
	assert.Zero(t, summary.Recovered, "清失败不得虚报已恢复")
	require.Len(t, repo.setCalls, 2, "两个 0 分账号都被尝试过")
	assert.Equal(t, []int64{511, 512}, []int64{repo.setCalls[0].accountID, repo.setCalls[1].accountID})
	assert.Equal(t, []int64{510}, repo.clearCalls, "恢复路径同样尝试过")
}

// 未使用的桩方法显式占位：AccountRepository 是宽接口，桩只实现用到的方法。
var _ AccountRepository = (*zeroCreditRepoStub)(nil)
var _ codeBuddyCreditsProber = (*zeroCreditProberStub)(nil)
