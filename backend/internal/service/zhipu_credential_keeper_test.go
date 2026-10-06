//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// keeperFakeRepo 只实现 keeper 允许调用的仓储方法；其余 AccountRepository
// 方法由嵌入的 nil 接口提供——一旦 keeper 越界调用（如 SetError）立即 panic，
// 这是比计数更强的负向断言。显式命名的停调方法另加计数器，便于断言恒为 0。
type keeperFakeRepo struct {
	AccountRepository

	accounts  []Account
	listErr   error
	extraErr  error
	listFlows []string

	mu          sync.Mutex
	extraWrites []keeperExtraWrite

	setErrorCalls  int
	setTempCalls   int
	clearTempCalls int
	schedCalls     int
	clearErrCalls  int
}

type keeperExtraWrite struct {
	id      int64
	updates map[string]any
}

func (r *keeperFakeRepo) ListAccountsByCredentialFlow(_ context.Context, flow string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listFlows = append(r.listFlows, flow)
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.accounts, nil
}

func (r *keeperFakeRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := make(map[string]any, len(updates))
	for k, v := range updates {
		clone[k] = v
	}
	r.extraWrites = append(r.extraWrites, keeperExtraWrite{id: id, updates: clone})
	return r.extraErr
}

func (r *keeperFakeRepo) SetError(context.Context, int64, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setErrorCalls++
	return nil
}

func (r *keeperFakeRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setTempCalls++
	return nil
}

func (r *keeperFakeRepo) ClearTempUnschedulable(context.Context, int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearTempCalls++
	return nil
}

func (r *keeperFakeRepo) SetSchedulable(context.Context, int64, bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedCalls++
	return nil
}

func (r *keeperFakeRepo) ClearError(context.Context, int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearErrCalls++
	return nil
}

func (r *keeperFakeRepo) writes() []keeperExtraWrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]keeperExtraWrite(nil), r.extraWrites...)
}

func (r *keeperFakeRepo) schedulingMutationCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setErrorCalls + r.setTempCalls + r.clearTempCalls + r.schedCalls + r.clearErrCalls
}

var _ AccountRepository = (*keeperFakeRepo)(nil)

// keeperFakeProbe 是票 08 探针接缝的测试桩。
type keeperFakeProbe struct {
	mu         sync.Mutex
	calls      int
	accountIDs []int64
	starts     []time.Time
	ends       []time.Time
	credits    []domain.MonitorQuotaModelCredit
	err        error
	onCall     chan struct{}
}

func (p *keeperFakeProbe) FetchUsageDetailForAccount(_ context.Context, account *Account, start, end time.Time) ([]domain.MonitorQuotaModelCredit, error) {
	p.mu.Lock()
	p.calls++
	p.accountIDs = append(p.accountIDs, account.ID)
	p.starts = append(p.starts, start)
	p.ends = append(p.ends, end)
	p.mu.Unlock()
	if p.onCall != nil {
		select {
		case p.onCall <- struct{}{}:
		default:
		}
	}
	return p.credits, p.err
}

func (p *keeperFakeProbe) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *keeperFakeProbe) windows() ([]time.Time, []time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.starts...), append([]time.Time(nil), p.ends...)
}

type keeperNotifyCall struct {
	accountID int64
	detail    string
}

type keeperFakeNotifier struct {
	mu    sync.Mutex
	calls []keeperNotifyCall
}

func (n *keeperFakeNotifier) NotifyZhipuCredentialInvalid(_ context.Context, account *Account, detail string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, keeperNotifyCall{accountID: account.ID, detail: detail})
}

func (n *keeperFakeNotifier) recorded() []keeperNotifyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]keeperNotifyCall(nil), n.calls...)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func keeperManagedAccount(id int64, name string, extra map[string]any) *Account {
	return &Account{
		ID:       id,
		Name:     name,
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			zhipuCredentialAuthFlow:    ZhipuLoginAuthFlow,
			zhipuCredentialAccessToken: "at-token",
		},
		Extra: extra,
	}
}

