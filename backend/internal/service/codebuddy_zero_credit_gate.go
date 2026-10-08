package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 票 #37：CodeBuddy 0 积分主动门（周期探测 + 自动恢复）。
//
// # 与 A7 被动冷却的分工（两条通道互补，不是重复）
//
//   - A7（codebuddy_account_cooldown.go）是**被动**的：请求已经打到该账号、
//     上游回 402 / 业务码 14018 之后才冷，代价是那一发失败请求（用户可见）。
//   - 本门是**主动**的：周期查余额，积分归零即提前摘出池子，把第一个失败请求
//     省掉；积分恢复（签到/充值）后再自动放回。
//
// 两条通道共用 TempUnschedulableUntil 字段，靠 **reason 前缀**区分所有权：
// 本门写 `codebuddy_zero_credit`，A7 写 `codebuddy_account_cooldown:<tag>`。
// 本门的 Clear **只清自己写的那条**——清掉 A7 的 404 浅冷却会让它静默失效。
//
// # 三条硬边界（红线）
//
//  1. 只动 codebuddy 平台账号（逐号 IsCodeBuddy 严判，不信列表来源）；
//  2. 查询失败 = **不作为**（fail-open）：既不摘号也不解冻，只记退避；
//     「探测通道坏了」绝不能变成「把好账号摘光」。
//  3. 不碰 manual_disabled / 签到 / growth 任何既有逻辑——本门只读写
//     temp_unschedulable 两列。

const (
	// codeBuddyZeroCreditReasonPrefix 本门写入的停调理由（前缀）。
	// 用前缀而不是全等：与 A7 的 `codebuddy_account_cooldown:` 区分所有权，
	// 且留出后续补充余额明细的余地（那时 Clear 判定无需改动）。
	codeBuddyZeroCreditReasonPrefix = "codebuddy_zero_credit"

	// codeBuddyZeroCreditProbeTimeout 单账号探测超时（含所有候选路径）。
	// 比 A2 的 15s 出站超时更短：本门是周期任务，探测拖长会挤占整轮预算，
	// 而「这轮查不到」的代价只是下轮再查（fail-open）。
	codeBuddyZeroCreditProbeTimeout = 10 * time.Second

	// codeBuddyZeroCreditDefaultIntervalMinutes 探测周期默认值（分钟）。
	codeBuddyZeroCreditDefaultIntervalMinutes = 30

	// codeBuddyZeroCreditProbeBackoffCycles 查询失败后跳过该账号的周期数。
	// 账号级退避而不是整轮熔断：单号凭据/上游抖动不该让其他账号的探测停摆。
	codeBuddyZeroCreditProbeBackoffCycles = 2
)

// codeBuddyCreditsProber 本门依赖的**窄接口**（*CodeBuddyCreditsFetcher 实现）。
// 收窄到单个方法：测试注入桩不必构造 HTTP 栈，也避免本门依赖具体 fetcher 类型。
type codeBuddyCreditsProber interface {
	FetchCredits(ctx context.Context, opts *CodeBuddyCreditsFetchOptions) (*UpstreamBalanceUsage, error)
}

// CodeBuddyZeroCreditRoundSummary 单轮探测汇总（日志与测试共用口径）。
//
// Checked 只统计**成功拿到余额**的账号（「探到了」而不是「探过」）；Failed 是探测
// 失败（含余额不可用）；Skipped 是「本轮有意不探」（退避期内、已被别的子系统停调）。
// 三者互斥。
type CodeBuddyZeroCreditRoundSummary struct {
	Checked   int
	Blocked   int
	Recovered int
	Skipped   int
	Failed    int
}

// CodeBuddyZeroCreditGate 周期探测 CodeBuddy 账号积分并停调/恢复。
// 克隆自 CNProviderBalanceCheckService 的 Start/Stop/runOnce + ticker 骨架。
type CodeBuddyZeroCreditGate struct {
	accountRepo AccountRepository
	prober      codeBuddyCreditsProber
	cfg         *config.Config
	interval    time.Duration

	// clock 供测试注入固定时刻（生产恒为 time.Now）。
	clock func() time.Time

	mu       sync.Mutex
	backoff  map[int64]int
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
	started  bool
	stopped  bool
}

// NewCodeBuddyZeroCreditGate 构造主动门。Start 由 wire 的 Provider 调用一次；
// interval <= 0 时 Start 不启动（防御性：Provider 已把 <=0 回落成默认周期，
// 关门的正门开关是 gateway.codebuddy.zero_credit_gate_enabled）。
func NewCodeBuddyZeroCreditGate(
	accountRepo AccountRepository,
	prober codeBuddyCreditsProber,
	cfg *config.Config,
	interval time.Duration,
) *CodeBuddyZeroCreditGate {
	return &CodeBuddyZeroCreditGate{
		accountRepo: accountRepo,
		prober:      prober,
		cfg:         cfg,
		interval:    interval,
		clock:       time.Now,
		stopCh:      make(chan struct{}),
	}
}

