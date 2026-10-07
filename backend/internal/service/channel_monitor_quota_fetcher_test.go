//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

// --- fetcher 依赖 stub ---

type stubMonitorUsageSource struct {
	usage *UsageInfo
	err   error
	// block 非 nil 时 GetUsageForAccount 阻塞在该 channel 上，用于并发/singleflight 测试。
	block chan struct{}

	mu          sync.Mutex
	calls       int
	lastCtx     context.Context
	lastAccount *Account
}

func (s *stubMonitorUsageSource) GetUsageForAccount(ctx context.Context, account *Account, force ...bool) (*UsageInfo, error) {
	s.mu.Lock()
	s.calls++
	s.lastCtx = ctx
	s.lastAccount = account
	s.mu.Unlock()
	if s.block != nil {
		<-s.block
	}
	return s.usage, s.err
}

func (s *stubMonitorUsageSource) getCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubMonitorUsageSource) getLastAccount() *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccount
}

type stubMonitorCNQuotaSource struct {
	result      *CNProviderQuotaProbeResult
	err         error
	calls       int
	lastAccount *Account
}

func (s *stubMonitorCNQuotaSource) QueryUsageForAccount(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error) {
	s.calls++
	s.lastAccount = account
	return s.result, s.err
}

type stubMonitorCNBalanceSource struct {
	result      *CNProviderBalanceResult
	err         error
	calls       int
	lastAccount *Account
}

func (s *stubMonitorCNBalanceSource) QueryBalanceForAccount(ctx context.Context, account *Account) (*CNProviderBalanceResult, error) {
	s.calls++
	s.lastAccount = account
	return s.result, s.err
}

type stubMonitorAccountSource struct {
	accounts map[int64]*Account
	err      error
	calls    int
	// extraErr 让 UpdateExtra 失败（写侧静默降级的用例）。
	extraErr error

	mu          sync.Mutex
	extraWrites []monitorExtraWrite
}

// monitorExtraWrite 记录一次 extra 回写（形状对齐 keeperExtraWrite 先例）。
type monitorExtraWrite struct {
	id      int64
	updates map[string]any
}

func (s *stubMonitorAccountSource) GetByID(ctx context.Context, id int64) (*Account, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.accounts[id], nil
}

// UpdateExtra 是重置卡余量的 extra 落库口（票 12 徽标写入方）：记录回写内容，
// 便于断言「写了什么/没写/写失败」三类行为。
func (s *stubMonitorAccountSource) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := make(map[string]any, len(updates))
	for k, v := range updates {
		clone[k] = v
	}
	s.extraWrites = append(s.extraWrites, monitorExtraWrite{id: id, updates: clone})
	return s.extraErr
}

func (s *stubMonitorAccountSource) getExtraWrites() []monitorExtraWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]monitorExtraWrite(nil), s.extraWrites...)
}

// stubMonitorZhipuLoginSource 桩：schedule 08 的 credit-usage 探针 + 票 12 的重置卡
// 只读读取（两个 fetch 方法与真实 ZhipuAccountMonitorService 的签名一致）。
type stubMonitorZhipuLoginSource struct {
	credits []domain.MonitorQuotaModelCredit
	err     error
	// cards / cardsErr 是重置卡只读读取（票 12 接线）的返回值。
	cards    []domain.MonitorResetCard
	cardsErr error
	// block 非 nil 时阻塞在该 channel 上，用于并发/超时测试。
	block chan struct{}

	mu          sync.Mutex
	calls       int
	lastAccount *Account
	lastStart   time.Time
	lastEnd     time.Time
	resetCalls  int
	resetSource *Account
}

func (s *stubMonitorZhipuLoginSource) FetchUsageDetailForAccount(ctx context.Context, account *Account, start, end time.Time) ([]domain.MonitorQuotaModelCredit, error) {
	s.mu.Lock()
	s.calls++
	s.lastAccount = account
	s.lastStart = start
	s.lastEnd = end
	s.mu.Unlock()
	if s.block != nil {
		<-s.block
	}
	return s.credits, s.err
}

// FetchResetStatusForAccount 是票 12 接线的第二个方法：只返回展示字段（R0 只读）。
func (s *stubMonitorZhipuLoginSource) FetchResetStatusForAccount(_ context.Context, account *Account) ([]domain.MonitorResetCard, error) {
	s.mu.Lock()
	s.resetCalls++
	s.resetSource = account
	s.mu.Unlock()
	return s.cards, s.cardsErr
}

func (s *stubMonitorZhipuLoginSource) getResetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetCalls
}

func (s *stubMonitorZhipuLoginSource) getResetSource() *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetSource
}

func (s *stubMonitorZhipuLoginSource) getCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubMonitorZhipuLoginSource) window() (time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStart, s.lastEnd
}

func (s *stubMonitorZhipuLoginSource) getLastAccount() *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccount
}

func newQuotaFetcherTestSetup(t *testing.T) (*ChannelMonitorQuotaFetcher, *stubMonitorUsageSource, *stubMonitorCNQuotaSource, *stubMonitorCNBalanceSource, *stubMonitorAccountSource) {
	t.Helper()
	usage := &stubMonitorUsageSource{}
	cnQuota := &stubMonitorCNQuotaSource{}
	cnBalance := &stubMonitorCNBalanceSource{}
	accounts := &stubMonitorAccountSource{accounts: make(map[int64]*Account)}
	fetcher := &ChannelMonitorQuotaFetcher{
		usage:            usage,
		cnQuota:          cnQuota,
		cnBalance:        cnBalance,
		accounts:         accounts,
		balanceThreshold: monitorBalanceThreshold(nil),
		cache:            make(map[int64]monitorQuotaCacheEntry),
	}
	return fetcher, usage, cnQuota, cnBalance, accounts
}

// --- 分派 ---

func TestQuotaFetcher_OverseasAccountUsesUsageService(t *testing.T) {
	fetcher, usage, _, cnQuota, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[7] = &Account{ID: 7, Platform: domain.PlatformAnthropic}
	resets := time.Now().Add(2 * time.Hour).UTC()
	usage.usage = &UsageInfo{
		FiveHour:         &UsageProgress{Utilization: 42.5, UsedRequests: 17, LimitRequests: 40, ResetsAt: &resets},
		SevenDay:         &UsageProgress{Utilization: 10},
		SubscriptionTier: "PRO",
	}

	snapshot := fetcher.Fetch(context.Background(), 7)

	require.True(t, snapshot.Success)
	require.Equal(t, "usage", snapshot.Source)
	require.Equal(t, "PRO", snapshot.PlanLevel)
	require.False(t, snapshot.CredentialInvalid)
	require.Empty(t, snapshot.Error)
	require.Len(t, snapshot.Tiers, 2)

	fiveHour := snapshot.Tiers[0]
	require.Equal(t, "5h", fiveHour.Window)
	require.Empty(t, fiveHour.Label)
	require.InDelta(t, 42.5, fiveHour.UsedPercent, 0.001)
	require.Equal(t, float64(17), fiveHour.Used)
	require.Equal(t, float64(40), fiveHour.Limit)
	require.NotEmpty(t, fiveHour.ResetAt)

	require.Equal(t, "7d", snapshot.Tiers[1].Window)
	require.Equal(t, 1, usage.getCalls())
	require.Equal(t, 0, cnQuota.calls)
}

