package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"golang.org/x/sync/singleflight"
)

// 渠道监控「配额模式」的配额抓取器。
//
// 不直接对接上游，而是把账号侧现成的用量服务归一成 domain.MonitorQuotaSnapshot：
//   - 海外 5 家（anthropic/openai/gemini/antigravity/grok）→ AccountUsageService.GetUsageForAccount
//   - 国产 coding plan（kimi/zhipu/deepseek）→ CNProviderQuotaService.QueryUsageForAccount
//   - 国产 payg（kimi/deepseek）→ CNProviderBalanceService.QueryBalanceForAccount
//     （zhipu payg 无公开余额端点，探测会返回该错误，原样透出）
// 数据源统一接受已加载的 *Account：fetchUncached 路由前 GetByID 一次并传下去，
// 下游服务不再各自重载（每次 GetByID 含 proxies/groups 联查）。
//
// Fetch 永不返回 error：所有失败都降级为 Success=false 的快照照常入库，
// 由 deriveQuotaCheckResult 推导为 failed/error 状态。
//
// 多个监控可能关联同一账号，而 interval 最小 15s 且国产配额服务自身无缓存，
// 所以快照统一带 TTL 缓存（成功 monitorQuotaFetchCacheTTL、失败
// monitorQuotaErrorCacheTTL 负缓存），防止打爆上游配额端点；同账号的并发
// 抓取由 singleflight 合并为一次上游查询。

// monitorUsageSource 海外平台账号用量查询（AccountUsageService 天然满足）。
// 传已加载的 *Account：fetchUncached 只 GetByID 一次，下游不再重复加载。
type monitorUsageSource interface {
	GetUsageForAccount(ctx context.Context, account *Account, force ...bool) (*UsageInfo, error)
}

// monitorCNQuotaSource 国产 coding plan 滚动窗口额度探测（CNProviderQuotaService 天然满足）。
type monitorCNQuotaSource interface {
	QueryUsageForAccount(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error)
}

// monitorCNBalanceSource 国产 payg 余额探测（CNProviderBalanceService 天然满足）。
type monitorCNBalanceSource interface {
	QueryBalanceForAccount(ctx context.Context, account *Account) (*CNProviderBalanceResult, error)
}

// monitorAccountSource 账号加载（AccountRepository 天然满足）。
type monitorAccountSource interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// monitorZhipuLoginSource 登录态智谱账号的管理面探针（design M4 第四数据源；
// ZhipuAccountMonitorService 的 credit-usage 部分天然满足）。
//
// TODO(票 12): design M4 规定本接口另有第二个方法
// FetchResetStatusForAccount(ctx, account) (*domain.MonitorResetCardStatus, error)，
// 由票 12 实现后并入此处；本票只消费 credit-usage，ResetCards 保持缺省。
type monitorZhipuLoginSource interface {
	FetchUsageDetailForAccount(ctx context.Context, account *Account, start, end time.Time) ([]domain.MonitorQuotaModelCredit, error)
}

// monitorSignReconcileSnapshotSink 是 L2 对账结果并入快照的口（票 27 接线）：
// *ZhipuAccountMonitorService 天然满足。经 monitorZhipuLoginSource 动态下探获取，
// 测试 fake 不实现该方法也不受影响。
type monitorSignReconcileSnapshotSink interface {
	ApplySignReconcileSnapshot(snapshot *domain.MonitorQuotaSnapshot)
}

// zhipuMonitorCreditUsageDays 是监控快照请求的积分明细窗口长度：含当日的近 7 个
// 自然日（与前端面板口径一致；ZhipuAccountMonitorService 会归一为 +8 自然日窗口）。
const zhipuMonitorCreditUsageDays = 7