func (s *CodeBuddyZeroCreditGate) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

// enabled 报告本门是否应当动作。开关是**配置层**键
// gateway.codebuddy.zero_credit_gate_enabled（默认 true，见 config.setDefaults）。
// cfg 为 nil 时按关闭处理：construct 阶段拿不到配置就不该动账号状态。
func (s *CodeBuddyZeroCreditGate) enabled() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	return s.cfg.Gateway.CodeBuddy.ZeroCreditGateEnabled
}

// Start 启动周期探测。重复调用安全；开关关闭或 interval <= 0 时不启动
// （构造仍成功，便于配置回滚）。
func (s *CodeBuddyZeroCreditGate) Start() {
	if s == nil || s.accountRepo == nil || s.prober == nil || !s.enabled() || s.interval <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		// 启动后先等一个周期再首次探测：避免与进程启动峰（连接池预热、缓存加载）
		// 叠加；本门的收益是"提前摘出"，晚 30 分钟不影响正确性（靠 A7 被动兜底）。
		for {
			select {
			case <-ticker.C:
				s.tick()
			case <-s.stopCh:
				return
			}
		}
	}()
	slog.Info("codebuddy_zero_credit.started", "interval", s.interval)
}

// Stop 停止周期探测（进程退出时调用，幂等）。停止后不再接受 Start
// （stopCh 已关，重启会让新 goroutine 立刻退出）。
func (s *CodeBuddyZeroCreditGate) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *CodeBuddyZeroCreditGate) isStarted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// tick 单次周期判定（ticker 与测试共用入口）：开关关闭时一个请求都不发。
func (s *CodeBuddyZeroCreditGate) tick() {
	if s == nil || !s.enabled() {
		return
	}
	s.runOnce(context.Background())
}

// runOnce 扫一轮 codebuddy 账号并按余额决定停调/恢复。
//
// 逐号判定，任何单号失败都不影响其他账号（与签到/保活同款隔离语义）。
func (s *CodeBuddyZeroCreditGate) runOnce(ctx context.Context) CodeBuddyZeroCreditRoundSummary {
	var summary CodeBuddyZeroCreditRoundSummary
	if s == nil || s.accountRepo == nil || s.prober == nil {
		return summary
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformCodeBuddy)
	if err != nil {
		slog.Warn("codebuddy_zero_credit.list_failed", "error", err)
		return summary
	}

	now := s.now()
	for i := range accounts {
		account := &accounts[i]
		if !codeBuddyZeroCreditProbeTarget(account) {
			continue
		}
		// 已在**别人的**停调窗口内（A7 冷却 / 保活三振 / 运营手动）：本轮不探也
		// 不动，绝不覆盖它写下的 reason。对照：本门自己的块必须照常探测——续期
		// 与解冻都靠这一轮余额。
		if codeBuddyZeroCreditBlockedByOther(account, now) {
			summary.Skipped++
			continue
		}
		if !s.probeAllowed(account.ID) {
			// 上一轮查询失败 → 本轮退避（不打上游、不动状态）。
			summary.Skipped++
			continue
		}
		credits, ok := s.probeCredits(ctx, account)
		if !ok {
			// 查询失败：不作为（fail-open），绝不因探测通道故障误摘账号。
			// 只对该账号退避（单号凭据坏掉不该拖停其他账号的探测）。
			s.markProbeFailure(account.ID)
			summary.Failed++
			continue
		}
		s.clearProbeBackoff(account.ID)
		summary.Checked++
		if credits > 0 {
			// 积分恢复（签到/充值到账）→ 立即放回池子，但**只清本门写的那条块**
			// （reason 前缀匹配）。其余来源的停调（A7 冷却、保活三振、运营手动）
			// 各有自己的恢复条件，被余额恢复顺手清掉会让它们静默失效。
			if !account.HasCodeBuddyZeroCreditBlock() {
				continue
			}
			if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
				slog.Warn("codebuddy_zero_credit.clear_failed", "account_id", account.ID, "error", err)
				continue
			}
			summary.Recovered++
			slog.Info("codebuddy_zero_credit.recovered", "account_id", account.ID, "credits", credits)
			continue
		}
		// 积分归零 → 摘出到次日 04:00 UTC+8（与 A7 硬冷却同时点：签到恢复后
		// 自然回池）。SetTempUnschedulable 是「只延长」写：已在更晚的窗口里时
		// 不会缩短它，也不会把 A7 的 reason 覆盖成更短窗口。
		until := nextCodeBuddyFourAM(now)
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, codeBuddyZeroCreditReasonPrefix); err != nil {
			slog.Warn("codebuddy_zero_credit.block_failed", "account_id", account.ID, "error", err)
			continue
		}
		summary.Blocked++
		slog.Info("codebuddy_zero_credit.blocked",
			"account_id", account.ID, "credits", credits, "until", until.Format(time.RFC3339))
	}

	slog.Info("codebuddy_zero_credit.round",
		"checked", summary.Checked,
		"blocked", summary.Blocked,
		"recovered", summary.Recovered,
		"skipped", summary.Skipped,
		"failed", summary.Failed,
	)
	return summary
}

