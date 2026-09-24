package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// CodeBuddy token 保活调度（T3，吸收自 workbuddy2api D4 / workbuddy-manager M3）。
//
// ## 形态：照抄 A4/A5/A6 的每分钟 tick + 窗口 + 当日去重
//
// 每分钟 tick 做两件事：
//  1. **expiry 模式（M3）**：只刷「3 天窗口内将过期」的号——NeedsRefresh 是本地
//     判断（读 credentials.expires_at），不产生上游调用；刷完即不再过期，天然幂等。
//  2. **scheduled 模式（D4）**：进入平台功能窗口（默认 22:00–23:00 UTC+8）后，
//     全量刷新一轮，当日去重（乐观占位，失败不回滚——回滚会退化成每分钟打上游）。
//
// 开关：平台功能 `codebuddy_token_keepalive`（默认关闭），窗口经既有
// PlatformFeatureTimeRange 机制配置。manual_disabled 账号照常保活（A9 语义，
// 见 CodeBuddyTokenKeepalive）。

const (
	codeBuddyKeepaliveTickSpec = "* * * * *"

	// CodeBuddyKeepaliveFeatureKey 保活平台功能键（默认关闭，面板开启）。
	CodeBuddyKeepaliveFeatureKey = "token_keepalive"

	// 默认窗口 22:00–23:00 UTC+8（对齐参考实现 token 保活 22 点档）。
	CodeBuddyKeepaliveDefaultStartHour = 22
	CodeBuddyKeepaliveDefaultEndHour   = 23

	codeBuddyKeepaliveRunTimeout = 15 * time.Minute
	codeBuddyKeepaliveBatchSize  = 200
)

// CodeBuddyTokenKeepaliveScheduler 每分钟 tick 的保活调度器。
type CodeBuddyTokenKeepaliveScheduler struct {
	keepalive      *CodeBuddyTokenKeepalive
	settingService *SettingService
	clock          func() time.Time

	mu          sync.Mutex
	cron        *cron.Cron
	started     bool
	stopped     bool
	lastRunDate string
}

func NewCodeBuddyTokenKeepaliveScheduler(
	keepalive *CodeBuddyTokenKeepalive,
	settingService *SettingService,
) *CodeBuddyTokenKeepaliveScheduler {
	return &CodeBuddyTokenKeepaliveScheduler{
		keepalive:      keepalive,
		settingService: settingService,
		clock:          time.Now,
	}
}

// Start 启动每分钟 tick。重复调用安全。
func (s *CodeBuddyTokenKeepaliveScheduler) Start() {
	if s == nil || s.keepalive == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	c := cron.New(cron.WithParser(codeBuddyGrowthCronParser))
	if _, err := c.AddFunc(codeBuddyKeepaliveTickSpec, func() { s.tick() }); err != nil {
		slog.Error("codebuddy_keepalive.schedule_failed", "error", err)
		return
	}
	s.cron = c
	c.Start()
	slog.Info("codebuddy_keepalive.scheduler_started", "spec", codeBuddyKeepaliveTickSpec)
}

func (s *CodeBuddyTokenKeepaliveScheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		s.cron.Stop()
	}
	s.stopped = true
}

func (s *CodeBuddyTokenKeepaliveScheduler) tick() {
	if s == nil || s.keepalive == nil || s.settingService == nil {
		return
	}
	now := s.clock()
	settings, err := s.settingService.GetAllSettings(context.Background())
	if err != nil {
		slog.Warn("codebuddy_keepalive.settings_read_failed", "error", err)
		return
	}
	features := settings.PlatformFeatures
	if !PlatformFeatureEnabled(features, PlatformCodeBuddy, CodeBuddyKeepaliveFeatureKey) {
		return
	}
	inWindow := false
	if start, end, location, ok := ResolvePlatformFeatureTimeRange(
		features, PlatformCodeBuddy, CodeBuddyKeepaliveFeatureKey,
	); ok {
		inWindow = WithinTimeRange(now, start, end, location)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codeBuddyKeepaliveRunTimeout)
	defer cancel()

	// 窗口内：全量刷新（当日去重）。
	if inWindow {
		localDay := now.In(codeBuddyTimeZone).Format(time.DateOnly)
		s.mu.Lock()
		if s.lastRunDate == localDay {
			s.mu.Unlock()
			return
		}
		s.lastRunDate = localDay
		s.mu.Unlock()
		summary, err := s.keepalive.RunKeepAlive(ctx, codeBuddyKeepaliveModeScheduled)
		if err != nil {
			slog.Error("codebuddy_keepalive.scheduled_failed", "error", err)
			return
		}
		slog.Info("codebuddy_keepalive.scheduled_done",
			"total", summary.Total, "refreshed", summary.Refreshed,
			"auth_failed", summary.AuthFailed, "disabled", len(summary.Disabled))
		return
	}
	// 窗口外：expiry 模式（M3）——只刷将过期号，本地判断为主，几乎零上游调用。
	summary, err := s.keepalive.RunKeepAlive(ctx, codeBuddyKeepaliveModeExpiry)
	if err != nil {
		slog.Warn("codebuddy_keepalive.expiry_failed", "error", err)
		return
	}
	if summary.Refreshed > 0 {
		slog.Info("codebuddy_keepalive.expiry_refreshed", "refreshed", summary.Refreshed)
	}
}