func TestQuotaFetcher_CodingPlanAccountUsesCNQuota(t *testing.T) {
	fetcher, _, cnQuota, cnBalance, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[9] = &Account{
		ID:          9,
		Platform:    domain.PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
	}
	cnQuota.result = &CNProviderQuotaProbeResult{
		Success:         true,
		CredentialValid: true,
		PlanLevel:       "",
		Tiers: []CNQuotaTier{
			{Window: "5h", UsedPercent: 33.3, ResetAt: "2026-08-18T06:00:00Z"},
			{Window: "weekly", UsedPercent: 12},
		},
	}

	snapshot := fetcher.Fetch(context.Background(), 9)

	require.True(t, snapshot.Success)
	require.Equal(t, "cn_quota", snapshot.Source)
	require.Len(t, snapshot.Tiers, 2)
	require.Equal(t, "5h", snapshot.Tiers[0].Window)
	require.InDelta(t, 33.3, snapshot.Tiers[0].UsedPercent, 0.001)
	require.Equal(t, "weekly", snapshot.Tiers[1].Window)
	require.Equal(t, 1, cnQuota.calls)
	require.Equal(t, 0, cnBalance.calls)
}

func TestQuotaFetcher_MiniMaxCodingPlanUsesCNQuota(t *testing.T) {
	fetcher, usage, cnQuota, cnBalance, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[19] = &Account{
		ID:          19,
		Platform:    domain.PlatformMiniMax,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
	}
	cnQuota.result = &CNProviderQuotaProbeResult{
		Success:         true,
		CredentialValid: true,
		Tiers: []CNQuotaTier{
			{Window: "5h", UsedPercent: 12},
		},
	}

	snapshot := fetcher.Fetch(context.Background(), 19)

	require.True(t, snapshot.Success)
	require.Equal(t, "cn_quota", snapshot.Source)
	require.Equal(t, 1, cnQuota.calls)
	require.Equal(t, 0, cnBalance.calls)
	require.Equal(t, 0, usage.getCalls())
}

func TestQuotaFetcher_PayGAccountUsesCNBalance(t *testing.T) {
	fetcher, _, _, cnBalance, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[11] = &Account{
		ID:          11,
		Platform:    domain.PlatformDeepseek,
		Credentials: map[string]any{"account_mode": AccountModePayG},
	}
	cnBalance.result = &CNProviderBalanceResult{
		Success:   true,
		Available: true,
		Balance:   12.34,
		Currency:  "CNY",
		Balances: []CNProviderBalanceEntry{
			{Currency: "CNY", Balance: 12.34},
			{Currency: "USD", Balance: 1.5},
		},
	}

	snapshot := fetcher.Fetch(context.Background(), 11)

	require.True(t, snapshot.Success)
	require.Equal(t, "cn_balance", snapshot.Source)
	require.NotNil(t, snapshot.Balance)
	require.InDelta(t, 12.34, *snapshot.Balance, 0.001)
	require.Equal(t, "CNY", snapshot.Currency)
	require.Len(t, snapshot.Balances, 2)
	require.Equal(t, "USD", snapshot.Balances[1].Currency)
	require.False(t, snapshot.BalanceLow)
	require.Empty(t, snapshot.Error)
}

// P2-6：fetchUncached 只 GetByID 一次，已加载的 account 指针直传数据源，
// 三条路由都不能让下游重载账号。
func TestQuotaFetcher_LoadsAccountOnceAndPassesItThrough(t *testing.T) {
	t.Run("overseas usage", func(t *testing.T) {
		fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
		acc := &Account{ID: 21, Platform: domain.PlatformAnthropic}
		accounts.accounts[21] = acc
		usage.usage = &UsageInfo{}

		fetcher.Fetch(context.Background(), 21)

		require.Equal(t, 1, accounts.calls)
		require.Same(t, acc, usage.getLastAccount())
		require.Equal(t, 1, usage.getCalls())
	})

	t.Run("cn coding plan", func(t *testing.T) {
		fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
		acc := &Account{ID: 22, Platform: domain.PlatformKimi, Credentials: map[string]any{"account_mode": AccountModeCoding}}
		accounts.accounts[22] = acc
		cnQuota.result = &CNProviderQuotaProbeResult{Success: true}

		fetcher.Fetch(context.Background(), 22)

		require.Equal(t, 1, accounts.calls)
		require.Same(t, acc, cnQuota.lastAccount)
		require.Equal(t, 1, cnQuota.calls)
	})

	t.Run("cn payg", func(t *testing.T) {
		fetcher, _, _, cnBalance, accounts := newQuotaFetcherTestSetup(t)
		acc := &Account{ID: 23, Platform: domain.PlatformDeepseek, Credentials: map[string]any{"account_mode": AccountModePayG}}
		accounts.accounts[23] = acc
		cnBalance.result = &CNProviderBalanceResult{Success: true, Available: true, Balance: 1, Currency: "CNY"}

		fetcher.Fetch(context.Background(), 23)

		require.Equal(t, 1, accounts.calls)
		require.Same(t, acc, cnBalance.lastAccount)
		require.Equal(t, 1, cnBalance.calls)
	})
}

// --- 第四数据源：登录态智谱账号的积分明细 / 重登状态（design M4 / 票 11）---

// zhipuLoginManagedQuotaAccount 构造一条「登录托管」智谱 coding 账号（票 03 契约：
// platform=zhipu + type=apikey + credentials.auth_flow=bigmodel_oauth）。
func zhipuLoginManagedQuotaAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID:       id,
		Platform: domain.PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode": AccountModeCoding,
			"auth_flow":    ZhipuLoginAuthFlow,
		},
		Extra: extra,
	}
}

func TestQuotaFetcher_ZhipuLoginManagedMergesCreditsAndRelogin(t *testing.T) {
	fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
	managed := zhipuLoginManagedQuotaAccount(31, map[string]any{ZhipuNeedsReloginExtraKey: true})
	accounts.accounts[31] = managed
	cnQuota.result = &CNProviderQuotaProbeResult{
		Success:   true,
		PlanLevel: "coding",
		Tiers:     []CNQuotaTier{{Window: "5h", UsedPercent: 33.3}, {Window: "weekly", UsedPercent: 12}},
	}
	credits := []domain.MonitorQuotaModelCredit{
		{Model: "glm-5.3", Date: "2026-10-05", InputTokens: 1200, CachedTokens: 340, OutputTokens: 560, Credits: 1.25},
		{Model: "glm-4.6-flash", Date: "2026-10-06", InputTokens: 20, CachedTokens: 0, OutputTokens: 5, Credits: 0.01},
	}
	zhipuLogin := &stubMonitorZhipuLoginSource{credits: credits}
	fetcher.zhipuLogin = zhipuLogin

	snapshot := fetcher.Fetch(context.Background(), 31)

	// 既有 CN 配额结果原样保留。
	require.True(t, snapshot.Success)
	require.Equal(t, "cn_quota", snapshot.Source)
	require.Equal(t, "coding", snapshot.PlanLevel)
	require.Len(t, snapshot.Tiers, 2)
	require.Equal(t, "weekly", snapshot.Tiers[1].Window)

	// 新字段并入同一快照。
	require.Equal(t, credits, snapshot.ModelCredits)
	require.True(t, snapshot.NeedsRelogin)
	require.Empty(t, snapshot.Error)

	// 数据源收到的是已加载的账号指针，窗口为近 7 个自然日。
	require.Equal(t, 1, zhipuLogin.getCalls())
	require.Same(t, managed, zhipuLogin.getLastAccount())
	start, end := zhipuLogin.window()
	require.WithinDuration(t, start.Add(6*24*time.Hour), end, time.Hour)
	require.WithinDuration(t, time.Now(), end, time.Minute)

	// 快照进 TTL 缓存：第二次 Fetch 不再打监控数据源。
	require.Equal(t, credits, fetcher.Fetch(context.Background(), 31).ModelCredits)
	require.Equal(t, 1, zhipuLogin.getCalls())
}

