package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// 签到调度（A4 批 4.1/4.2）。
//
// 形态：每分钟一次的 tick（cron 5 字段 `* * * * *`），每次 tick 判断三件事——
//   1. 该平台的功能开关是否开启（默认关闭：没显式打开就一个上游请求都不发）
//   2. 当前时刻是否落在配置的签到窗口内（默认 09:00–11:00，UTC+8）
//   3. 当地日期下是否已经执行过
//
// 为什么用 tick 而不是把 cron 精确到窗口起点：窗口是**区间**语义（"这段时间内签到一次"），
// 提前把 cron 定到 09:00 会让"实例在 09:30 才启动"直接错过当天；tick 形态天然覆盖
// 进程重启、窗口跨界等情况。同理，**绝不做成"窗口内每分钟都试一遍"**——那会持续打上游，
// 当日已执行（或上游回 10001 幂等）后这一天就不再发请求。
//
// 多实例：与既有 cron 调度器同款，不做 leader 选举，靠上游 10001 幂等 + 本进程的
// 当日去重兜底；重复签到的代价是一次幂等请求，而不是重复发放积分。

const (
	// codeBuddyCheckinTickSpec 每分钟一次（5 字段：分 时 日 月 周）。
	codeBuddyCheckinTickSpec = "* * * * *"

	// codeBuddyCheckinBatchSize 单轮最多处理的账号数（防止极端体量一次打爆上游）。
	codeBuddyCheckinBatchSize = 200

	// codeBuddyCheckinRunTimeout 单轮 tick 的整体超时。
	codeBuddyCheckinRunTimeout = 10 * time.Minute
)

var codeBuddyCheckinCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// codeBuddyCheckinRunner 调度器依赖的最小面（便于测试注入 stub，不依赖具体 service 类型）。
type codeBuddyCheckinRunner interface {
	ListCodeBuddyCheckinCandidates(ctx context.Context, limit int) ([]CodeBuddyCheckinCandidate, error)
	CheckinAll(ctx context.Context) (*CodeBuddyCheckinBatchResponse, error)
}

// CodeBuddyCheckinScheduler 按配置的时间段自动签到 CodeBuddy 账号。
type CodeBuddyCheckinScheduler struct {
	runner         codeBuddyCheckinRunner
	settingService *SettingService
	clock          func() time.Time
	batchSize      int

	mu      sync.Mutex
	cron    *cron.Cron
	started bool
	stopped bool

	// lastRunDate 记录"已执行过的当地日期"。用当地日期而非 UTC：UTC+8 的早上
	// 在 UTC 下还属于前一天，按 UTC 记会让窗口内的同一天跨 UTC 日重复触发。
	lastRunDate string
}

// NewCodeBuddyCheckinScheduler 构造签到调度器。Start 由 wire 调用一次。
func NewCodeBuddyCheckinScheduler(
	runner codeBuddyCheckinRunner,
	settingService *SettingService,
) *CodeBuddyCheckinScheduler {
	return &CodeBuddyCheckinScheduler{
		runner:         runner,
		settingService: settingService,
		clock:          time.Now,
		batchSize:      codeBuddyCheckinBatchSize,
	}
}

// Start 启动每分钟 tick。重复调用安全（第二次直接返回）。
func (s *CodeBuddyCheckinScheduler) Start() {
	if s == nil || s.runner == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true

	c := cron.New(cron.WithParser(codeBuddyCheckinCronParser))
	if _, err := c.AddFunc(codeBuddyCheckinTickSpec, func() { s.tick() }); err != nil {
		slog.Error("codebuddy_checkin.schedule_failed", "error", err)
		return
	}
	s.cron = c
	c.Start()
	slog.Info("codebuddy_checkin.scheduler_started", "spec", codeBuddyCheckinTickSpec)
}

// Stop 停止调度（进程退出时调用）。
func (s *CodeBuddyCheckinScheduler) Stop() {
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

// tick 单次判定 + 执行。测试直接调用它，不必等真实时钟。
func (s *CodeBuddyCheckinScheduler) tick() {
	if s == nil || s.runner == nil || s.settingService == nil {
		return
	}
	now := s.clock()

	settings, err := s.settingService.GetAllSettings(context.Background())
	if err != nil {
		slog.Warn("codebuddy_checkin.settings_read_failed", "error", err)
		return
	}
	features := settings.PlatformFeatures

	// 1) 开关（默认关闭）。没开启时直接返回，连窗口都不看——这是"默认关闭"
	//    真正关闭的证据点：未配置时不会发出任何上游请求。
	if !PlatformFeatureEnabled(features, PlatformCodeBuddy, CodeBuddyCheckinFeatureKey) {
		return
	}

	start, end, location, ok := ResolvePlatformFeatureTimeRange(
		features, PlatformCodeBuddy, CodeBuddyCheckinFeatureKey,
	)
	if !ok {
		return
	}

	// 2) 窗口
	if !WithinTimeRange(now, start, end, location) {
		return
	}

	// 3) 当日已执行？
	localDate := PlatformFeatureLocalDate(now, location).Format(time.DateOnly)
	s.mu.Lock()
	if s.lastRunDate == localDate {
		s.mu.Unlock()
		return
	}
	// 乐观占位：先记日期再执行，避免单轮执行超过 tick 间隔时被下一轮重入。
	// 执行失败不回滚——当天的重试交给下一分钟的下一次 tick 没有意义（上游拒绝
	// 的原因不会因一分钟而改变），而回滚会让"持续失败"变成"每分钟都打上游"。
	s.lastRunDate = localDate
	s.mu.Unlock()

	s.runOnce(localDate, start, end)
}

// runOnce 对当轮账号批量签到并汇总。
//
// 走 CheckinAll（与手动批量端点同一条实现），保证"窗口内自动签"与"管理端点手动签"
// 的并发上限、四态口径、跳过判定完全一致——两条路径各写一份必然会漂移。
func (s *CodeBuddyCheckinScheduler) runOnce(localDate string, start, end TimeOfDay) {
	ctx, cancel := context.WithTimeout(context.Background(), codeBuddyCheckinRunTimeout)
	defer cancel()

	summary, err := s.runner.CheckinAll(ctx)
	if err != nil {
		slog.Warn("codebuddy_checkin.run_failed", "error", err)
		return
	}
	if summary == nil {
		return
	}

	slog.Info("codebuddy_checkin.window_run",
		"local_date", localDate,
		"window", start.Format()+"-"+end.Format(),
		"total", summary.Total,
		"succeeded", summary.Succeeded,
		"already", summary.AlreadyCheckedIn,
		"failed", summary.Failed,
		"skipped", summary.Skipped,
	)
}