// ChannelMonitorQuotaFetcher 配额抓取器（成功/失败快照均带 TTL 缓存，
// 同账号并发抓取由 singleflight 合并）。
type ChannelMonitorQuotaFetcher struct {
	usage     monitorUsageSource
	cnQuota   monitorCNQuotaSource
	cnBalance monitorCNBalanceSource
	accounts  monitorAccountSource
	// zhipuLogin 登录态智谱账号的第四数据源（design M4，票 11）。可为 nil：
	// 未接线时 CN 快照只带 NeedsRelogin，不产生积分明细（回滚即回到三源现状）。
	zhipuLogin monitorZhipuLoginSource
	// balanceThreshold cn_balance 余额告警阈值（与账号停调共用配置，见 monitorBalanceThreshold）。
	balanceThreshold float64

	mu     sync.Mutex
	cache  map[int64]monitorQuotaCacheEntry
	flight singleflight.Group
}

type monitorQuotaCacheEntry struct {
	snapshot *domain.MonitorQuotaSnapshot
	expiry   time.Time
}

// NewChannelMonitorQuotaFetcher 构造配额抓取器。
// 参数取具体服务类型以便 wire 直连；单元测试在同包内用 struct 字面量注入 stub。
// zhipuMonitor 为第四数据源（design M4 票 11 接线）：nil 时登录态账号的快照只带
// NeedsRelogin，不产生积分明细与 L2 对账字段（回滚即回到三源现状）。
func NewChannelMonitorQuotaFetcher(
	usage *AccountUsageService,
	cnQuota *CNProviderQuotaService,
	cnBalance *CNProviderBalanceService,
	accounts AccountRepository,
	cfg *config.Config,
	zhipuMonitor *ZhipuAccountMonitorService,
) *ChannelMonitorQuotaFetcher {
	f := &ChannelMonitorQuotaFetcher{
		cache:            make(map[int64]monitorQuotaCacheEntry),
		balanceThreshold: monitorBalanceThreshold(cfg),
	}
	if usage != nil {
		f.usage = usage
	}
	if cnQuota != nil {
		f.cnQuota = cnQuota
	}
	if cnBalance != nil {
		f.cnBalance = cnBalance
	}
	if accounts != nil {
		f.accounts = accounts
	}
	if zhipuMonitor != nil {
		f.zhipuLogin = zhipuMonitor
	}
	return f
}

// monitorBalanceThreshold 余额告警阈值，与账号停调（CNProviderBalanceCheckService）
// 共用 gateway.cn_providers.balance_threshold，保证监控 degraded 与调度器停调
// 口径一致（任一币种达标即健康）。未配置/非正值时回退 viper 默认 0.5（config.go），
// 避免 0 阈值下「余额=0 也不告警」相对旧 `<=0` 判定的回归。
func monitorBalanceThreshold(cfg *config.Config) float64 {
	if cfg != nil && cfg.Gateway.CNProviders.BalanceThreshold > 0 {
		return cfg.Gateway.CNProviders.BalanceThreshold
	}
	return 0.5
}

// LoadAccount 加载账号（不走缓存）。供 Create/Update 时校验
// provider 与 account.platform 一致；账号不存在时返回错误。
func (f *ChannelMonitorQuotaFetcher) LoadAccount(ctx context.Context, id int64) (*Account, error) {
	if f == nil || f.accounts == nil {
		return nil, fmt.Errorf("quota fetcher is not configured")
	}
	return f.accounts.GetByID(ctx, id)
}