// 部分失败：第四数据源失败只缺字段，Success/tiers/Error 全由既有 CN 结果决定；
// 连 401/403 形状的错误也不改变数据面判定（CredentialInvalid 是数据面结论）。
func TestQuotaFetcher_ZhipuLoginSourceFailureOnlyDropsFields(t *testing.T) {
	cases := []struct {
		name             string
		source           *stubMonitorZhipuLoginSource
		extra            map[string]any
		wantNeedsRelogin bool
	}{
		{
			name:   "probe transport error",
			source: &stubMonitorZhipuLoginSource{err: errors.New("zhipu credit usage: connection refused")},
		},
		{
			name:             "probe unauthorized",
			source:           &stubMonitorZhipuLoginSource{err: errors.New("zhipu credit usage: upstream rejected credentials: HTTP 401")},
			extra:            map[string]any{ZhipuNeedsReloginExtraKey: true},
			wantNeedsRelogin: true,
		},
		{
			name:   "empty detail keeps field absent",
			source: &stubMonitorZhipuLoginSource{credits: []domain.MonitorQuotaModelCredit{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
			accounts.accounts[33] = zhipuLoginManagedQuotaAccount(33, tc.extra)
			cnQuota.result = &CNProviderQuotaProbeResult{Success: true, Tiers: []CNQuotaTier{{Window: "5h", UsedPercent: 20}}}
			fetcher.zhipuLogin = tc.source

			snapshot := fetcher.Fetch(context.Background(), 33)

			require.True(t, snapshot.Success, "Success 仍由既有 CN 配额结果决定")
			require.Empty(t, snapshot.Error)
			require.False(t, snapshot.CredentialInvalid)
			require.Len(t, snapshot.Tiers, 1)
			require.Nil(t, snapshot.ModelCredits)
			require.Equal(t, tc.wantNeedsRelogin, snapshot.NeedsRelogin)
			require.Equal(t, 1, tc.source.getCalls())
			require.Equal(t, MonitorStatusOperational, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)
		})
	}
}

// CN 配额本身失败时，第四数据源失败不得改写既有 Error/状态。
func TestQuotaFetcher_ZhipuLoginSourceFailureKeepsCNError(t *testing.T) {
	fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[35] = zhipuLoginManagedQuotaAccount(35, nil)
	cnQuota.result = &CNProviderQuotaProbeResult{Success: false, StatusCode: 500, Error: "cn quota boom"}
	fetcher.zhipuLogin = &stubMonitorZhipuLoginSource{err: errors.New("credit usage exploded")}

	snapshot := fetcher.Fetch(context.Background(), 35)

	require.False(t, snapshot.Success)
	require.Equal(t, "cn quota boom", snapshot.Error)
	require.False(t, snapshot.CredentialInvalid)
	require.Nil(t, snapshot.ModelCredits)
	require.Equal(t, MonitorStatusError, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)
}

// --- 重置卡只读余量并入快照（票 12 接线；R0：仅观测，永不使用）---

// 合并：重置卡读取成功时快照带 reset_cards，且与积分明细共用同一次账号加载。
func TestQuotaFetcher_ZhipuLoginManagedMergesResetCards(t *testing.T) {
	fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
	managed := zhipuLoginManagedQuotaAccount(36, nil)
	accounts.accounts[36] = managed
	cnQuota.result = &CNProviderQuotaProbeResult{
		Success:   true,
		PlanLevel: "coding",
		Tiers:     []CNQuotaTier{{Window: "5h", UsedPercent: 33.3}},
	}
	cards := []domain.MonitorResetCard{
		{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"},
		{Type: "week", ExpireAt: "2026-11-05T09:20:00Z"},
	}
	zhipuLogin := &stubMonitorZhipuLoginSource{
		credits: []domain.MonitorQuotaModelCredit{{Model: "glm-5.3", Date: "2026-10-06", Credits: 1.25}},
		cards:   cards,
	}
	fetcher.zhipuLogin = zhipuLogin

	snapshot := fetcher.Fetch(context.Background(), 36)

	// 既有 CN 配额结果原样保留，只读字段并入。
	require.True(t, snapshot.Success)
	require.Equal(t, "cn_quota", snapshot.Source)
	require.Equal(t, cards, snapshot.ResetCards)
	require.Equal(t, zhipuLogin.credits, snapshot.ModelCredits)
	require.Empty(t, snapshot.Error)
	require.False(t, snapshot.NeedsRelogin)
	require.Equal(t, MonitorStatusOperational, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)

	// 重置卡读取收到的是同一次加载的账号指针（fetcher 只 GetByID 一次）。
	require.Equal(t, 1, zhipuLogin.getResetCalls())
	require.Same(t, managed, zhipuLogin.getResetSource())

	// 快照进 TTL 缓存：第二次 Fetch 不再打监控数据源。
	require.Equal(t, cards, fetcher.Fetch(context.Background(), 36).ResetCards)
	require.Equal(t, 1, zhipuLogin.getResetCalls())
}

// 部分失败：重置卡读取失败只让 reset_cards 缺省，Success/Error/状态与积分明细都不受影响
// （两个数据源相互独立：重置卡失败不得挡住已成功的积分明细）。
func TestQuotaFetcher_ZhipuResetCardSourceFailureOnlyDropsFields(t *testing.T) {
	cases := []struct {
		name  string
		cards []domain.MonitorResetCard
		err   error
	}{
		{
			name: "probe transport error",
			err:  errors.New("bigmodel reset_status: connection refused"),
		},
		{
			name: "risk client backoff",
			err:  errors.New("bigmodel risk client: endpoint \"zcode_reset_status\" blocked (backoff)"),
		},
		{
			name:  "empty pools keep the field absent",
			cards: []domain.MonitorResetCard{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
			accounts.accounts[37] = zhipuLoginManagedQuotaAccount(37, nil)
			cnQuota.result = &CNProviderQuotaProbeResult{Success: true, Tiers: []CNQuotaTier{{Window: "5h", UsedPercent: 20}}}
			fetcher.zhipuLogin = &stubMonitorZhipuLoginSource{
				credits:  []domain.MonitorQuotaModelCredit{{Model: "glm-5.3", Date: "2026-10-06", Credits: 2}},
				cards:    tc.cards,
				cardsErr: tc.err,
			}

			snapshot := fetcher.Fetch(context.Background(), 37)

			require.True(t, snapshot.Success, "Success 仍由既有 CN 配额结果决定")
			require.Empty(t, snapshot.Error)
			require.False(t, snapshot.CredentialInvalid)
			require.Nil(t, snapshot.ResetCards)
			// 独立性：重置卡失败不吞掉积分明细。
			require.Equal(t, []domain.MonitorQuotaModelCredit{{Model: "glm-5.3", Date: "2026-10-06", Credits: 2}}, snapshot.ModelCredits)
			require.Equal(t, MonitorStatusOperational, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)
		})
	}
}

// --- 重置卡余量回写账号 extra（票 12 徽标写入方；R0：只写展示数据）---

// 首次采集：读到可用重置卡后 best-effort 回写 accounts.extra 的 zhipu_reset_cards。
// 该键是账号卡片徽标（CNProviderQuotaCell）的读取口径 `<platform>_reset_cards`，
// 值形状与监控快照的 reset_cards 同形：[{type, expire_at}]。
func TestQuotaFetcher_ZhipuResetCardsExtraWrittenOnFirstFetch(t *testing.T) {
	fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[41] = zhipuLoginManagedQuotaAccount(41, nil)
	cnQuota.result = &CNProviderQuotaProbeResult{Success: true, Tiers: []CNQuotaTier{{Window: "5h", UsedPercent: 10}}}
	cards := []domain.MonitorResetCard{
		{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"},
		{Type: "week", ExpireAt: "2026-11-05T09:20:00Z"},
	}
	fetcher.zhipuLogin = &stubMonitorZhipuLoginSource{cards: cards}

	snapshot := fetcher.Fetch(context.Background(), 41)

	require.Equal(t, cards, snapshot.ResetCards)
	writes := accounts.getExtraWrites()
	require.Len(t, writes, 1, "首次采集应回写一次 extra")
	require.Equal(t, int64(41), writes[0].id)
	require.Len(t, writes[0].updates, 1, "只写 zhipu_reset_cards，不带其它键")
	raw, err := json.Marshal(writes[0].updates[ZhipuResetCardsExtraKey])
	require.NoError(t, err)
	// 前端读数口径（CNProviderQuotaCell.spec.ts）：数组，条目 {type, expire_at}。
	require.JSONEq(t, `[
		{"type": "five_hour", "expire_at": "2026-10-06T12:00:00Z"},
		{"type": "week", "expire_at": "2026-11-05T09:20:00Z"}
	]`, string(raw))

	// 快照进 TTL 缓存：第二次 Fetch 不再抓取，也就不再回写。
	require.Equal(t, cards, fetcher.Fetch(context.Background(), 41).ResetCards)
	require.Len(t, accounts.getExtraWrites(), 1)
}

// 回写时机：只在值变化时写；值未变化 / 空池 / 探针失败都不刷库，
// 也不删除 extra 里的最后已知值（保持最后已知状态）。
func TestQuotaFetcher_ZhipuResetCardsExtraWriteTiming(t *testing.T) {
	// extra 里已存值的落库形状：JSONB 往返成 []any + map[string]any
	// （与 accountEntityToService 的 copyJSONMap 一致，不是 Go 结构体）。
	storedCards := []any{
		map[string]any{"type": "five_hour", "expire_at": "2026-10-06T12:00:00Z"},
		map[string]any{"type": "week", "expire_at": "2026-11-05T09:20:00Z"},
	}
	sameCards := []domain.MonitorResetCard{
		{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"},
		{Type: "week", ExpireAt: "2026-11-05T09:20:00Z"},
	}
	newerCards := []domain.MonitorResetCard{{Type: "week", ExpireAt: "2026-12-01T00:00:00Z"}}
	cases := []struct {
		name      string
		stored    any
		cards     []domain.MonitorResetCard
		err       error
		wantWrite bool
	}{
		{name: "unchanged value skips the write", stored: storedCards, cards: sameCards},
		{name: "changed value overwrites the stale snapshot", stored: storedCards, cards: newerCards, wantWrite: true},
		{name: "malformed stored value is replaced by the fresh snapshot", stored: "two", cards: newerCards, wantWrite: true},
		{name: "empty pools keep the last known value", stored: storedCards},
		{name: "reset probe failure keeps the last known value", stored: storedCards, err: errors.New("bigmodel reset_status: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
			account := zhipuLoginManagedQuotaAccount(43, map[string]any{ZhipuResetCardsExtraKey: tc.stored})
			accounts.accounts[43] = account
			cnQuota.result = &CNProviderQuotaProbeResult{Success: true, Tiers: []CNQuotaTier{{Window: "5h", UsedPercent: 10}}}
			fetcher.zhipuLogin = &stubMonitorZhipuLoginSource{cards: tc.cards, cardsErr: tc.err}

			snapshot := fetcher.Fetch(context.Background(), 43)

			require.True(t, snapshot.Success)
			writes := accounts.getExtraWrites()
			if !tc.wantWrite {
				require.Empty(t, writes, "值未变化/空池/探针失败都不刷库")
				require.Equal(t, tc.stored, account.Extra[ZhipuResetCardsExtraKey],
					"旧值必须原样保留（保持最后已知状态）")
				return
			}
			require.Len(t, writes, 1, "值变化或旧值形状不对时用新快照覆盖")
			require.Equal(t, int64(43), writes[0].id)
			require.Equal(t, tc.cards, writes[0].updates[ZhipuResetCardsExtraKey])
		})
	}
}

// 写失败静默：extra 回写报错只留 debug 日志，快照（reset_cards 与状态判定）照常返回。
func TestQuotaFetcher_ZhipuResetCardsExtraWriteFailureKeepsSnapshot(t *testing.T) {
	fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[45] = zhipuLoginManagedQuotaAccount(45, nil)
	accounts.extraErr = errors.New("extra write boom")
	cnQuota.result = &CNProviderQuotaProbeResult{
		Success:   true,
		PlanLevel: "coding",
		Tiers:     []CNQuotaTier{{Window: "5h", UsedPercent: 10}},
	}
	cards := []domain.MonitorResetCard{{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"}}
	fetcher.zhipuLogin = &stubMonitorZhipuLoginSource{cards: cards}

	snapshot := fetcher.Fetch(context.Background(), 45)

	require.Len(t, accounts.getExtraWrites(), 1, "首次采集仍会尝试回写")
	require.Equal(t, cards, snapshot.ResetCards, "写失败不得吞掉快照字段")
	require.True(t, snapshot.Success)
	require.Empty(t, snapshot.Error)
	require.False(t, snapshot.CredentialInvalid)
	require.Equal(t, MonitorStatusOperational, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)
}

// 未接线/回滚：第四数据源为 nil 时登录态账号只补 extra 标记，
// Success/tiers 与既有三源现状一致，不 panic。
func TestQuotaFetcher_ZhipuLoginManagedWithoutSourceKeepsReloginOnly(t *testing.T) {
	fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[34] = zhipuLoginManagedQuotaAccount(34, map[string]any{ZhipuNeedsReloginExtraKey: true})
	cnQuota.result = &CNProviderQuotaProbeResult{Success: true, Tiers: []CNQuotaTier{{Window: "5h", UsedPercent: 20}}}
	require.Nil(t, fetcher.zhipuLogin)

	snapshot := fetcher.Fetch(context.Background(), 34)

	require.True(t, snapshot.Success)
	require.Len(t, snapshot.Tiers, 1)
	require.Nil(t, snapshot.ModelCredits)
	require.Nil(t, snapshot.ResetCards, "未接线时重置卡字段保持缺省")
	require.True(t, snapshot.NeedsRelogin)
}

// 非登录态账号（存量手填 apikey 的 zhipu / kimi / deepseek / 海外平台）走原路径：
// 第四数据源调用计数恒为 0，新字段保持缺省（返回字段与现状等价）。
func TestQuotaFetcher_NonZhipuLoginAccountsSkipFourthSource(t *testing.T) {
	cases := []struct {
		name      string
		accountID int64
		account   *Account
	}{
		{
			// 存量手填 apikey：即使 extra 里有重登标记也不得读（标记只对托管账号有意义）。
			name:      "legacy zhipu api key coding plan",
			accountID: 51,
			account: &Account{
				ID: 51, Platform: domain.PlatformZhipu, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"account_mode": AccountModeCoding, "api_key": "sk-legacy"},
				Extra:       map[string]any{ZhipuNeedsReloginExtraKey: true},
			},
		},
		{
			name:      "legacy zhipu api key payg",
			accountID: 52,
			account: &Account{
				ID: 52, Platform: domain.PlatformZhipu, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"account_mode": AccountModePayG},
			},
		},
		{
			// 登录标记存在但 type 不是 apikey（非托管）：同样跳过。
			name:      "zhipu oauth type without managed marker",
			accountID: 53,
			account: &Account{
				ID: 53, Platform: domain.PlatformZhipu, Type: AccountTypeOAuth,
				Credentials: map[string]any{"account_mode": AccountModeCoding, "auth_flow": ZhipuLoginAuthFlow},
			},
		},
		{
			name:      "kimi coding plan",
			accountID: 54,
			account: &Account{
				ID: 54, Platform: domain.PlatformKimi,
				Credentials: map[string]any{"account_mode": AccountModeCoding},
				Extra:       map[string]any{ZhipuNeedsReloginExtraKey: true},
			},
		},
		{
			name:      "deepseek payg",
			accountID: 55,
			account: &Account{
				ID: 55, Platform: domain.PlatformDeepseek,
				Credentials: map[string]any{"account_mode": AccountModePayG},
			},
		},
		{
			name:      "overseas anthropic",
			accountID: 56,
			account:   &Account{ID: 56, Platform: domain.PlatformAnthropic},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher, usage, cnQuota, cnBalance, accounts := newQuotaFetcherTestSetup(t)
			accounts.accounts[tc.accountID] = tc.account
			usage.usage = &UsageInfo{}
			cnQuota.result = &CNProviderQuotaProbeResult{Success: true}
			cnBalance.result = &CNProviderBalanceResult{Success: true, Available: true, Balance: 5, Currency: "CNY"}
			source := &stubMonitorZhipuLoginSource{credits: []domain.MonitorQuotaModelCredit{
				{Model: "glm-5.3", Date: "2026-10-06", Credits: 9},
			}}
			fetcher.zhipuLogin = source

			snapshot := fetcher.Fetch(context.Background(), tc.accountID)

			require.True(t, snapshot.Success)
			require.Equal(t, 0, source.getCalls(), "非登录态账号不得调用第四数据源")
			require.Equal(t, 0, source.getResetCalls(), "非登录态账号不得读重置卡")
			require.Nil(t, snapshot.ModelCredits)
			require.Nil(t, snapshot.ResetCards)
			require.False(t, snapshot.NeedsRelogin)
		})
	}
}

// --- 失败路径（Fetch 永不返回 error） ---

func TestQuotaFetcher_AccountMissingYieldsLinkedAccountSnapshot(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.err = errors.New("not found")

	snapshot := fetcher.Fetch(context.Background(), 404)

	require.False(t, snapshot.Success)
	require.Equal(t, "linked account not found", snapshot.Error)
	require.Equal(t, 0, usage.getCalls()) // 未走到数据源
}

func TestQuotaFetcher_UsageAuthErrorMarksCredentialInvalid(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[3] = &Account{ID: 3, Platform: domain.PlatformOpenAI}
	usage.err = errors.New("API returned 401: unauthorized")

	snapshot := fetcher.Fetch(context.Background(), 3)

	require.False(t, snapshot.Success)
	require.True(t, snapshot.CredentialInvalid)
	require.Contains(t, snapshot.Error, "401")
}

// 值通道失败：antigravity/grok 等平台 err==nil 但错误降级在 UsageInfo 字段里，
// 必须识别为失败快照，否则会被误判为 operational。
func TestQuotaFetcher_UsageValueChannelFailureYieldsFailureSnapshot(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)

	// 凭据失效（401 语义）→ failed。
	accounts.accounts[3] = &Account{ID: 3, Platform: domain.PlatformAnthropic}
	usage.usage = &UsageInfo{Error: "usage API error: HTTP 401", ErrorCode: errorCodeUnauthenticated, NeedsReauth: true}
	snapshot := fetcher.Fetch(context.Background(), 3)
	require.False(t, snapshot.Success)
	require.True(t, snapshot.CredentialInvalid)
	require.Contains(t, snapshot.Error, "401")
	require.Equal(t, MonitorStatusFailed, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)

	// 限流等非凭据失败 → error（而非 operational）。
	accounts.accounts[13] = &Account{ID: 13, Platform: domain.PlatformAnthropic}
	usage.usage = &UsageInfo{Error: "usage API error: HTTP 429", ErrorCode: errorCodeRateLimited}
	snapshot = fetcher.Fetch(context.Background(), 13)
	require.False(t, snapshot.Success)
	require.False(t, snapshot.CredentialInvalid)
	require.Contains(t, snapshot.Error, "429")
	require.Equal(t, MonitorStatusError, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)

	// grok 已知未知态（尚未观测到计费/限流头）不算失败。
	accounts.accounts[14] = &Account{ID: 14, Platform: domain.PlatformGrok}
	usage.usage = &UsageInfo{ErrorCode: "quota_unknown", Error: "Grok quota is unknown until billing is probed"}
	snapshot = fetcher.Fetch(context.Background(), 14)
	require.True(t, snapshot.Success)
	require.Empty(t, snapshot.Error)
	require.Empty(t, snapshot.Tiers)
	require.Equal(t, MonitorStatusOperational, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)
}

func TestUsageFailureInfo_ClassificationMatrix(t *testing.T) {
	cases := []struct {
		name              string
		usage             *UsageInfo
		failed            bool
		credentialInvalid bool
		msg               string
	}{
		{name: "nil usage", usage: nil},
		{name: "healthy empty", usage: &UsageInfo{}},
		{name: "error text only", usage: &UsageInfo{Error: "boom"}, failed: true, msg: "boom"},
		{name: "needs reauth", usage: &UsageInfo{NeedsReauth: true}, failed: true, credentialInvalid: true, msg: "usage fetch failed"},
		{name: "banned", usage: &UsageInfo{IsBanned: true}, failed: true, credentialInvalid: true, msg: "usage fetch failed"},
		{name: "forbidden with reason", usage: &UsageInfo{IsForbidden: true, ForbiddenReason: "usage limited"}, failed: true, credentialInvalid: true, msg: "usage limited"},
		{name: "error code unauthenticated", usage: &UsageInfo{ErrorCode: errorCodeUnauthenticated}, failed: true, credentialInvalid: true, msg: errorCodeUnauthenticated},
		{name: "error code forbidden", usage: &UsageInfo{ErrorCode: errorCodeForbidden}, failed: true, credentialInvalid: true, msg: errorCodeForbidden},
		{name: "error code rate limited", usage: &UsageInfo{ErrorCode: errorCodeRateLimited}, failed: true, msg: errorCodeRateLimited},
		{name: "error code network error", usage: &UsageInfo{ErrorCode: errorCodeNetworkError}, failed: true, msg: errorCodeNetworkError},
		{name: "grok quota unknown exempted", usage: &UsageInfo{ErrorCode: "quota_unknown", Error: "Grok quota is unknown until billing is probed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failed, credentialInvalid, msg := usageFailureInfo(tc.usage)
			require.Equal(t, tc.failed, failed)
			require.Equal(t, tc.credentialInvalid, credentialInvalid)
			if tc.msg != "" {
				require.Equal(t, tc.msg, msg)
			}
		})
	}
}

// 凭据失效只认 401/403（与 fetchCNBalance 口径一致）：CN quota 服务的
// CredentialValid 仅成功路径置 true，500/429/智谱业务错误须推导为 error 而非 failed。
func TestQuotaFetcher_CNQuotaCredentialInvalidByStatusCode(t *testing.T) {
	cases := []struct {
		name           string
		accountID      int64
		statusCode     int
		credentialBad  bool
		expectedStatus string
	}{
		{name: "401 unauthorized", accountID: 5, statusCode: 401, credentialBad: true, expectedStatus: MonitorStatusFailed},
		{name: "403 forbidden", accountID: 15, statusCode: 403, credentialBad: true, expectedStatus: MonitorStatusFailed},
		{name: "500 server error", accountID: 16, statusCode: 500, expectedStatus: MonitorStatusError},
		{name: "429 rate limited", accountID: 17, statusCode: 429, expectedStatus: MonitorStatusError},
		// 智谱 2xx 但业务级失败：StatusCode=200，非凭据问题。
		{name: "200 business error", accountID: 18, statusCode: 200, expectedStatus: MonitorStatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher, _, cnQuota, _, accounts := newQuotaFetcherTestSetup(t)
			accounts.accounts[tc.accountID] = &Account{
				ID:          tc.accountID,
				Platform:    domain.PlatformZhipu,
				Credentials: map[string]any{"account_mode": AccountModeCoding},
			}
			cnQuota.result = &CNProviderQuotaProbeResult{
				Success:    false,
				StatusCode: tc.statusCode,
				Error:      "api key expired",
			}

			snapshot := fetcher.Fetch(context.Background(), tc.accountID)

			require.False(t, snapshot.Success)
			require.Equal(t, tc.credentialBad, snapshot.CredentialInvalid)
			require.Equal(t, tc.expectedStatus, deriveQuotaCheckResult(snapshot, "quota", time.Now()).Status)
		})
	}
}

func TestQuotaFetcher_CNBalanceHTTP403MarksCredentialInvalid(t *testing.T) {
	fetcher, _, _, cnBalance, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[6] = &Account{ID: 6, Platform: domain.PlatformKimi}
	cnBalance.result = &CNProviderBalanceResult{Success: false, StatusCode: 403, Error: "forbidden"}

	snapshot := fetcher.Fetch(context.Background(), 6)

	require.False(t, snapshot.Success)
	require.True(t, snapshot.CredentialInvalid)
}

// 余额告警口径与账号停调（CNProviderBalanceCheckService.checkOne）一致：
// 上游标记不可用或全部币种低于阈值 → BalanceLow → degraded；任一币种达标即健康。
func TestQuotaFetcher_CNBalanceLowMarksDegraded(t *testing.T) {
	cases := []struct {
		name        string
		accountID   int64
		result      *CNProviderBalanceResult
		balanceLow  bool
		wantStatus  string
		wantMessage string
	}{
		{
			// 审查例：余额 5/阈值 10 的账号调度器已停调，监控不能仍绿灯。
			name:       "balance below threshold",
			accountID:  21,
			result:     &CNProviderBalanceResult{Success: true, Available: true, Balance: 5, Currency: "CNY"},
			balanceLow: true,
			wantStatus: MonitorStatusDegraded, wantMessage: "balance low: 5 CNY",
		},
		{
			name:       "upstream marked unavailable",
			accountID:  22,
			result:     &CNProviderBalanceResult{Success: true, Available: false, Balance: 20, Currency: "CNY"},
			balanceLow: true,
			wantStatus: MonitorStatusDegraded, wantMessage: "balance low: 20 CNY",
		},
		{
			// deepseek 双币种：任一币种（USD 20）达标即健康。
			name:      "any currency above threshold is healthy",
			accountID: 23,
			result: &CNProviderBalanceResult{
				Success: true, Available: true, Balance: 5, Currency: "CNY",
				Balances: []CNProviderBalanceEntry{{Currency: "CNY", Balance: 5}, {Currency: "USD", Balance: 20}},
			},
			wantStatus: MonitorStatusOperational,
		},
		{
			name:       "single currency above threshold",
			accountID:  24,
			result:     &CNProviderBalanceResult{Success: true, Available: true, Balance: 20, Currency: "CNY"},
			wantStatus: MonitorStatusOperational,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher, _, _, cnBalance, accounts := newQuotaFetcherTestSetup(t)
			fetcher.balanceThreshold = 10
			accounts.accounts[tc.accountID] = &Account{
				ID:          tc.accountID,
				Platform:    domain.PlatformKimi,
				Credentials: map[string]any{"account_mode": AccountModePayG},
			}
			cnBalance.result = tc.result

			snapshot := fetcher.Fetch(context.Background(), tc.accountID)

			require.True(t, snapshot.Success)
			require.Equal(t, tc.balanceLow, snapshot.BalanceLow)
			res := deriveQuotaCheckResult(snapshot, "quota", time.Now())
			require.Equal(t, tc.wantStatus, res.Status)
			if tc.wantMessage != "" {
				require.Contains(t, res.Message, tc.wantMessage)
			} else {
				require.Empty(t, res.Message)
			}
		})
	}
}

