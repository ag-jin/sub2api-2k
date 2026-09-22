package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/robfig/cron/v3"
)

// 成长任务链调度（A6 批 P5）。
//
// ## 形态：照抄 A4/A5 的窗口内一次 + 当日去重
//
// 每分钟 tick，每次判三件事：功能开关（默认关闭）→ 是否在窗口内 → 当日是否已执行。
// 不另造一套（A4 的 `codebuddy_checkin_scheduler.go` 是同一个模板）。
//
// ## ⚠️ 本调度器**只能**跑 `preview` / `claim` 级通道
//
// 取通道列表的唯一入口是 `codebuddy.CodeBuddyGrowthAutoSchedulableChannelKeys()`
// ——它按 `GrowthTier.AutoSchedulable()` 过滤，`full` 级（含伪造活跃上报语义）
// 在**类型层面**进不来。**不要**改成遍历 `CodeBuddyGrowthChannelSpecs` 自己过滤：
// 那样一旦有人新增 full 级通道，它会被静默纳入自动排程（用户裁定的合规红线）。
//
// 领养（adopt）、夜猫子（night_cat）、开学季点亮（school）都是 full 级，
// 它们的手动入口在管理端点，见 `codebuddy_growth_manual.go`。

const (
	// codeBuddyGrowthTickSpec 每分钟一次（与签到/活跃上报同款 5 字段）。
	codeBuddyGrowthTickSpec = "* * * * *"

	// codeBuddyGrowthRunTimeout 单轮整体超时。
	// 需要容纳"账号数 ×（连登链多步 + 旅行闭环）+ 账号间 800ms 间隔"。
	codeBuddyGrowthRunTimeout = 30 * time.Minute

	// codeBuddyGrowthBatchSize 单轮最多处理的账号数。
	codeBuddyGrowthBatchSize = 200
)

var codeBuddyGrowthCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// codeBuddyGrowthRunner 调度器依赖的最小面（便于测试注入 stub）。
type codeBuddyGrowthRunner interface {
	ListCodeBuddyGrowthCandidates(ctx context.Context, limit int) ([]codeBuddyGrowthCandidate, error)
	RunCodeBuddyGrowthChannels(ctx context.Context, account *Account, localDay string, channelKeys []string) CodeBuddyGrowthAccountSummary
}

// codeBuddyGrowthCandidate 一个待处理的账号（含跳过原因）。
type codeBuddyGrowthCandidate struct {
	AccountID int64
	Name      string
	// SkipReason 非空 = 本轮不发请求（与"失败"区分：失败是签到了但出错）。
	SkipReason string
}

// CodeBuddyGrowthScheduler 按窗口自动执行成长链（仅 preview/claim 级通道）。
type CodeBuddyGrowthScheduler struct {
	runner         codeBuddyGrowthRunner
	accountRepo    AccountRepository
	settingService *SettingService
	clock          func() time.Time
	batchSize      int
	accountDelay   func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	cron    *cron.Cron
	started bool
	stopped bool

	// lastRunDate 已执行过的当地日期（UTC+8，与 A4/A5 同口径）。
	lastRunDate string
	// lastSummary 最近一轮的汇总（仅测试断言用）。
	lastSummary CodeBuddyGrowthRunSummary
}

// CodeBuddyGrowthRunSummary 一轮成长链执行的汇总。
type CodeBuddyGrowthRunSummary struct {
	// Attempted 本轮考虑过的账号数。
	Attempted int `json:"attempted"`
	// Succeeded 至少完成一个动作的账号数。
	Succeeded int `json:"succeeded"`
	// Skipped 被跳过（停调 / 缺凭据 / 各有业务原因）的账号数。
	Skipped int `json:"skipped"`
	// Failed 过程出错的账号数。
	Failed int `json:"failed"`
	// Channels 本轮实际跑的通道键（**测试断言 full 不在此列**）。
	Channels []string `json:"channels"`
	// Error 顶层失败（取候选失败等）。
	Error string `json:"error,omitempty"`
}

// NewCodeBuddyGrowthScheduler 构造成长链调度器。
func NewCodeBuddyGrowthScheduler(
	runner codeBuddyGrowthRunner,
	accountRepo AccountRepository,
	settingService *SettingService,
) *CodeBuddyGrowthScheduler {
	return &CodeBuddyGrowthScheduler{
		runner:         runner,
		accountRepo:    accountRepo,
		settingService: settingService,
		clock:          time.Now,
		batchSize:      codeBuddyGrowthBatchSize,
		accountDelay:   codeBuddyActivitySleep,
	}
}

// Start 启动每分钟 tick。重复调用安全。
func (s *CodeBuddyGrowthScheduler) Start() {
	if s == nil || s.runner == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true

	c := cron.New(cron.WithParser(codeBuddyGrowthCronParser))
	if _, err := c.AddFunc(codeBuddyGrowthTickSpec, func() { s.tick() }); err != nil {
		slog.Error("codebuddy_growth.schedule_failed", "error", err)
		return
	}
	s.cron = c
	c.Start()
	slog.Info("codebuddy_growth.scheduler_started", "spec", codeBuddyGrowthTickSpec)
}