func keeperTestConfig(minutes int) *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.Zhipu.CredentialCheckIntervalMinutes = minutes
	return cfg
}

// keeperTestKeeper 组装冻结时钟、可注入随机源、可注入停更阈值的 keeper。
func keeperTestKeeper(
	repo *keeperFakeRepo,
	probe *keeperFakeProbe,
	notifier *keeperFakeNotifier,
	cfg *config.Config,
	now time.Time,
) *ZhipuCredentialKeeper {
	k := NewZhipuCredentialKeeper(repo, probe, notifier, cfg)
	k.now = func() time.Time { return now }
	k.usageStaleAfter = 6 * time.Hour
	return k
}

// requireNoSchedulingMutation 是票 09 的核心负向断言：任何分支都不得改动调度状态。
func requireNoSchedulingMutation(t *testing.T, repo *keeperFakeRepo) {
	t.Helper()
	require.Equal(t, 0, repo.schedulingMutationCalls(),
		"keeper 不得调用 SetError/ClearError/SetTempUnschedulable/ClearTempUnschedulable/SetSchedulable")
	for _, w := range repo.writes() {
		for key := range w.updates {
			require.Equal(t, ZhipuNeedsReloginExtraKey, key,
				"keeper 只允许写 needs_relogin 这一个 extra 键")
		}
	}
}

// ---------------------------------------------------------------------------
// CheckAccount 表驱动
// ---------------------------------------------------------------------------