func TestNewChannelMonitorQuotaFetcher_ThresholdFromConfig(t *testing.T) {
	require.InDelta(t, 0.5, NewChannelMonitorQuotaFetcher(nil, nil, nil, nil, nil, nil).balanceThreshold, 0.0001)

	cfg10 := &config.Config{Gateway: config.GatewayConfig{CNProviders: config.GatewayCNProvidersConfig{BalanceThreshold: 10}}}
	require.InDelta(t, 10, NewChannelMonitorQuotaFetcher(nil, nil, nil, nil, cfg10, nil).balanceThreshold, 0.0001)

	// 非正值（含显式 0）回退默认，避免 0 阈值下「余额=0 也不告警」。
	cfg0 := &config.Config{Gateway: config.GatewayConfig{CNProviders: config.GatewayCNProvidersConfig{BalanceThreshold: 0}}}
	require.InDelta(t, 0.5, NewChannelMonitorQuotaFetcher(nil, nil, nil, nil, cfg0, nil).balanceThreshold, 0.0001)
}

func TestQuotaFetcher_NilDependenciesProduceErrorSnapshots(t *testing.T) {
	// fetcher 本体为 nil：直接降级为错误快照，不 panic。
	var nilFetcher *ChannelMonitorQuotaFetcher
	snapshot := nilFetcher.Fetch(context.Background(), 1)
	require.False(t, snapshot.Success)
	require.Equal(t, "quota fetcher is not configured", snapshot.Error)

	// 数据源缺失：账号能加载，但对应服务未注入。
	fetcher, _, _, _, accounts := newQuotaFetcherTestSetup(t)
	fetcher.usage = nil
	accounts.accounts[2] = &Account{ID: 2, Platform: domain.PlatformOpenAI}
	snapshot = fetcher.Fetch(context.Background(), 2)
	require.False(t, snapshot.Success)
	require.Contains(t, snapshot.Error, "not configured")
}