// Fetch 抓取账号的最新配额快照。永不返回 error：失败降级为
// Success=false 快照（Error 带摘要），保证检测历史的时间线连续。
func (f *ChannelMonitorQuotaFetcher) Fetch(ctx context.Context, accountID int64) *domain.MonitorQuotaSnapshot {
	if f == nil {
		// fail-closed：fetcher 未注入（存量测试构造）时不 panic，降级为错误快照。
		return quotaErrorSnapshot("usage", "quota fetcher is not configured", time.Now())
	}

	now := time.Now()

	if cached, ok := f.cachedSnapshot(accountID, now); ok {
		return cached
	}

	// singleflight 合并同账号并发抓取；脱离调用方 ctx（仿 CN 配额服务），
	// 避免某个监控的取消波及共享同一账号的其他监控。
	key := "monitor-quota:" + strconv.FormatInt(accountID, 10)
	ch := f.flight.DoChan(key, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.Background(), monitorQuotaFetchTimeout)
		defer cancel()
		snapshot := f.fetchUncached(fetchCtx, accountID, time.Now())
		// 失败也进短 TTL 负缓存：凭据失效/故障期间不必每次调度都打上游。
		ttl := monitorQuotaFetchCacheTTL
		if !snapshot.Success {
			ttl = monitorQuotaErrorCacheTTL
		}
		f.storeSnapshot(accountID, snapshot, time.Now().Add(ttl))
		return snapshot, nil
	})
	select {
	case <-ctx.Done():
		return quotaErrorSnapshot("usage", "context canceled", now)
	case res := <-ch:
		snapshot, ok := res.Val.(*domain.MonitorQuotaSnapshot)
		if res.Err != nil || !ok || snapshot == nil {
			return quotaErrorSnapshot("usage", "quota fetch failed", now)
		}
		return snapshot
	}
}

func (f *ChannelMonitorQuotaFetcher) cachedSnapshot(accountID int64, now time.Time) (*domain.MonitorQuotaSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.cache[accountID]
	if !ok || now.After(entry.expiry) {
		return nil, false
	}
	return entry.snapshot, true
}

func (f *ChannelMonitorQuotaFetcher) storeSnapshot(accountID int64, snapshot *domain.MonitorQuotaSnapshot, expiry time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[accountID] = monitorQuotaCacheEntry{snapshot: snapshot, expiry: expiry}
}

func (f *ChannelMonitorQuotaFetcher) fetchUncached(ctx context.Context, accountID int64, now time.Time) *domain.MonitorQuotaSnapshot {
	if f == nil {
		return quotaErrorSnapshot("usage", "quota fetcher is not configured", now)
	}

	account, err := f.LoadAccount(ctx, accountID)
	if err != nil || account == nil {
		// FK ON DELETE SET NULL 后 account_id 可能为空/失效；显式报「账号未关联」，
		// 推导为 degraded（配置问题，不是渠道故障）。
		slog.Warn("channel_monitor: load linked account failed",
			"account_id", accountID, "error", err)
		return quotaErrorSnapshot("usage", "linked account not found", now)
	}

	// 账号只在路由前加载这一次；已加载的 account 直接传给数据源
	// （GetUsageForAccount / QueryUsageForAccount / QueryBalanceForAccount），
	// 下游服务不再各自 GetByID（每次含 proxies/groups 联查）。
	switch account.Platform {
	case domain.PlatformKimi, domain.PlatformZhipu, domain.PlatformDeepseek:
		var snapshot *domain.MonitorQuotaSnapshot
		if account.IsCodingPlan() {
			snapshot = f.fetchCNQuota(ctx, account, now)
		} else {
			snapshot = f.fetchCNBalance(ctx, account, now)
		}
		// 登录态智谱账号：在既有 CN 结果上并入 M4 附加字段（设计第四数据源）。
		// 失败只缺字段，Success/状态判定仍由上面的 CN 配额结果决定。
		f.appendZhipuLoginFields(ctx, account, snapshot, now)
		return snapshot
	default:
		return f.fetchUsage(ctx, account, now)
	}
}