// probeAllowed 本轮是否可探测该账号。退避计数在**本次调用**递减：
// 一次失败置 2 → 紧随的两轮跳过 → 第 3 轮重新探测。
func (s *CodeBuddyZeroCreditGate) probeAllowed(accountID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backoff[accountID] <= 0 {
		return true
	}
	s.backoff[accountID]--
	return false
}

// markProbeFailure 记一次探测失败并进入退避窗口。
func (s *CodeBuddyZeroCreditGate) markProbeFailure(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backoff == nil {
		s.backoff = map[int64]int{}
	}
	s.backoff[accountID] = codeBuddyZeroCreditProbeBackoffCycles
}

// clearProbeBackoff 探测成功后清零退避，避免"坏过一次就一直被跳过"。
func (s *CodeBuddyZeroCreditGate) clearProbeBackoff(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.backoff, accountID)
}

// codeBuddyZeroCreditProbeTarget 本门可探测的账号形态：codebuddy + active +
// 有 access token。无 token 时探测必然失败，直接跳过（不浪费一次出站）。
func codeBuddyZeroCreditProbeTarget(account *Account) bool {
	if account == nil || !account.IsCodeBuddy() || !account.IsActive() {
		return false
	}
	return strings.TrimSpace(account.GetCodeBuddyAccessToken()) != ""
}

// codeBuddyZeroCreditBlockedByOther 账号是否正被**其它**子系统停调
// （窗口未过期 + reason 不是本门前缀）。「未过期」是关键：已过期的窗口不再挡选号，
// 本门可以正常接手。
func codeBuddyZeroCreditBlockedByOther(account *Account, now time.Time) bool {
	if account == nil || account.TempUnschedulableUntil == nil {
		return false
	}
	if !now.Before(*account.TempUnschedulableUntil) {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(account.TempUnschedulableReason), codeBuddyZeroCreditReasonPrefix)
}

// HasCodeBuddyZeroCreditBlock 账号当前是否带着**本门**写的停调块
// （reason 前缀匹配 + until 在场）。
//
// 这是 Clear 的所有权判定：只看前缀，不看窗口是否已过期——已过期的本门块也该被
// 顺手清干净（否则理由字段会永远挂在账号上），而别人的块一律不认。
// 判定大小写敏感、不做子串宽容：A7 的 `codebuddy_account_cooldown:` 与本门
// `codebuddy_zero_credit` 是两条独立通道，误判就是互相拆台。
func (a *Account) HasCodeBuddyZeroCreditBlock() bool {
	if a == nil || !a.IsCodeBuddy() {
		return false
	}
	if a.TempUnschedulableUntil == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(a.TempUnschedulableReason), codeBuddyZeroCreditReasonPrefix)
}

// probeCredits 查一次实时积分余额（只读，不改任何账号状态）。
// ok=false 表示「本轮拿不到可信余额」——查询失败、余额字段缺失都算，
// 与「余额确实是 0」严格区分（把 nil 当 0 会误摘账号）。
func (s *CodeBuddyZeroCreditGate) probeCredits(ctx context.Context, account *Account) (float64, bool) {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	probeCtx, cancel := context.WithTimeout(ctx, codeBuddyZeroCreditProbeTimeout)
	defer cancel()
	snapshot, err := s.prober.FetchCredits(probeCtx, &CodeBuddyCreditsFetchOptions{
		AccessToken: account.GetCodeBuddyAccessToken(),
		ProxyURL:    proxyURL,
		AccountID:   account.ID,
		Account:     account, // realm 判定用（决定计费域名与路径族）
	})
	if err != nil || snapshot == nil || snapshot.Status != "ok" || snapshot.Balance == nil {
		return 0, false
	}
	return *snapshot.Balance, true
}