// --- TTL 缓存 ---

func TestQuotaFetcher_CachesSuccessSnapshotPerAccount(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[8] = &Account{ID: 8, Platform: domain.PlatformOpenAI}
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 10}}

	for i := 0; i < 3; i++ {
		snapshot := fetcher.Fetch(context.Background(), 8)
		require.True(t, snapshot.Success)
	}
	require.Equal(t, 1, usage.getCalls(), "success snapshots should be served from cache")

	// 缓存过期后重新拉取。
	fetcher.mu.Lock()
	entry := fetcher.cache[8]
	entry.expiry = time.Now().Add(-time.Second)
	fetcher.cache[8] = entry
	fetcher.mu.Unlock()

	_ = fetcher.Fetch(context.Background(), 8)
	require.Equal(t, 2, usage.getCalls())
}

func TestQuotaFetcher_CachesFailureSnapshotWithShortTTL(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[4] = &Account{ID: 4, Platform: domain.PlatformOpenAI}
	usage.err = errors.New("boom")

	for i := 0; i < 2; i++ {
		snapshot := fetcher.Fetch(context.Background(), 4)
		require.False(t, snapshot.Success)
	}
	require.Equal(t, 1, usage.getCalls(), "failure snapshots should be served from the short negative cache")

	// 失败快照的 TTL 是负缓存时长（而非成功 TTL）。
	fetcher.mu.Lock()
	entry := fetcher.cache[4]
	require.WithinDuration(t, entry.snapshot.FetchedAt.Add(monitorQuotaErrorCacheTTL), entry.expiry, time.Second)
	entry.expiry = time.Now().Add(-time.Second)
	fetcher.cache[4] = entry
	fetcher.mu.Unlock()

	_ = fetcher.Fetch(context.Background(), 4)
	require.Equal(t, 2, usage.getCalls(), "expired negative cache should refetch")
}