func TestZhipuCredentialKeeperCheckAccountProbeOutcomes(t *testing.T) {
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	unauthorized401 := fmt.Errorf("%w: HTTP 401", ErrZhipuCreditUsageUnauthorized)
	unauthorized403 := fmt.Errorf("%w: HTTP 403", ErrZhipuCreditUsageUnauthorized)
	networkErr := fmt.Errorf("%w: dial tcp 1.2.3.4:443: i/o timeout", ErrZhipuCreditUsageUpstream)

	staleSnapshot := map[string]any{
		cnExtraKey(PlatformZhipu, cnExtraSuffixUsageUpdated): now.Add(-24 * time.Hour).Format(time.RFC3339),
	}
	freshSnapshot := map[string]any{
		cnExtraKey(PlatformZhipu, cnExtraSuffixUsageUpdated): now.Add(-5 * time.Minute).Format(time.RFC3339),
	}

	marker := func(v any) map[string]any {
		return map[string]any{ZhipuNeedsReloginExtraKey: v}
	}
	merged := func(base map[string]any, extra map[string]any) map[string]any {
		out := make(map[string]any, len(base)+len(extra))
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	tests := []struct {
		name     string
		account  *Account
		probeErr error
		// errorMessage 是数据面已经持久化的账号错误文本（accounts.error_message），
		// 兜底分支的第二个信号。
		errorMessage string
		wantProbe    int
		wantWrites   []keeperExtraWrite
		wantNotify   int
		wantDetail   string
		wantNotifyID int64
	}{
		{
			name:       "探针 401 → 写标记并通知",
			account:    keeperManagedAccount(42, "zhipu-1", nil),
			probeErr:   unauthorized401,
			wantProbe:  1,
			wantWrites: []keeperExtraWrite{{id: 42, updates: marker(true)}},
			wantNotify: 1, wantDetail: "401", wantNotifyID: 42,
		},
		{
			name:       "探针 403 → 写标记并通知",
			account:    keeperManagedAccount(42, "zhipu-1", nil),
			probeErr:   unauthorized403,
			wantProbe:  1,
			wantWrites: []keeperExtraWrite{{id: 42, updates: marker(true)}},
			wantNotify: 1, wantDetail: "403", wantNotifyID: 42,
		},
		{
			name:       "探针 2xx 且已有标记 → 清除标记",
			account:    keeperManagedAccount(42, "zhipu-1", merged(marker(true), map[string]any{"keep": "yes"})),
			probeErr:   nil,
			wantProbe:  1,
			wantWrites: []keeperExtraWrite{{id: 42, updates: marker(false)}},
			wantNotify: 0,
		},
		{
			name:       "探针 2xx 且无标记 → 不写库",
			account:    keeperManagedAccount(42, "zhipu-1", nil),
			probeErr:   nil,
			wantProbe:  1,
			wantWrites: nil,
			wantNotify: 0,
		},
		{
			name:       "探针网络错误 → 既不清标记也不写库",
			account:    keeperManagedAccount(42, "zhipu-1", merged(marker(true), freshSnapshot)),
			probeErr:   networkErr,
			wantProbe:  1,
			wantWrites: nil,
			wantNotify: 0,
		},
		{
			name: "探针未接线 → 不动标记",
			account: keeperManagedAccount(42, "zhipu-1",
				merged(marker(true), freshSnapshot)),
			probeErr:   ErrZhipuCreditUsageNotConfigured,
			wantProbe:  1,
			wantWrites: nil,
			wantNotify: 0,
		},
		{
			name:       "非托管账号被跳过",
			account:    &Account{ID: 7, Name: "anthropic-1", Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{zhipuCredentialAuthFlow: ZhipuLoginAuthFlow, zhipuCredentialAccessToken: "at-token"}},
			probeErr:   unauthorized401,
			wantProbe:  0,
			wantWrites: nil,
			wantNotify: 0,
		},
		{
			name: "缺 access_token 的托管账号被跳过",
			account: &Account{
				ID: 8, Name: "zhipu-no-token", Platform: PlatformZhipu, Type: AccountTypeAPIKey,
				Credentials: map[string]any{zhipuCredentialAuthFlow: ZhipuLoginAuthFlow},
			},
			probeErr:   unauthorized401,
			wantProbe:  0,
			wantWrites: nil,
			wantNotify: 0,
		},
		{
			name:       "重复 401 已标记 → 不重复写库不重复通知",
			account:    keeperManagedAccount(42, "zhipu-1", marker(true)),
			probeErr:   unauthorized401,
			wantProbe:  1,
			wantWrites: nil,
			wantNotify: 0,
		},
		{
			name:         "兜底：快照长期停更且错误含 401 → 写标记并通知",
			account:      keeperManagedAccount(42, "zhipu-1", staleSnapshot),
			probeErr:     networkErr,
			errorMessage: "Authentication failed (401): invalid or expired credentials",
			wantProbe:    1,
			wantWrites:   []keeperExtraWrite{{id: 42, updates: marker(true)}},
			wantNotify:   1, wantDetail: "401", wantNotifyID: 42,
		},
		{
			name:         "兜底不触发：快照新鲜",
			account:      keeperManagedAccount(42, "zhipu-1", freshSnapshot),
			probeErr:     networkErr,
			errorMessage: "Authentication failed (401): invalid or expired credentials",
			wantProbe:    1,
			wantWrites:   nil,
			wantNotify:   0,
		},
		{
			name:         "兜底不触发：错误不含 401",
			account:      keeperManagedAccount(42, "zhipu-1", staleSnapshot),
			probeErr:     networkErr,
			errorMessage: "Payment required (402): insufficient balance or billing issue",
			wantProbe:    1,
			wantWrites:   nil,
			wantNotify:   0,
		},
		{
			name:         "兜底不触发：无快照",
			account:      keeperManagedAccount(42, "zhipu-1", nil),
			probeErr:     networkErr,
			errorMessage: "Authentication failed (401): invalid or expired credentials",
			wantProbe:    1,
			wantWrites:   nil,
			wantNotify:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.account != nil && tc.errorMessage != "" {
				tc.account.ErrorMessage = tc.errorMessage
			}

			repo := &keeperFakeRepo{}
			probe := &keeperFakeProbe{err: tc.probeErr}
			notifier := &keeperFakeNotifier{}
			k := keeperTestKeeper(repo, probe, notifier, keeperTestConfig(60), now)

			k.CheckAccount(context.Background(), tc.account)

			require.Equal(t, tc.wantProbe, probe.callCount(), "探针调用次数")
			require.Equal(t, tc.wantWrites, repo.writes(), "extra 写入")
			require.Len(t, notifier.recorded(), tc.wantNotify, "管理员通知次数")
			if tc.wantNotify > 0 {
				got := notifier.recorded()[0]
				require.Equal(t, tc.wantNotifyID, got.accountID)
				require.Contains(t, got.detail, tc.wantDetail)
			}
			if tc.wantProbe > 0 {
				starts, ends := probe.windows()
				require.Equal(t, now, starts[0], "探针窗口起点取注入时钟")
				require.Equal(t, now, ends[0], "探针窗口终点取注入时钟")
			}
			requireNoSchedulingMutation(t, repo)
		})
	}
}