// fetchUsage 海外平台：AccountUsageService.GetUsageForAccount → 快照。
func (f *ChannelMonitorQuotaFetcher) fetchUsage(ctx context.Context, account *Account, now time.Time) *domain.MonitorQuotaSnapshot {
	if f.usage == nil {
		return quotaErrorSnapshot("usage", "usage service is not configured", now)
	}
	usage, err := f.usage.GetUsageForAccount(ctx, account)
	if err != nil {
		msg := truncateMessage(sanitizeErrorMessage(err.Error()))
		return &domain.MonitorQuotaSnapshot{
			Source:            "usage",
			Success:           false,
			CredentialInvalid: isCredentialErrorMessage(msg),
			Error:             msg,
			FetchedAt:         now,
		}
	}
	if usage == nil {
		return quotaErrorSnapshot("usage", "usage service returned no data", now)
	}
	// openai/gemini/antigravity/grok 的失败多走「值通道」（err==nil 但错误
	// 降级在 UsageInfo 字段里），必须显式识别，否则会被误判为 operational。
	if failed, credInvalid, msg := usageFailureInfo(usage); failed {
		return &domain.MonitorQuotaSnapshot{
			Source:            "usage",
			Success:           false,
			CredentialInvalid: credInvalid,
			Error:             truncateMessage(sanitizeErrorMessage(msg)),
			FetchedAt:         now,
		}
	}
	snapshot := &domain.MonitorQuotaSnapshot{
		Source:    "usage",
		Success:   true,
		PlanLevel: usage.SubscriptionTier,
		Tiers:     usageQuotaTiers(usage),
		FetchedAt: now,
	}
	if snapshot.PlanLevel == "" {
		snapshot.PlanLevel = usage.SubscriptionTierRaw
	}
	return snapshot
}

// usageQuotaTiers 把 UsageInfo 的各平台窗口归一为 tier 列表（无数据的窗口跳过）。
func usageQuotaTiers(usage *UsageInfo) []domain.MonitorQuotaTier {
	if usage == nil {
		return nil
	}
	tiers := make([]domain.MonitorQuotaTier, 0, 8)
	appendProgressTier(&tiers, "5h", "", usage.FiveHour)
	appendProgressTier(&tiers, "7d", "", usage.SevenDay)
	appendProgressTier(&tiers, "7d-sonnet", "", usage.SevenDaySonnet)
	appendProgressTier(&tiers, "7d-fable", "", usage.SevenDayFable)
	appendProgressTier(&tiers, "30d", "", usage.ThirtyDay)
	// Gemini 多档日配额：同 Window 不同 Label。
	appendProgressTier(&tiers, "daily", "shared", usage.GeminiSharedDaily)
	appendProgressTier(&tiers, "daily", "pro", usage.GeminiProDaily)
	appendProgressTier(&tiers, "daily", "flash", usage.GeminiFlashDaily)
	// Grok requests/tokens 两个日窗口 + 月度计费窗口。
	appendQuotaWindowTier(&tiers, "daily", "requests", usage.GrokRequestQuota)
	appendQuotaWindowTier(&tiers, "daily", "tokens", usage.GrokTokenQuota)
	// Antigravity per-model 总量额度，Label = 模型名（按名排序保证输出稳定）。
	for _, model := range sortedQuotaModelNames(usage.AntigravityQuota) {
		q := usage.AntigravityQuota[model]
		if q == nil {
			continue
		}
		tiers = append(tiers, domain.MonitorQuotaTier{
			Window:      "total",
			Label:       model,
			UsedPercent: float64(q.Utilization),
			ResetAt:     q.ResetTime,
		})
	}
	if len(tiers) == 0 {
		return nil
	}
	return tiers
}

func appendProgressTier(tiers *[]domain.MonitorQuotaTier, window, label string, p *UsageProgress) {
	if p == nil {
		return
	}
	tier := domain.MonitorQuotaTier{
		Window:      window,
		Label:       label,
		UsedPercent: p.Utilization,
	}
	if p.ResetsAt != nil {
		tier.ResetAt = p.ResetsAt.UTC().Format(time.RFC3339)
	}
	if p.LimitRequests > 0 {
		tier.Used = float64(p.UsedRequests)
		tier.Limit = float64(p.LimitRequests)
	}
	*tiers = append(*tiers, tier)
}

func appendQuotaWindowTier(tiers *[]domain.MonitorQuotaTier, window, label string, q *xai.QuotaWindow) {
	if q == nil || q.Limit == nil || *q.Limit <= 0 {
		return
	}
	used := *q.Limit
	if q.Remaining != nil {
		used = *q.Limit - *q.Remaining
		if used < 0 {
			used = 0
		}
	}
	tier := domain.MonitorQuotaTier{
		Window:      window,
		Label:       label,
		Used:        float64(used),
		Limit:       float64(*q.Limit),
		UsedPercent: float64(used) / float64(*q.Limit) * 100,
	}
	if q.ResetAt != "" {
		tier.ResetAt = q.ResetAt
	} else if q.ResetUnix != nil && *q.ResetUnix > 0 {
		tier.ResetAt = time.Unix(*q.ResetUnix, 0).UTC().Format(time.RFC3339)
	}
	*tiers = append(*tiers, tier)
}