func TestQuotaFetcher_ConcurrentFetchesShareSingleFlight(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[12] = &Account{ID: 12, Platform: domain.PlatformOpenAI}
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 10}}
	usage.block = make(chan struct{})

	var wg sync.WaitGroup
	snapshots := make([]*domain.MonitorQuotaSnapshot, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			snapshots[idx] = fetcher.Fetch(context.Background(), 12)
		}(i)
	}

	// 上游被 block 卡住时，5 个并发 Fetch 应只产生 1 次真实查询。
	require.Eventually(t, func() bool { return usage.getCalls() == 1 },
		5*time.Second, 10*time.Millisecond)
	close(usage.block)
	wg.Wait()

	for _, snapshot := range snapshots {
		require.NotNil(t, snapshot)
		require.True(t, snapshot.Success)
	}
	require.Equal(t, 1, usage.getCalls())

	// 成功快照已缓存：再取一次仍不打上游。
	_ = fetcher.Fetch(context.Background(), 12)
	require.Equal(t, 1, usage.getCalls())
}

// 上面那条并发用例只能以极低概率撞上真正的缺陷窗口（实测约 300 次一次），
// 所以这里直接钉住窗口本身。
//
// 窗口在 Fetch 顶部的 cachedSnapshot 与 flight.DoChan 之间：一个 goroutine 判定
// 缓存未命中之后、真正入队之前，另一个 goroutine 的 flight 可能已经跑完、写好缓存
// 并被摘掉 key，于是前者不会并入那次飞行，而是另起一次新的，对同一账号重复打上游。
// fetchShared 就是 flight 的执行体，直接调用它等价于「已经越过顶层缓存判定、拿到了
// 属于自己的那次飞行」这个状态。
func TestQuotaFetcher_SharedFetchCacheRecheckAvoidsDuplicateUpstream(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[13] = &Account{ID: 13, Platform: domain.PlatformOpenAI}
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 10}}

	first := fetcher.Fetch(context.Background(), 13)
	require.True(t, first.Success)
	require.Equal(t, 1, usage.getCalls())

	shared := fetcher.fetchShared(13)
	require.Equal(t, 1, usage.getCalls(), "重查缓存后不得再打一次上游")
	require.Same(t, first, shared, "应原样返回已缓存的快照")
}