func TestZhipuCredentialKeeperUnauthorizedFallbackDoesNotMatchSubstring(t *testing.T) {
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	repo := &keeperFakeRepo{}
	probe := &keeperFakeProbe{err: fmt.Errorf("%w: i/o timeout", ErrZhipuCreditUsageUpstream)}
	notifier := &keeperFakeNotifier{}
	k := keeperTestKeeper(repo, probe, notifier, keeperTestConfig(60), now)

	account := keeperManagedAccount(42, "zhipu-1", map[string]any{
		cnExtraKey(PlatformZhipu, cnExtraSuffixUsageUpdated): now.Add(-48 * time.Hour).Format(time.RFC3339),
	})
	// "4021 ms" / "1401" 不是 401 状态码，不得据此误标。
	account.ErrorMessage = "upstream latency 4021 ms, request id 1401"

	k.CheckAccount(context.Background(), account)

	require.Empty(t, repo.writes(), "非 401 数字不得触发兜底标记")
	require.Empty(t, notifier.recorded())
	requireNoSchedulingMutation(t, repo)
}

// ---------------------------------------------------------------------------
// 轮次与生命周期
// ---------------------------------------------------------------------------

func TestZhipuCredentialKeeperRunOnceCoversAllCredentialFlowAccounts(t *testing.T) {
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	repo := &keeperFakeRepo{accounts: []Account{
		*keeperManagedAccount(1, "zhipu-1", nil),
		*keeperManagedAccount(2, "zhipu-2", map[string]any{ZhipuNeedsReloginExtraKey: true}),
	}}
	// 探针统一 401：账号 1 首跨 → 写标记 + 通知一次；账号 2 已标记 → 首跨语义跳过
	// （不重复写库/通知）。这才支撑「恰好 1 条通知」的断言（健康探针不会产生任何通知）。
	probe := &keeperFakeProbe{err: ErrZhipuCreditUsageUnauthorized}
	notifier := &keeperFakeNotifier{}
	k := keeperTestKeeper(repo, probe, notifier, keeperTestConfig(60), now)

	k.runOnce(context.Background())

	require.Equal(t, []string{ZhipuLoginAuthFlow}, repo.listFlows, "只按登录托管凭据流筛选")
	require.Equal(t, 2, probe.callCount())
	require.Len(t, notifier.recorded(), 1)
	requireNoSchedulingMutation(t, repo)
}

func TestZhipuCredentialKeeperRunOnceListFailureDoesNotProbe(t *testing.T) {
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	repo := &keeperFakeRepo{listErr: errors.New("db down")}
	probe := &keeperFakeProbe{}
	notifier := &keeperFakeNotifier{}
	k := keeperTestKeeper(repo, probe, notifier, keeperTestConfig(60), now)

	k.runOnce(context.Background())

	require.Equal(t, 0, probe.callCount())
	require.Empty(t, notifier.recorded())
	requireNoSchedulingMutation(t, repo)
}