func sortedQuotaModelNames(quotas map[string]*AntigravityModelQuota) []string {
	names := make([]string, 0, len(quotas))
	for name := range quotas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// appendZhipuLoginFields 把登录态智谱账号（design M4）的附加字段就地并入 CN 快照：
//   - NeedsRelogin 读 accounts.extra 的 zhipu_needs_relogin（票 09 契约，无上游调用）；
//   - ModelCredits 取近 zhipuMonitorCreditUsageDays 个自然日的逐模型积分明细（第四数据源）。
//
// 新字段一期不参与状态判定：任何失败都只让对应字段缺失，绝不修改
// Success/Error/CredentialInvalid（避免新数据源抖动把健康渠道误报为 degraded）。
func (f *ChannelMonitorQuotaFetcher) appendZhipuLoginFields(ctx context.Context, account *Account, snapshot *domain.MonitorQuotaSnapshot, now time.Time) {
	if f == nil || snapshot == nil || account == nil || !account.IsZhipuLoginManaged() {
		return
	}
	snapshot.NeedsRelogin = zhipuNeedsRelogin(account)
	if f.zhipuLogin == nil {
		// 未接线（或回滚）时静默跳过：快照形状与三源现状一致。
		return
	}
	// L2 对账结果并入快照（票 27 接线）：与积分明细抓取独立，抓取失败不影响
	// 对账字段的展示；fake 不实现 sink 口时同样静默跳过。
	if sink, ok := f.zhipuLogin.(monitorSignReconcileSnapshotSink); ok {
		sink.ApplySignReconcileSnapshot(snapshot)
	}
	start := now.AddDate(0, 0, -(zhipuMonitorCreditUsageDays - 1))
	credits, err := f.zhipuLogin.FetchUsageDetailForAccount(ctx, account, start, now)
	if err != nil {
		slog.Debug("channel_monitor: zhipu credit usage probe failed",
			"account_id", account.ID, "error", err)
		return
	}
	if len(credits) > 0 {
		snapshot.ModelCredits = credits
	}
}

// zhipuNeedsRelogin 读 accounts.extra 的重登标记（票 09 写入契约：布尔 true；
// 缺键/非布尔/非 true 一律视为 false，老账号行天然兼容）。
func zhipuNeedsRelogin(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	needs, _ := account.Extra[ZhipuNeedsReloginExtraKey].(bool)
	return needs
}

// fetchCNQuota 国产 coding plan：CNProviderQuotaService.QueryUsageForAccount → 快照。
func (f *ChannelMonitorQuotaFetcher) fetchCNQuota(ctx context.Context, account *Account, now time.Time) *domain.MonitorQuotaSnapshot {
	if f.cnQuota == nil {
		return quotaErrorSnapshot("cn_quota", "cn quota service is not configured", now)
	}
	result, err := f.cnQuota.QueryUsageForAccount(ctx, account)
	if err != nil {
		msg := truncateMessage(sanitizeErrorMessage(err.Error()))
		return &domain.MonitorQuotaSnapshot{
			Source:            "cn_quota",
			Success:           false,
			CredentialInvalid: isCredentialErrorMessage(msg),
			Error:             msg,
			FetchedAt:         now,
		}
	}
	snapshot := &domain.MonitorQuotaSnapshot{
		Source:    "cn_quota",
		Success:   result.Success,
		PlanLevel: result.PlanLevel,
		Error:     result.Error,
		FetchedAt: now,
	}
	// 只有 401/403 判凭据失效（与 fetchCNBalance 口径一致）：CN quota 服务的
	// CredentialValid 仅在成功路径置 true，若按 `!Success && !CredentialValid`
	// 推导，500/429/智谱业务错误全会被误判为 failed。
	if !result.Success && (result.StatusCode == 401 || result.StatusCode == 403) {
		snapshot.CredentialInvalid = true
	}
	if len(result.Tiers) > 0 {
		snapshot.Tiers = make([]domain.MonitorQuotaTier, 0, len(result.Tiers))
		for _, t := range result.Tiers {
			snapshot.Tiers = append(snapshot.Tiers, domain.MonitorQuotaTier{
				Window:      t.Window,
				UsedPercent: t.UsedPercent,
				ResetAt:     t.ResetAt,
			})
		}
	}
	if !snapshot.Success {
		snapshot.Error = firstNonEmpty(snapshot.Error, "cn quota probe failed")
	}
	return snapshot
}

// fetchCNBalance 国产 payg：CNProviderBalanceService.QueryBalanceForAccount → 快照。
func (f *ChannelMonitorQuotaFetcher) fetchCNBalance(ctx context.Context, account *Account, now time.Time) *domain.MonitorQuotaSnapshot {
	if f.cnBalance == nil {
		return quotaErrorSnapshot("cn_balance", "cn balance service is not configured", now)
	}
	result, err := f.cnBalance.QueryBalanceForAccount(ctx, account)
	if err != nil {
		msg := truncateMessage(sanitizeErrorMessage(err.Error()))
		return &domain.MonitorQuotaSnapshot{
			Source:            "cn_balance",
			Success:           false,
			CredentialInvalid: isCredentialErrorMessage(msg),
			Error:             msg,
			FetchedAt:         now,
		}
	}
	snapshot := &domain.MonitorQuotaSnapshot{
		Source:    "cn_balance",
		Success:   result.Success,
		Currency:  result.Currency,
		Error:     result.Error,
		FetchedAt: now,
	}
	if result.Success {
		balance := result.Balance
		snapshot.Balance = &balance
		// 与账号停调（checkOne）同口径：上游标记不可用或全部币种低于阈值
		// 才告警，任一币种达标即健康（余额 5 元/阈值 10 元的账号调度器已
		// 停调，监控不能仍绿灯）。
		snapshot.BalanceLow = !result.Available || allCNBalancesBelowThreshold(result, f.balanceThreshold)
	} else if result.StatusCode == 401 || result.StatusCode == 403 {
		snapshot.CredentialInvalid = true
	}
	if len(result.Balances) > 0 {
		snapshot.Balances = make([]domain.MonitorBalance, 0, len(result.Balances))
		for _, b := range result.Balances {
			snapshot.Balances = append(snapshot.Balances, domain.MonitorBalance{
				Currency: b.Currency,
				Balance:  b.Balance,
			})
		}
	}
	if !snapshot.Success {
		snapshot.Error = firstNonEmpty(snapshot.Error, "cn balance probe failed")
	}
	return snapshot
}

// quotaErrorSnapshot 构造统一错误快照。
func quotaErrorSnapshot(source, message string, now time.Time) *domain.MonitorQuotaSnapshot {
	return &domain.MonitorQuotaSnapshot{
		Source:    source,
		Success:   false,
		Error:     truncateMessage(sanitizeErrorMessage(message)),
		FetchedAt: now,
	}
}

// isCredentialErrorMessage 上游 401/403 鉴权失败的启发式识别
// （海外 GetUsage 的错误没有结构化状态码，只能看文本）。
func isCredentialErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "invalid_api_key") ||
		strings.Contains(msg, "authentication")
}