// Stop 停止调度（进程退出时调用）。幂等。
func (s *CodeBuddyGrowthScheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	c := s.cron
	s.cron = nil
	s.stopped = true
	s.mu.Unlock()
	if c != nil {
		c.Stop()
	}
}

// tick 单次判定 + 执行（三道门与签到/活跃上报同序）。
func (s *CodeBuddyGrowthScheduler) tick() {
	if s == nil || s.runner == nil || s.settingService == nil {
		return
	}
	now := s.clock()

	settings, err := s.settingService.GetAllSettings(context.Background())
	if err != nil {
		slog.Warn("codebuddy_growth.settings_read_failed", "error", err)
		return
	}
	features := settings.PlatformFeatures

	// 1) 开关（默认关闭）。未开启直接返回，连窗口都不看。
	if !PlatformFeatureEnabled(features, PlatformCodeBuddy, CodeBuddyGrowthFeatureKey) {
		return
	}

	start, end, location, ok := ResolvePlatformFeatureTimeRange(
		features, PlatformCodeBuddy, CodeBuddyGrowthFeatureKey,
	)
	if !ok {
		return
	}

	// 2) 窗口。
	if !WithinTimeRange(now, start, end, location) {
		return
	}

	// 3) 当日已执行？
	localDay := PlatformFeatureLocalDate(now, location).Format(time.DateOnly)
	s.mu.Lock()
	if s.lastRunDate == localDay {
		s.mu.Unlock()
		return
	}
	// 乐观占位（与 A4/A5 一致）：先记日期再执行，避免单轮超过 tick 间隔被重入；
	// 失败不回滚——回滚会让"持续失败"退化成"每分钟打一遍上游"。
	s.lastRunDate = localDay
	s.mu.Unlock()

	s.runOnce(localDay, start, end)
}

// runOnce 遍历候选账号逐号执行成长链。
func (s *CodeBuddyGrowthScheduler) runOnce(localDay string, start, end TimeOfDay) {
	ctx, cancel := context.WithTimeout(context.Background(), codeBuddyGrowthRunTimeout)
	defer cancel()

	// ⚠️ 唯一入口：已按分级过滤（full 级进不来）。见文件头说明。
	channelKeys := codebuddy.CodeBuddyGrowthAutoSchedulableChannelKeys()

	candidates, err := s.runner.ListCodeBuddyGrowthCandidates(ctx, s.batchSize)
	if err != nil {
		slog.Warn("codebuddy_growth.list_failed", "error", err)
		return
	}

	summary := CodeBuddyGrowthRunSummary{Attempted: len(candidates), Channels: channelKeys}
	first := true
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			slog.Warn("codebuddy_growth.stopped_early", "remaining", len(candidates))
			break
		}
		if candidate.SkipReason != "" {
			summary.Skipped++
			continue
		}
		if !first {
			if err := s.accountDelay(ctx, codeBuddyGrowthAccountDelay); err != nil {
				slog.Warn("codebuddy_growth.cancelled_between_accounts", "error", err)
				break
			}
		}
		first = false

		account, loadErr := s.accountRepo.GetByID(ctx, candidate.AccountID)
		if loadErr != nil || account == nil {
			summary.Failed++
			slog.Warn("codebuddy_growth.account_load_failed",
				"account_id", candidate.AccountID, "error", loadErr)
			continue
		}
		// 单号失败不影响其他号（与 A5 同口径）。
		perAccount := s.runner.RunCodeBuddyGrowthChannels(ctx, account, localDay, channelKeys)
		if perAccount.Error != "" {
			summary.Failed++
			slog.Warn("codebuddy_growth.account_failed",
				"account_id", candidate.AccountID, "error", perAccount.Error)
			continue
		}
		summary.Succeeded++
	}

	s.mu.Lock()
	s.lastSummary = summary
	s.mu.Unlock()

	attrs := []any{
		"local_date", localDay,
		"window", start.Format() + "-" + end.Format(),
		"channels", channelKeys,
		"total", summary.Attempted,
		"succeeded", summary.Succeeded,
		"skipped", summary.Skipped,
		"failed", summary.Failed,
	}
	if summary.Failed > 0 {
		slog.Warn("codebuddy_growth.window_run_degraded", attrs...)
		return
	}
	slog.Info("codebuddy_growth.window_run", attrs...)
}

// codeBuddyGrowthSchedulerSummaryForTest 暴露最近一轮汇总（仅测试用）。
func (s *CodeBuddyGrowthScheduler) codeBuddyGrowthSchedulerSummaryForTest() CodeBuddyGrowthRunSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSummary
}

// codeBuddyGrowthSchedulerLastRunDateForTest 暴露当日去重状态（仅测试用）。
func (s *CodeBuddyGrowthScheduler) codeBuddyGrowthSchedulerLastRunDateForTest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRunDate
}
