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

// 活跃上报执行器（A5 批 5.2；A6 批 P5 按合规裁定改为**仅手动**）。
//
// # ⚠️ 合规分级：`full` 级 = 仅手动，不得进自动排程
//
// 用户 2026-09-22 裁定的三级分级把活跃上报归为 **`full`**：
// 它复刻官方客户端 `chat_request_send` 事件形状、同一 conversationId 内 N 条、
// 条间 1.5s——这些行为的目的就是**伪造对话活跃以过 `chat_5` 门槛**。
// 授权记录见 `/Volumes/数据盘/网站/中转站/.scratch/codebuddy-impl/_briefs/00-shared.md`
// 的「用户授权记录」节。
//
// **授权了"做这个功能"，不等于授权"把它自动跑"**。所以：
//   - `Start()` 仍在（能力保留、测试可用），但 **wire 不再调用它**，
//     进程启动后不会有任何自动上报；
//   - 唯一入口是 `RunActivityNow`（管理端点 / 运维脚本手动触发）；
//   - `TestCodeBuddyActivityIsNeverAutoScheduled` 断言 wire 的 provider 不启动它。
//
// 若日后要恢复自动：必须先改分级并重走用户裁定，而不是把 `Start()` 加回去。
//
// # 与签到调度器的差异（保留说明，供恢复自动时参考）
//
// 形态照抄 A4 签到调度器（窗口内一次 + 当日去重）。差异在时间语义：
// 签到是用户配置的**时间段**；活跃上报参考实现是**单时点**（`activity_hours: [10]`），
// 风控口径要求"每号每天 1 次即可，不做多时点高频上报"。所以窗口默认 10:00–11:00。

const (
	// codeBuddyActivityTickSpec 每分钟一次（与签到同款 5 字段）。
	// **仅在显式调用 Start() 时才会用到**（当前 wire 不调用）。
	codeBuddyActivityTickSpec = "* * * * *"

	// codeBuddyActivityBatchSize 单轮最多处理的账号数。
	codeBuddyActivityBatchSize = 200

	// codeBuddyActivityRunTimeout 单轮执行的整体超时。
	//
	// 需要留足：单账号 5 条 × 条间 1.5s ≈ 6s，加上账号间 800ms 与 15s 请求超时，
	// 200 个账号串行远超 10 分钟——所以上限压到 batchSize 且超时给宽。
	codeBuddyActivityRunTimeout = 30 * time.Minute
)

var codeBuddyActivityCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// codeBuddyActivityRunner 执行器依赖的最小面（便于测试注入 stub）。
type codeBuddyActivityRunner interface {
	ListCodeBuddyActivityCandidates(ctx context.Context, limit int) ([]CodeBuddyCheckinCandidate, error)
	ReportCodeBuddyActivity(ctx context.Context, account *Account, count int) CodeBuddyActivityReportResult
}

// CodeBuddyActivityRunSummary 一轮活跃上报的汇总（手动入口的返回值）。
//
// 字段刻意区分"发了"与"验证过"：坑 1 说的静默丢弃正是"看起来成功"，
// 只报一个成功数会把这类问题掩盖掉。
type CodeBuddyActivityRunSummary struct {
	// Attempted 本轮考虑过的候选账号数（含被跳过的）。
	Attempted int `json:"attempted"`
	// Reported 实际发出上报的账号数（含自检可疑者）。
	Reported int `json:"reported"`
	// Skipped 被跳过（已停调 / 缺凭据）未发请求的账号数。
	Skipped int `json:"skipped"`
	// Failed 上报过程出错的账号数。
	Failed int `json:"failed"`
	// SelfCheckSuspicious 上报成功但自检可疑（days=0，疑似被静默丢弃）。
	SelfCheckSuspicious int `json:"self_check_suspicious"`
	// Error 顶层失败（如候选列表拉取失败）。非空时其余计数无意义——
	// 必须与"确实没有候选账号"区分开，否则一次 DB 故障会显示成"0 个账号"。
	Error string `json:"error,omitempty"`
}