// usageFailureInfo 识别 GetUsage 经「值通道」返回的失败：antigravity/grok
// 等平台 err==nil 但把错误降级在 UsageInfo 字段里（Error/ErrorCode/状态标记）。
// 返回 failed=false 表示可用；credentialInvalid 表示凭据失效（401/403 语义，
// 推导为 failed 状态）；msg 为失败摘要。
//
// grok 的 ErrorCode=quota_unknown 是「尚未观测到计费快照/限流头」的已知未知态，
// 不是失败（严格按 ErrorCode 判会把健康 grok 账号永久判 error），显式豁免。
func usageFailureInfo(usage *UsageInfo) (failed, credentialInvalid bool, msg string) {
	if usage == nil {
		return false, false, ""
	}
	if usage.ErrorCode == "quota_unknown" {
		return false, false, ""
	}
	failed = usage.Error != "" || usage.NeedsReauth || usage.IsBanned ||
		usage.IsForbidden || usage.ErrorCode != ""
	if !failed {
		return false, false, ""
	}
	credentialInvalid = usage.NeedsReauth || usage.IsBanned || usage.IsForbidden ||
		usage.ErrorCode == errorCodeUnauthenticated || usage.ErrorCode == errorCodeForbidden
	msg = firstNonEmpty(usage.Error, usage.ForbiddenReason, usage.ErrorCode, "usage fetch failed")
	return failed, credentialInvalid, msg
}