func TestZhipuCredentialKeeperStartStopsOnIntervalZero(t *testing.T) {
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	repo := &keeperFakeRepo{}
	probe := &keeperFakeProbe{}
	k := NewZhipuCredentialKeeper(repo, probe, &keeperFakeNotifier{}, keeperTestConfig(0))
	k.interval = 0
	k.now = func() time.Time { return now }

	k.Start()
	k.Stop() // 未启动时也必须是安全的

	require.Equal(t, 0, probe.callCount(), "interval<=0 时不启动探测")
}

func TestZhipuCredentialKeeperStartRunsRoundsUntilStop(t *testing.T) {
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	repo := &keeperFakeRepo{accounts: []Account{*keeperManagedAccount(1, "zhipu-1", nil)}}
	probe := &keeperFakeProbe{onCall: make(chan struct{}, 16)}
	k := keeperTestKeeper(repo, probe, &keeperFakeNotifier{}, keeperTestConfig(60), now)
	k.interval = 10 * time.Millisecond

	k.Start()
	select {
	case <-probe.onCall:
	case <-time.After(2 * time.Second):
		t.Fatal("keeper 未在预期时间内执行首轮探测")
	}
	k.Stop()

	after := probe.callCount()
	require.GreaterOrEqual(t, after, 1)
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, after, probe.callCount(), "Stop 返回后不得再探测")
	require.NotEmpty(t, repo.listFlows)
	require.Equal(t, ZhipuLoginAuthFlow, repo.listFlows[0])
}

// ---------------------------------------------------------------------------
// 间隔与抖动
// ---------------------------------------------------------------------------

func TestNewZhipuCredentialKeeperReadsConfiguredInterval(t *testing.T) {
	tests := []struct {
		name    string
		minutes int
		nilCfg  bool
		want    time.Duration
	}{
		{name: "配置值生效", minutes: 15, want: 15 * time.Minute},
		{name: "缺省回落 60 分钟", minutes: 0, want: 60 * time.Minute},
		{name: "配置缺失回落 60 分钟", nilCfg: true, want: 60 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *config.Config
			if !tc.nilCfg {
				cfg = keeperTestConfig(tc.minutes)
			}
			k := NewZhipuCredentialKeeper(&keeperFakeRepo{}, &keeperFakeProbe{}, &keeperFakeNotifier{}, cfg)
			require.Equal(t, tc.want, k.interval)
		})
	}
}

func TestZhipuCredentialKeeperNextIntervalJitterBounds(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		draw     func(n int64) int64
		want     time.Duration
	}{
		{
			name:     "60 分钟下界（抖动 = min(interval/5, 5min) = 5min）",
			interval: 60 * time.Minute,
			draw:     func(int64) int64 { return 0 },
			want:     55 * time.Minute,
		},
		{
			name:     "60 分钟上界",
			interval: 60 * time.Minute,
			draw:     func(n int64) int64 { return n - 1 },
			want:     65 * time.Minute,
		},
		{
			name:     "短间隔按 1/5 抖动",
			interval: 3 * time.Second,
			draw:     func(int64) int64 { return 0 },
			want:     2400 * time.Millisecond,
		},
		{
			name:     "短间隔上界",
			interval: 3 * time.Second,
			draw:     func(n int64) int64 { return n - 1 },
			want:     3600 * time.Millisecond,
		},
		{
			name:     "中位抽样不越界",
			interval: 60 * time.Minute,
			draw:     func(n int64) int64 { return n / 2 },
			want:     60 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := NewZhipuCredentialKeeper(&keeperFakeRepo{}, &keeperFakeProbe{}, &keeperFakeNotifier{}, keeperTestConfig(60))
			k.interval = tc.interval
			k.randInt64n = tc.draw

			require.Equal(t, tc.want, k.nextInterval())

			// 抖动幅度由常量决定：min(interval/5, 5min)，上下界对称。
			jitter := tc.interval / 5
			if jitter > 5*time.Minute {
				jitter = 5 * time.Minute
			}
			delay := k.nextInterval()
			require.GreaterOrEqual(t, delay, tc.interval-jitter)
			require.LessOrEqual(t, delay, tc.interval+jitter)
		})
	}
}

