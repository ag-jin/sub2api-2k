package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/robfig/cron/v3"
)

// codeBuddyActivityReportCount 单账号单轮上报条数。
// 5 条对齐 `chat_5` 前置（领养要求同一会话内 5 次对话）。
//
// 注：CodeBuddyActivityFeatureKey 与默认窗口常量已由
// codebuddy_platform_features.go 声明（5.4 注册那批），此处不重复定义。
const codeBuddyActivityReportCount = 5

// ListCodeBuddyActivityCandidates 列出活跃上报候选账号（含跳过标记）。
//
// 与签到候选同形（复用 AccountRepository.ListByPlatform，不新增仓储方法），
// 但**多一条 uid 检查**：上报事件必须带 userId（= uid），缺失时上游 200 静默丢弃
// （见 platform/codebuddy activity_report.go 文件头）。宁可在这里标 skipped，
// 也不要发一个注定被丢弃的请求。
func (s *CodeBuddyAdminService) ListCodeBuddyActivityCandidates(ctx context.Context, limit int) ([]CodeBuddyCheckinCandidate, error) {
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
		case strings.TrimSpace(account.GetCredential("uid")) == "":
			// uid 缺失 = 事件必被静默丢弃，直接跳过（坑 1 的前置拦截）。
			candidate.SkipReason = "账号缺少 uid（上报必被上游静默丢弃）"
		}
		candidates = append(candidates, candidate)
		if len(candidates) >= limit {
			break
		}
	}
	return candidates, nil
}

// 活跃上报调度（A5 批 5.2）。
//
// 形态**照抄 A4 的签到调度器**（codebuddy_checkin_scheduler.go）——同款需求：
// 一个**时点/时段**触发一次、当日去重、默认关闭、多实例靠上游幂等兜底。
// 不另造一套。
//
// 与签到的**语义差异**（决定了这里用"时点"而非"窗口"）：
//   - 签到是用户配置的**时间段**（窗口内执行一次），因为签到时刻可弹性；
//   - 活跃上报参考实现是**单时点**（`activity_hours: [10]`），风控口径要求
//     "每号每天 1 次即可，不做多时点高频上报"。
//
// 所以这里注册的功能项用 **time_range** 形态、默认 10:00–11:00：
// 既保留了 A4 基础设施的"窗口内一次"语义（进程重启/迟到启动都不漏），
// 又天然把上报压在一天一个时段内，符合风控口径。

const (
	// codeBuddyActivityTickSpec 每分钟一次（与签到同款 5 字段）。
	codeBuddyActivityTickSpec = "* * * * *"

	// codeBuddyActivityBatchSize 单轮最多处理的账号数。
	codeBuddyActivityBatchSize = 200

	// codeBuddyActivityRunTimeout 单轮 tick 的整体超时。
	//
	// 需要留足：单账号 5 条 × 条间 1.5s ≈ 6s，加上账号间 800ms 与 15s 请求超时，
	// 200 个账号串行远超 10 分钟——所以上限压到 batchSize 且超时给宽。
	codeBuddyActivityRunTimeout = 30 * time.Minute
)

var codeBuddyActivityCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// codeBuddyActivityRunner 调度器依赖的最小面（便于测试注入 stub）。
type codeBuddyActivityRunner interface {
	ListCodeBuddyActivityCandidates(ctx context.Context, limit int) ([]CodeBuddyCheckinCandidate, error)
	ReportCodeBuddyActivity(ctx context.Context, account *Account, count int) CodeBuddyActivityReportResult
}

// CodeBuddyActivityScheduler 按配置的时段自动上报对话活跃。
type CodeBuddyActivityScheduler struct {
	runner         codeBuddyActivityRunner
	accountRepo    AccountRepository
	settingService *SettingService
	clock          func() time.Time
	batchSize      int
	// reportCount 单账号上报条数（默认 5，对齐 chat_5 前置）。
	reportCount int
	// accountDelay 账号之间的间隔（可注入，测试置 0）。
	accountDelay func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	cron    *cron.Cron
	started bool
	stopped bool

	// lastRunDate 记录"已执行过的当地日期"（UTC+8，与签到同口径）。
	lastRunDate string

	// lastSelfCheckFailed 最近一轮的自检异常账号数（仅测试断言用；生产只看日志）。
	lastSelfCheckFailed int
}

// NewCodeBuddyActivityScheduler 构造活跃上报调度器。
func NewCodeBuddyActivityScheduler(
	runner codeBuddyActivityRunner,
	accountRepo AccountRepository,
	settingService *SettingService,
) *CodeBuddyActivityScheduler {
	return &CodeBuddyActivityScheduler{
		runner:         runner,
		accountRepo:    accountRepo,
		settingService: settingService,
		clock:          time.Now,
		batchSize:      codeBuddyActivityBatchSize,
		reportCount:    codeBuddyActivityReportCount,
		accountDelay:   codeBuddyActivitySleep,
	}
}

// Start 启动每分钟 tick。重复调用安全。
func (s *CodeBuddyActivityScheduler) Start() {
	if s == nil || s.runner == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true

	c := cron.New(cron.WithParser(codeBuddyActivityCronParser))
	if _, err := c.AddFunc(codeBuddyActivityTickSpec, func() { s.tick() }); err != nil {
		slog.Error("codebuddy_activity.schedule_failed", "error", err)
		return
	}
	s.cron = c
	c.Start()
	slog.Info("codebuddy_activity.scheduler_started", "spec", codeBuddyActivityTickSpec)
}