// deriveQuotaCheckResult 把配额快照推导为检测状态（复用既有 status 枚举，
// 时间线/可用率机制自动生效）：
//   - 查询成功且无告警        → operational
//   - 任一窗口使用率 >= 阈值或余额低于阈值/不可用 → degraded
//   - 账号未关联（配置问题）    → degraded
//   - 凭据失效（401/403）     → failed
//   - 网络/解析等其他错误      → error
//
// M4 新字段（ModelCredits/ResetCards/NeedsRelogin）一期不参与判定：
// 状态仍只由上面的 Success/Tiers/BalanceLow/CredentialInvalid 推导。
func deriveQuotaCheckResult(snapshot *domain.MonitorQuotaSnapshot, model string, checkedAt time.Time) *CheckResult {
	res := &CheckResult{Model: model, CheckedAt: checkedAt}
	if snapshot == nil {
		res.Status = MonitorStatusError
		res.Message = "quota snapshot missing"
		return res
	}

	switch {
	case !snapshot.Success && snapshot.CredentialInvalid:
		res.Status = MonitorStatusFailed
		res.Message = snapshot.Error
	case !snapshot.Success && strings.Contains(snapshot.Error, "linked account not found"):
		res.Status = MonitorStatusDegraded
		res.Message = snapshot.Error
	case !snapshot.Success:
		res.Status = MonitorStatusError
		res.Message = snapshot.Error
	default:
		if hint := quotaDegradedHint(snapshot); hint != "" {
			res.Status = MonitorStatusDegraded
			res.Message = hint
		} else {
			res.Status = MonitorStatusOperational
		}
	}
	return res
}

// quotaDegradedHint 生成 degraded 的 message（指出触发告警的窗口/余额）；
// 空串表示无告警。
//
// needs_relogin（登录态失效，M4）一期不参与状态判定：它只在已经存在告警原因时
// 追加提示，绝不单独把快照从 operational 拉成 degraded。
func quotaDegradedHint(snapshot *domain.MonitorQuotaSnapshot) string {
	hint := ""
	for _, tier := range snapshot.Tiers {
		if tier.UsedPercent >= monitorQuotaDegradedUsedPercent {
			name := tier.Window
			if tier.Label != "" {
				name = tier.Label + "/" + tier.Window
			}
			hint = fmt.Sprintf("quota high: %s at %s%%", name, strconv.FormatFloat(tier.UsedPercent, 'f', 1, 64))
			break
		}
	}
	if hint == "" && snapshot.BalanceLow {
		if snapshot.Balance != nil {
			hint = fmt.Sprintf("balance low: %s %s", strconv.FormatFloat(*snapshot.Balance, 'f', -1, 64), firstNonEmpty(snapshot.Currency, "?"))
		} else {
			hint = fmt.Sprintf("balance low (%s)", firstNonEmpty(snapshot.Currency, "?"))
		}
	}
	if hint != "" && snapshot.NeedsRelogin {
		hint += "; zhipu login needs relogin"
	}
	return hint
}