// ---------------------------------------------------------------------------
// 通知接缝（复用既有账号运维邮件通道）
// ---------------------------------------------------------------------------

type keeperFakeSettings struct {
	SettingRepository
	values map[string]string
}

func (s *keeperFakeSettings) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", errors.New("setting not found")
}

type keeperMail struct {
	to      string
	subject string
	body    string
}

type keeperFakeMailer struct {
	mu    sync.Mutex
	sends []keeperMail
	err   error
}

func (m *keeperFakeMailer) SendEmail(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends = append(m.sends, keeperMail{to: to, subject: subject, body: body})
	return m.err
}

func (m *keeperFakeMailer) recorded() []keeperMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]keeperMail(nil), m.sends...)
}

func TestZhipuCredentialAlertNotifierReusesAccountOpsChannel(t *testing.T) {
	verifiedPair := `[{"email":"ops-a@example.com","disabled":false,"verified":true},{"email":"ops-b@example.com","disabled":false,"verified":true}]`

	tests := []struct {
		name           string
		settings       map[string]string
		wantSends      int
		wantRecipients []string
	}{
		{
			name: "开关开启且有已验证收件人 → 逐人发送",
			settings: map[string]string{
				SettingKeyAccountQuotaNotifyEnabled: "true",
				SettingKeyAccountQuotaNotifyEmails:  verifiedPair,
				SettingKeySiteName:                  "中转站",
			},
			wantSends:      2,
			wantRecipients: []string{"ops-a@example.com", "ops-b@example.com"},
		},
		{
			name: "全局开关关闭 → 不发送",
			settings: map[string]string{
				SettingKeyAccountQuotaNotifyEnabled: "false",
				SettingKeyAccountQuotaNotifyEmails:  verifiedPair,
			},
			wantSends: 0,
		},
		{
			name: "未验证/停用收件人被过滤",
			settings: map[string]string{
				SettingKeyAccountQuotaNotifyEnabled: "true",
				SettingKeyAccountQuotaNotifyEmails:  `[{"email":"ops-a@example.com","disabled":false,"verified":false},{"email":"ops-b@example.com","disabled":true,"verified":true}]`,
			},
			wantSends: 0,
		},
		{
			name: "无收件人 → 不发送",
			settings: map[string]string{
				SettingKeyAccountQuotaNotifyEnabled: "true",
			},
			wantSends: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mailer := &keeperFakeMailer{}
			settings := &keeperFakeSettings{values: tc.settings}
			notifier := NewZhipuCredentialAlertNotifier(mailer, settings)

			notifier.NotifyZhipuCredentialInvalid(context.Background(), keeperManagedAccount(42, "zhipu-1", nil), "credit-usage 探针返回 HTTP 401")

			sends := mailer.recorded()
			require.Len(t, sends, tc.wantSends)
			if tc.wantSends > 0 {
				recipients := make([]string, 0, len(sends))
				for _, s := range sends {
					recipients = append(recipients, s.to)
					require.Contains(t, s.subject, "zhipu-1")
					require.Contains(t, s.body, "42")
					require.Contains(t, s.body, "401")
				}
				require.Equal(t, tc.wantRecipients, recipients)
			}
		})
	}
}

func TestZhipuCredentialAlertNotifierNilDependenciesAreSafe(t *testing.T) {
	require.NotPanics(t, func() {
		NewZhipuCredentialAlertNotifier(nil, nil).NotifyZhipuCredentialInvalid(context.Background(), keeperManagedAccount(42, "zhipu-1", nil), "401")
	})
}