// Stop 停止调度（进程退出时调用）。幂等。
func (s *CodeBuddyActivityScheduler) Stop() {
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

// tick 单次判定 + 执行。
//
// 三道门与签到同序：**开关 → 窗口 → 当日去重**。
// 开关在最前：未开启时连窗口都不看，保证"默认关闭"真的一个请求都不发。
func (s *CodeBuddyActivityScheduler) tick() {
	if s == nil || s.runner == nil || s.settingService == nil {
		return
	}
	now := s.clock()

	settings, err := s.settingService.GetAllSettings(context.Background())
	if err != nil {
		slog.Warn("codebuddy_activity.settings_read_failed", "error", err)
		return
	}
	features := settings.PlatformFeatures

	// 1) 开关（默认关闭）。
	if !PlatformFeatureEnabled(features, PlatformCodeBuddy, CodeBuddyActivityFeatureKey) {
		return
	}

	start, end, location, ok := ResolvePlatformFeatureTimeRange(
		features, PlatformCodeBuddy, CodeBuddyActivityFeatureKey,
	)
	if !ok {
		return
	}

	// 2) 窗口。
	if !WithinTimeRange(now, start, end, location) {
		return
	}

	// 3) 当日已执行？（乐观占位，语义与签到完全一致：执行失败不回滚，
	//    否则"持续失败"会退化成"每分钟打上游"，正是风控口径反对的形态。）
	localDate := PlatformFeatureLocalDate(now, location).Format(time.DateOnly)
	s.mu.Lock()
	if s.lastRunDate == localDate {
		s.mu.Unlock()
		return
	}
	s.lastRunDate = localDate
	s.mu.Unlock()

	s.runOnce(localDate, start, end)
}

// runOnce 遍历候选账号逐号上报。
//
// 单号失败**不影响其他号**（参考实现同口径：失败只记 WARN 并继续下号）。
func (s *CodeBuddyActivityScheduler) runOnce(localDate string, start, end TimeOfDay) {
	ctx, cancel := context.WithTimeout(context.Background(), codeBuddyActivityRunTimeout)
	defer cancel()

	candidates, err := s.runner.ListCodeBuddyActivityCandidates(ctx, s.batchSize)
	if err != nil {
		slog.Warn("codebuddy_activity.list_failed", "error", err)
		return
	}

	var reported, skipped, failed, selfCheckFailed int
	first := true
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			slog.Warn("codebuddy_activity.stopped_early", "error", ctx.Err(), "remaining", len(candidates))
			break
		}
		// 跳过项（停调 / 缺凭据）不发请求——与签到同口径。
		if candidate.SkipReason != "" {
			skipped++
			continue
		}
		if !first {
			if err := s.accountDelay(ctx, codeBuddyActivityAccountDelay); err != nil {
				slog.Warn("codebuddy_activity.cancelled_between_accounts", "error", err)
				break
			}
		}
		first = false

		account, loadErr := s.accountRepo.GetByID(ctx, candidate.AccountID)
		if loadErr != nil || account == nil {
			failed++
			slog.Warn("codebuddy_activity.account_load_failed",
				"account_id", candidate.AccountID, "error", loadErr)
			continue
		}
		result := s.runner.ReportCodeBuddyActivity(ctx, account, s.reportCount)
		if result.Err != nil {
			// 单号失败：记 WARN 继续下号（不让一个坏号拖累整轮）。
			failed++
			slog.Warn("codebuddy_activity.account_report_failed",
				"account_id", candidate.AccountID,
				"reported", result.Reported, "expected", result.Expected,
				"error", result.Err)
			continue
		}
		reported++
		if result.SelfCheckFailed {
			selfCheckFailed++
		}
	}

	// ⚠️ 有自检异常或失败时**降到 WARN**：`reported` 只表示"请求发出去了"，
	// 而自检异常恰恰是"发出去了但可能被上游静默丢弃"的信号（坑 1）。
	// 一律 Info 会让运维在日志里看到一条"成功"汇总，把可疑轮次当正常。
	attrs := []any{
		"local_date", localDate,
		"window", start.Format() + "-" + end.Format(),
		"total", len(candidates),
		"reported", reported,
		"skipped", skipped,
		"failed", failed,
		"self_check_failed", selfCheckFailed,
	}
	s.mu.Lock()
	s.lastSelfCheckFailed = selfCheckFailed
	s.mu.Unlock()

	if failed > 0 || selfCheckFailed > 0 {
		slog.Warn("codebuddy_activity.window_run_degraded", attrs...)
		return
	}
	slog.Info("codebuddy_activity.window_run", attrs...)
}

// codeBuddyActivitySelfCheckFailedForTest 暴露最近一轮的自检异常数（仅测试用）。
func (s *CodeBuddyActivityScheduler) codeBuddyActivitySelfCheckFailedForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSelfCheckFailed
}

// CodeBuddyActivitySchedulerLastRunDate 暴露当日去重状态（仅测试用）。
func (s *CodeBuddyActivityScheduler) CodeBuddyActivitySchedulerLastRunDate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRunDate
}