// 重查不能变成无条件短路：缓存过期后同一个执行体必须照常回源，
// 否则快照会永远停在第一次的值上。
func TestQuotaFetcher_SharedFetchStillRefetchesAfterCacheExpiry(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[14] = &Account{ID: 14, Platform: domain.PlatformOpenAI}
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 10}}

	require.True(t, fetcher.Fetch(context.Background(), 14).Success)
	require.Equal(t, 1, usage.getCalls())

	fetcher.mu.Lock()
	entry := fetcher.cache[14]
	entry.expiry = time.Now().Add(-time.Second)
	fetcher.cache[14] = entry
	fetcher.mu.Unlock()

	require.True(t, fetcher.fetchShared(14).Success)
	require.Equal(t, 2, usage.getCalls(), "缓存过期后必须回源")
}

// --- UsageInfo → tiers 归一 ---

func TestUsageQuotaTiers_MapsAllWindowKinds(t *testing.T) {
	limit := int64(1000)
	remaining := int64(400)
	resetUnix := int64(1777283883)
	usage := &UsageInfo{
		FiveHour:          &UsageProgress{Utilization: 50},
		SevenDay:          &UsageProgress{Utilization: 60},
		SevenDaySonnet:    &UsageProgress{Utilization: 70},
		SevenDayFable:     &UsageProgress{Utilization: 80},
		ThirtyDay:         &UsageProgress{Utilization: 20},
		GeminiSharedDaily: &UsageProgress{Utilization: 11},
		GeminiProDaily:    &UsageProgress{Utilization: 22},
		GeminiFlashDaily:  &UsageProgress{Utilization: 33},
		GrokRequestQuota:  &xai.QuotaWindow{Limit: &limit, Remaining: &remaining, ResetUnix: &resetUnix},
		GrokTokenQuota:    &xai.QuotaWindow{Limit: &limit, Remaining: &remaining, ResetAt: "2026-08-19T00:00:00Z"},
		AntigravityQuota: map[string]*AntigravityModelQuota{
			"gemini-3-pro":   {Utilization: 45},
			"gemini-3-flash": {Utilization: 55},
		},
	}

	tiers := usageQuotaTiers(usage)

	// 5h/7d/7d-sonnet/7d-fable/30d + gemini×3 + grok×2 + antigravity×2
	require.Len(t, tiers, 12)

	byKey := make(map[string]domain.MonitorQuotaTier, len(tiers))
	for _, tier := range tiers {
		key := tier.Window
		if tier.Label != "" {
			key = tier.Window + "/" + tier.Label
		}
		byKey[key] = tier
	}

	require.Contains(t, byKey, "5h")
	require.Contains(t, byKey, "7d")
	require.Contains(t, byKey, "7d-sonnet")
	require.Contains(t, byKey, "7d-fable")
	require.Contains(t, byKey, "30d")
	require.Contains(t, byKey, "daily/shared")
	require.Contains(t, byKey, "daily/pro")
	require.Contains(t, byKey, "daily/flash")
	require.Contains(t, byKey, "daily/requests")
	require.Contains(t, byKey, "daily/tokens")
	require.Contains(t, byKey, "total/gemini-3-pro")
	require.Contains(t, byKey, "total/gemini-3-flash")

	// grok requests 窗口：used = limit - remaining，百分比 60%。
	requests := byKey["daily/requests"]
	require.Equal(t, float64(600), requests.Used)
	require.Equal(t, float64(1000), requests.Limit)
	require.InDelta(t, 60.0, requests.UsedPercent, 0.001)
	require.NotEmpty(t, requests.ResetAt, "ResetUnix should fall back to RFC3339")

	tokens := byKey["daily/tokens"]
	require.Equal(t, "2026-08-19T00:00:00Z", tokens.ResetAt)
}