// CodeBuddyActivityScheduler 活跃上报**执行器**（`full` 级，仅手动）。
//
// 名字保留 Scheduler 是历史原因；当前语义是"可手动调用的执行器"——
// 自动排程已按合规裁定移除（见上方注释）。
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

// RunActivityNow 手动执行一轮活跃上报（**唯一的生产入口**）。
//
// 绕开窗口/开关判定：手动触发意味着"人明确要求现在就报"，不该再被
// "当前不在 10 点档"或"平台功能开关关着"挡住——那会让管理员点了没反应、
// 且找不到原因。仍保留的参数是 `count`（单账号条数）与返回汇总。
//
// 分级：`full` 级（含伪造活跃上报语义），**只允许从这里手动调用**，
// 不得被任何自动排程依赖。
func (s *CodeBuddyActivityScheduler) RunActivityNow(ctx context.Context) CodeBuddyActivityRunSummary {
	if s == nil || s.runner == nil {
		return CodeBuddyActivityRunSummary{Error: "activity scheduler is not configured"}
	}
	return s.executeOnce(ctx)
}

// Start 启动每分钟 tick。
//
// ⚠️ **当前 wire 不调用它**：活跃上报是 `full` 级，按用户裁定仅手动
// （见文件头注释）。保留此方法是为了能力完整与测试可用；生产路径请用
// `RunActivityNow`。若你正打算把它加回 wire 的 provider——先去看文件头的
// 合规说明，以及 `TestCodeBuddyActivityIsNeverAutoScheduled`。
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

// runOnce 遍历候选账号逐号上报（自动排程入口；当前 wire 不调用）。
//
// 单号失败**不影响其他号**（参考实现同口径：失败只记 WARN 并继续下号）。
func (s *CodeBuddyActivityScheduler) runOnce(localDate string, start, end TimeOfDay) {
	summary := s.executeOnce(context.Background())
	// ⚠️ 有自检异常或失败时**降到 WARN**：`reported` 只表示"请求发出去了"，
	// 而自检异常恰恰是"发出去了但可能被上游静默丢弃"的信号（坑 1）。
	// 一律 Info 会让运维在日志里看到一条"成功"汇总，把可疑轮次当正常。
	attrs := []any{
		"local_date", localDate,
		"window", start.Format() + "-" + end.Format(),
		"total", summary.Attempted,
		"reported", summary.Reported,
		"skipped", summary.Skipped,
		"failed", summary.Failed,
		"self_check_failed", summary.SelfCheckSuspicious,
	}
	if summary.Failed > 0 || summary.SelfCheckSuspicious > 0 {
		slog.Warn("codebuddy_activity.window_run_degraded", attrs...)
		return
	}
	slog.Info("codebuddy_activity.window_run", attrs...)
}

// executeOnce 是**自动与手动共用**的执行体：遍历候选账号逐号上报并汇总。
//
// 抽成一处是刻意的：手动路径（`RunActivityNow`）与自动路径若各写一份遍历，
// 迟早会在"跳过判定 / 单号失败是否继续 / 自检计数"上漂移——
// 而这两条路径的差别**只在触发方式**，不在执行语义。
func (s *CodeBuddyActivityScheduler) executeOnce(parent context.Context) CodeBuddyActivityRunSummary {
	ctx, cancel := context.WithTimeout(parent, codeBuddyActivityRunTimeout)
	defer cancel()

	summary := CodeBuddyActivityRunSummary{}
	if s == nil || s.runner == nil {
		summary.Error = "activity scheduler is not configured"
		return summary
	}

	candidates, err := s.runner.ListCodeBuddyActivityCandidates(ctx, s.batchSize)
	if err != nil {
		slog.Warn("codebuddy_activity.list_failed", "error", err)
		// 与"确实没有候选账号"区分开：否则一次 DB 故障会显示成"0 个账号"。
		summary.Error = "list activity candidates failed"
		return summary
	}
	summary.Attempted = len(candidates)

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

	summary.Skipped = skipped
	summary.Failed = failed
	summary.Reported = reported
	summary.SelfCheckSuspicious = selfCheckFailed

	s.mu.Lock()
	s.lastSelfCheckFailed = selfCheckFailed
	s.mu.Unlock()
	return summary
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