func TestUsageQuotaTiers_NilAndEmptyInputs(t *testing.T) {
	require.Nil(t, usageQuotaTiers(nil))
	require.Nil(t, usageQuotaTiers(&UsageInfo{}))

	// Grok 窗口 limit<=0 时跳过，避免除零。
	var zero int64
	tiers := usageQuotaTiers(&UsageInfo{
		GrokRequestQuota: &xai.QuotaWindow{Limit: &zero, Remaining: &zero},
	})
	require.Nil(t, tiers)
}

// --- 状态推导 ---

func TestDeriveQuotaCheckResult_StatusMatrix(t *testing.T) {
	now := time.Now()

	healthy := &domain.MonitorQuotaSnapshot{Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 40}}}
	res := deriveQuotaCheckResult(healthy, "quota", now)
	require.Equal(t, MonitorStatusOperational, res.Status)
	require.Equal(t, "quota", res.Model)
	require.Empty(t, res.Message)

	highUsage := &domain.MonitorQuotaSnapshot{Success: true, Tiers: []domain.MonitorQuotaTier{
		{Window: "5h", UsedPercent: 30},
		{Window: "daily", Label: "pro", UsedPercent: 95},
	}}
	res = deriveQuotaCheckResult(highUsage, "quota", now)
	require.Equal(t, MonitorStatusDegraded, res.Status)
	require.Contains(t, res.Message, "pro/daily")
	require.Contains(t, res.Message, "95.0%")

	balance := -0.5
	lowBalance := &domain.MonitorQuotaSnapshot{Success: true, BalanceLow: true, Balance: &balance, Currency: "CNY"}
	res = deriveQuotaCheckResult(lowBalance, "quota", now)
	require.Equal(t, MonitorStatusDegraded, res.Status)
	require.Contains(t, res.Message, "balance low")

	invalid := &domain.MonitorQuotaSnapshot{Success: false, CredentialInvalid: true, Error: "401 unauthorized"}
	res = deriveQuotaCheckResult(invalid, "quota", now)
	require.Equal(t, MonitorStatusFailed, res.Status)

	unlinked := &domain.MonitorQuotaSnapshot{Success: false, Error: "linked account not found"}
	res = deriveQuotaCheckResult(unlinked, "quota", now)
	require.Equal(t, MonitorStatusDegraded, res.Status)

	other := &domain.MonitorQuotaSnapshot{Success: false, Error: "connection refused"}
	res = deriveQuotaCheckResult(other, "quota", now)
	require.Equal(t, MonitorStatusError, res.Status)
	require.Equal(t, "connection refused", res.Message)

	res = deriveQuotaCheckResult(nil, "quota", now)
	require.Equal(t, MonitorStatusError, res.Status)
}

// 回归（票 11 验收）：M4 新字段一期不参与状态判定——有积分明细/重置卡/重登标记
// 也不能把健康账号从 operational 拉下来；只在已经 degraded 时附加 needs_relogin 文案。
func TestDeriveQuotaCheckResult_IgnoresZhipuLoginFields(t *testing.T) {
	now := time.Now()
	credits := []domain.MonitorQuotaModelCredit{{Model: "glm-5.3", Date: "2026-10-06", Credits: 3.5}}
	cards := []domain.MonitorResetCard{{Type: "five_hour", ExpireAt: "2026-10-06T20:00:00+08:00"}}

	cases := []struct {
		name            string
		snapshot        *domain.MonitorQuotaSnapshot
		wantStatus      string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "healthy with credits and relogin stays operational",
			snapshot: &domain.MonitorQuotaSnapshot{
				Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 20}},
				ModelCredits: credits, ResetCards: cards, NeedsRelogin: true,
			},
			wantStatus: MonitorStatusOperational,
		},
		{
			name: "degraded hint appends relogin text",
			snapshot: &domain.MonitorQuotaSnapshot{
				Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}},
				ModelCredits: credits, NeedsRelogin: true,
			},
			wantStatus:   MonitorStatusDegraded,
			wantContains: []string{"quota high: 5h at 95.0%", "relogin"},
		},
		{
			name: "degraded hint unchanged without relogin",
			snapshot: &domain.MonitorQuotaSnapshot{
				Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}},
				ModelCredits: credits, ResetCards: cards,
			},
			wantStatus:      MonitorStatusDegraded,
			wantContains:    []string{"quota high: 5h at 95.0%"},
			wantNotContains: []string{"relogin"},
		},
		{
			name: "failed stays failed regardless of new fields",
			snapshot: &domain.MonitorQuotaSnapshot{
				Success: false, CredentialInvalid: true, Error: "401 unauthorized",
				ModelCredits: credits, ResetCards: cards, NeedsRelogin: true,
			},
			wantStatus:   MonitorStatusFailed,
			wantContains: []string{"401 unauthorized"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := deriveQuotaCheckResult(tc.snapshot, "quota", now)
			require.Equal(t, tc.wantStatus, res.Status)
			for _, want := range tc.wantContains {
				require.Contains(t, res.Message, want)
			}
			for _, notWant := range tc.wantNotContains {
				require.NotContains(t, res.Message, notWant)
			}
			if len(tc.wantContains) == 0 {
				require.Empty(t, res.Message)
			}
		})
	}
}

// 集成点（票 11 测试要求）：登录托管账号 type=apikey，validateCodingPlanAccount
// 只看 platform+mode，天然通过，无需为登录态新增分支。
func TestValidateCodingPlanAccount_ZhipuLoginManagedAccountPasses(t *testing.T) {
	managed := zhipuLoginManagedQuotaAccount(41, map[string]any{ZhipuNeedsReloginExtraKey: true})
	require.NoError(t, validateCodingPlanAccount(managed))

	// 既有负向路径不变。
	require.Error(t, validateCodingPlanAccount(&Account{
		ID: 42, Platform: domain.PlatformAnthropic, Credentials: map[string]any{"account_mode": AccountModeCoding},
	}))
	require.Error(t, validateCodingPlanAccount(&Account{
		ID: 43, Platform: domain.PlatformZhipu, Credentials: map[string]any{"account_mode": AccountModePayG},
	}))
	require.Error(t, validateCodingPlanAccount(nil))
}
