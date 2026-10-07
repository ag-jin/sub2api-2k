package service

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 智谱签名 L2 费率对账（design M5 费率对账段 / spec 硬性要求 L2 / 票 27）。
//
// 目的：兜住「签名被静默按无签名计费」——上游不报错、L1 计数器全零，但实际积分仍按
// 1.0 渠道系数结算。做法：在 M4 既有周期任务里取 credit-usage 的窗口增量 credits，除以
// 同窗口同 token 的期望积分（ZhipuEffectiveCost，签名渠道 0.67）→ **有效系数**；经
// ZhipuSignAlerts.RecordEffectiveRate 上报为 zhipu_sign_effective_rate 指标（票 25 的
// 内置规则 "> 0.70" 告警），并由 ZhipuAccountMonitorService.ApplySignReconcileSnapshot
// 带进监控快照供前端展示（票 30）。
//
// 周期调度：本文件**不新建 goroutine / ticker**。ZhipuAccountMonitorService.RunDueSignReconcile
// 挂在 M4 既有的 keeper loop 里，每轮调一次；是否到期由本文件的窗口口径决定。
//
// 窗口口径（交接产物）：
//   - 已结算边界：窗口末端 = 本轮时刻 − zhipuSignReconcileSettlementDelay（11 分钟，
//     research §六 的 4–11 分钟取上限），未结算的尾巴留给下一窗口；
//   - 首尾相接：本轮窗口起点 = 上一轮成功窗口的末端，不重叠、不遗漏；
//   - 周期与下限：已结算窗口 < max(sign_reconcile_interval_hours, 1h) 时跳过且**不推进基线**
//     （下一轮用累积后的完整窗口）；窗口 < 1h 单独记 window_too_short 原因码；
//   - 首轮只记基线：credit-usage 是按自然日的累计读数，没有上一读数就算不出增量；
//   - 比值公式：Σ 增量积分 ÷ Σ(逐模型增量 token 的期望积分)。期望积分对窗口逐分钟采样
//     ZhipuEffectiveCost 取平均——窗口可能横跨高峰/闲时边界，公式本身仍只有成本模型一个事实源。
//
// 非责任：不改变平台对终端用户的计费口径（design M5），本文件只做观测。

// 跳过 / 失败原因码（稳定标识，日志与快照用；不含自然语言）。
const (
	// ZhipuSignReconcileReasonNotConfigured 表示对账器缺少依赖（账号来源/数据源/上报口未接线）。
	ZhipuSignReconcileReasonNotConfigured = "not_configured"
	// ZhipuSignReconcileReasonSignDisabled 表示全局签名开关关闭：签名没生效时算出的 ≈1.0
	// 不是失效，报上去只会制造误告警。
	ZhipuSignReconcileReasonSignDisabled = "sign_disabled"
	// ZhipuSignReconcileReasonAccountListError 表示账号列表读取失败。
	ZhipuSignReconcileReasonAccountListError = "account_list_error"
	// ZhipuSignReconcileReasonNoSignedAccounts 表示没有任何「启用签名」的账号。
	ZhipuSignReconcileReasonNoSignedAccounts = "no_signed_accounts"
	// ZhipuSignReconcileReasonNoBaseline 表示首轮只记基线（没有上一读数，算不出增量）。
	ZhipuSignReconcileReasonNoBaseline = "no_baseline"
	// ZhipuSignReconcileReasonWindowTooShort 表示已结算窗口不足 1h 下限。
	ZhipuSignReconcileReasonWindowTooShort = "window_too_short"
	// ZhipuSignReconcileReasonNotDue 表示窗口未达配置周期（sign_reconcile_interval_hours）。
	ZhipuSignReconcileReasonNotDue = "not_due"
	// ZhipuSignReconcileReasonUpstreamError 表示 credit-usage 探测失败（保留上次有效值并标陈旧）。
	ZhipuSignReconcileReasonUpstreamError = "upstream_error"
	// ZhipuSignReconcileReasonNoUsage 表示窗口内没有可用增量（增量为 0 或读数为坏值）。
	ZhipuSignReconcileReasonNoUsage = "no_usage"
	// ZhipuSignReconcileReasonNoExpectedCost 表示期望积分为 0（表外模型，无成本基准）：防除零。
	ZhipuSignReconcileReasonNoExpectedCost = "no_expected_cost"
	// ZhipuSignReconcileReasonInvalidRate 表示算出的系数非有限/非正（防御性兜底：
	// 「绝不产生 NaN/Inf 指标」是硬性要求，上报前最后一道检查）。
	ZhipuSignReconcileReasonInvalidRate = "invalid_rate"
)

const (
	// zhipuSignReconcileDefaultIntervalHours 是周期兜底值（与 #01/#28 的默认 6 同源）。
	zhipuSignReconcileDefaultIntervalHours = 6
	// zhipuSignReconcileMinWindow 是窗口下限：短窗被 4–11 分钟结算延时主导，比值不可用。
	zhipuSignReconcileMinWindow = time.Hour
	// zhipuSignReconcileSettlementDelay 是窗口末端的结算回退上限（research §六：4–11 分钟）。
	zhipuSignReconcileSettlementDelay = 11 * time.Minute
	// zhipuSignReconcileSampleStep 是期望积分的时段系数采样步长（见 zhipuSignReconcileMeanEffectiveCost）。
	zhipuSignReconcileSampleStep = time.Minute
	// zhipuSignReconcileRowKeySeparator 分隔读数键的模型与自然日。
	zhipuSignReconcileRowKeySeparator = "\x00"
)

// zhipuReconcileUsageSource 是 credit-usage 探针接缝（生产 = *ZhipuAccountMonitorService，
// 与 keeper 共用同一 singleflight/TTL 入口，绝不重复打上游）。
type zhipuReconcileUsageSource interface {
	FetchUsageDetailForAccount(ctx context.Context, account *Account, start, end time.Time) ([]domain.MonitorQuotaModelCredit, error)
}

// zhipuReconcileRateSink 是有效系数的上报接缝（生产 = #24/#25 的 *ZhipuSignAlerts；
// 票 25 的评估器从它读取指标值）。实现方必须忽略非有限值（见 RecordEffectiveRate）。
type zhipuReconcileRateSink interface {
	RecordEffectiveRate(ctx context.Context, rate float64)
}

// ZhipuSignReconcileResult 是一轮对账的结果：既是 RunIfDue 的返回值，也是快照展示口径
// （ZhipuAccountMonitorService.ApplySignReconcileSnapshot 读 LastResult）。
type ZhipuSignReconcileResult struct {
	// At 是本轮对账时刻（引擎时钟）。
	At time.Time `json:"at"`
	// WindowStart / WindowEnd 是产出有效系数所依据的已结算窗口。
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// Accounts 是参与本轮对账的启用签名账号数。
	Accounts int `json:"accounts"`
	// ActualCredits 是窗口内实际积分增量合计。
	ActualCredits float64 `json:"actual_credits"`
	// ExpectedCredits 是同窗口同 token 的期望积分合计（签名渠道 0.67）。
	ExpectedCredits float64 `json:"expected_credits"`
	// EffectiveRate = ActualCredits ÷ ExpectedCredits；仅在 Applied=true 时有意义。
	EffectiveRate float64 `json:"effective_rate"`
	// PeakFactor 是本轮对账时刻的时段系数（0.5 闲时 / 1.0 高峰，前端展示口径）。
	PeakFactor float64 `json:"peak_factor"`
	// Threshold 是本轮生效的偏离阈值（sign_reconcile_deviation_threshold，默认 0.70）。
	Threshold float64 `json:"threshold"`
	// Deviation 表示 EffectiveRate 超过生效阈值（票 25 的规则据此告警）。
	Deviation bool `json:"deviation"`
	// Applied 表示本轮产出并上报了有效系数。
	Applied bool `json:"applied"`
	// Stale 表示本轮失败而保留了上一次的有效值（前端不得当作新鲜值展示）。
	Stale bool `json:"stale"`
	// Reason 是本轮的原因码（成功时为空）。
	Reason string `json:"reason"`
}

// zhipuSignReconcileReading 是单个账号一次读取的累计读数（键 = 模型 + 自然日）。
type zhipuSignReconcileReading map[string]domain.MonitorQuotaModelCredit

// zhipuSignReconcileDelta 是单个模型在窗口内的增量。
type zhipuSignReconcileDelta struct {
	model   string
	input   float64
	cached  float64
	output  float64
	credits float64
}

// ZhipuSignReconciler 是 L2 费率对账器：持有基线读数与最近一次可展示结果，全部状态都在
// 进程内存（重启后由下一轮重新建基线），方法对 nil 接收者安全。
type ZhipuSignReconciler struct {
	accounts zhipuSignAccountLister
	usage    zhipuReconcileUsageSource
	rates    zhipuReconcileRateSink
	now      func() time.Time

	// configSource 是 #28 的生效配置面（运行层覆盖 > 部署层）；未接线时用 fallback。
	configSource zhipuSignConfigSource
	fallback     ZhipuSignConfig

	mu sync.Mutex
	// baseline 是上一次对账读到的「逐账号逐模型逐日累计值」：增量 = 本轮读数 − 基线读数。
	baseline map[int64]zhipuSignReconcileReading
	// baselineWindowEnd 是基线读数对应的已结算窗口末端（相邻窗口首尾相接的接缝）。
	baselineWindowEnd time.Time
	hasBaseline       bool
	// display 是最近一次「可展示」的对账状态（快照读它）：成功轮写入新值，
	// 失败轮只把 Stale 置位（保留上次有效值）。
	display    ZhipuSignReconcileResult
	hasDisplay bool
}

// NewZhipuSignReconciler 构造 L2 对账器。accounts 是账号来源（*repository.accountRepository
// 等实现 zhipuSignAccountLister 的类型）；usage 是 credit-usage 探针（生产传本包的
// *ZhipuAccountMonitorService）；rates 是有效系数上报口（生产传 *ZhipuSignAlerts）；
// now 为 nil 时用 time.Now。
func NewZhipuSignReconciler(
	cfg *config.Config,
	accounts zhipuSignAccountLister,
	usage zhipuReconcileUsageSource,
	rates zhipuReconcileRateSink,
	now func() time.Time,
) *ZhipuSignReconciler {
	if now == nil {
		now = time.Now
	}
	return &ZhipuSignReconciler{
		accounts: accounts,
		usage:    usage,
		rates:    rates,
		now:      now,
		fallback: zhipuSignReconcileFallbackConfig(cfg),
	}
}

// SetConfigSource 注入 #28 的生效配置面（装配期调用一次，运行期只读）。传入 nil 等于
// 只用部署层配置。
func (r *ZhipuSignReconciler) SetConfigSource(source zhipuSignConfigSource) {
	if r == nil {
		return
	}
	r.configSource = source
}

// RunIfDue 是对账的周期入口（M4 既有周期任务每轮调一次）。未到期、全局开关关闭或没有
// 启用签名的账号时立即返回；返回值 applied 表示本轮是否产出并上报了有效系数。
func (r *ZhipuSignReconciler) RunIfDue(ctx context.Context) (ZhipuSignReconcileResult, bool) {
	if r == nil {
		return ZhipuSignReconcileResult{Reason: ZhipuSignReconcileReasonNotConfigured}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	effective := r.effectiveConfig(ctx)
	now := r.nowTime()
	result := ZhipuSignReconcileResult{
		At:         now,
		PeakFactor: ZhipuPeakFactor(now),
		Threshold:  effective.SignReconcileDeviationThreshold,
	}
	if !effective.SignV4Enabled {
		return r.skip(result, ZhipuSignReconcileReasonSignDisabled), false
	}
	if r.accounts == nil || r.usage == nil || r.rates == nil {
		return r.skip(result, ZhipuSignReconcileReasonNotConfigured), false
	}

	windowEnd := now.Add(-zhipuSignReconcileSettlementDelay)
	result.WindowStart = windowEnd
	result.WindowEnd = windowEnd

	accounts, err := r.signedAccounts(ctx)
	if err != nil {
		return r.fail(result, ZhipuSignReconcileReasonAccountListError), false
	}
	if len(accounts) == 0 {
		return r.skip(result, ZhipuSignReconcileReasonNoSignedAccounts), false
	}
	result.Accounts = len(accounts)

	baseline, baselineEnd, hasBaseline := r.baselineSnapshot()
	if !hasBaseline {
		return r.captureBaseline(ctx, accounts, windowEnd, result)
	}

	result.WindowStart = baselineEnd
	window := windowEnd.Sub(baselineEnd)
	if window < zhipuSignReconcileMinWindow {
		return r.skip(result, ZhipuSignReconcileReasonWindowTooShort), false
	}
	if window < r.interval(effective) {
		return r.skip(result, ZhipuSignReconcileReasonNotDue), false
	}

	readings, ok := r.readAccounts(ctx, accounts, baselineEnd, windowEnd)
	if !ok {
		return r.fail(result, ZhipuSignReconcileReasonUpstreamError), false
	}
	deltas := zhipuSignReconcileDeltaByModel(baseline, readings)
	actual, expected, rate, reason := zhipuSignReconcileRate(deltas, baselineEnd, windowEnd)
	switch {
	case reason == ZhipuSignReconcileReasonInvalidRate:
		return r.fail(result, reason), false
	case reason != "":
		return r.skip(result, reason), false
	}

	result.ActualCredits = actual
	result.ExpectedCredits = expected
	result.EffectiveRate = rate
	result.Deviation = zhipuSignReconcileDeviation(rate, effective.SignReconcileDeviationThreshold)
	result.Applied = true

	r.rates.RecordEffectiveRate(ctx, rate)
	r.storeBaseline(readings, windowEnd)
	r.publish(result)
	r.logApplied(result)
	return result, true
}

// captureBaseline 是首轮分支：读取一次累计读数并记为基线，本轮不产出系数
// （credit-usage 是自然日累计读数，没有上一读数就算不出增量）。
func (r *ZhipuSignReconciler) captureBaseline(
	ctx context.Context, accounts []Account, windowEnd time.Time, result ZhipuSignReconcileResult,
) (ZhipuSignReconcileResult, bool) {
	readings, ok := r.readAccounts(ctx, accounts, windowEnd, windowEnd)
	if !ok {
		return r.fail(result, ZhipuSignReconcileReasonUpstreamError), false
	}
	r.storeBaseline(readings, windowEnd)
	return r.skip(result, ZhipuSignReconcileReasonNoBaseline), false
}

// LastResult 返回最近一次可展示的对账状态（ok=false 表示从未成功对账过）。失败轮保留
// 上一次的有效值并把 Stale 置位；调用方（快照合并）据此标记「陈旧」。
func (r *ZhipuSignReconciler) LastResult() (ZhipuSignReconcileResult, bool) {
	if r == nil {
		return ZhipuSignReconcileResult{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.hasDisplay {
		return ZhipuSignReconcileResult{}, false
	}
	return r.display, true
}

// signedAccounts 返回参与对账的账号：智谱登录托管 ∧ 账号级 zcode_client_sign=v4 ∧ 有
// access_token。没有凭据的账号发不出签名请求（属 keeper 的判定面），排除它以免永久把
// 整轮对账卡在「上游错误」。
func (r *ZhipuSignReconciler) signedAccounts(ctx context.Context) ([]Account, error) {
	accounts, err := r.accounts.ListByPlatform(ctx, PlatformZhipu)
	if err != nil {
		return nil, err
	}
	enabled := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !zhipuSignAccountEnabled(account) {
			continue
		}
		if strings.TrimSpace(account.GetZhipuAccessToken()) == "" {
			continue
		}
		enabled = append(enabled, *account)
	}
	return enabled, nil
}

// readAccounts 逐账号读取累计读数。任一账号失败即整轮判失败：部分求和会算出错误的比值
// （少算任一账号的积分都会把系数推向 0.67 或 1.0），宁可不报。
func (r *ZhipuSignReconciler) readAccounts(
	ctx context.Context,
	accounts []Account,
	start, end time.Time,
) (map[int64]zhipuSignReconcileReading, bool) {
	readings := make(map[int64]zhipuSignReconcileReading, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		rows, err := r.usage.FetchUsageDetailForAccount(ctx, account, start, end)
		if err != nil {
			logger.L().Warn("zhipu sign reconcile: credit usage probe failed",
				zap.Int64("account_id", account.ID),
				zap.Error(err),
			)
			return nil, false
		}
		reading := make(zhipuSignReconcileReading, len(rows))
		for _, row := range rows {
			reading[zhipuSignReconcileRowKey(row)] = row
		}
		readings[account.ID] = reading
	}
	return readings, true
}

func (r *ZhipuSignReconciler) baselineSnapshot() (map[int64]zhipuSignReconcileReading, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.baseline, r.baselineWindowEnd, r.hasBaseline
}

func (r *ZhipuSignReconciler) storeBaseline(readings map[int64]zhipuSignReconcileReading, windowEnd time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.baseline = readings
	r.baselineWindowEnd = windowEnd
	r.hasBaseline = true
}

func (r *ZhipuSignReconciler) publish(result ZhipuSignReconcileResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.display = result
	r.hasDisplay = true
}

// markStale 把最近一次可展示结果标记为陈旧（保留旧值），返回是否存在旧值。
func (r *ZhipuSignReconciler) markStale() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.hasDisplay {
		return false
	}
	r.display.Stale = true
	return true
}

// skip 记一次「本轮不产出」并返回结果。非失败跳过（未到期、无启用账号、窗口内无数据）
// 不改写展示状态：上一次的有效值仍是已知事实，只有失败（fail）才标陈旧。
func (r *ZhipuSignReconciler) skip(result ZhipuSignReconcileResult, reason string) ZhipuSignReconcileResult {
	result.Reason = reason
	logger.L().Debug("zhipu sign reconcile: skipped",
		zap.String("reason", reason),
		zap.Time("window_start", result.WindowStart),
		zap.Time("window_end", result.WindowEnd),
	)
	return result
}

// fail 记一次失败：保留上次有效值并标记陈旧（绝不写 0 冒充系数）。
func (r *ZhipuSignReconciler) fail(result ZhipuSignReconcileResult, reason string) ZhipuSignReconcileResult {
	result.Reason = reason
	result.Stale = r.markStale()
	logger.L().Warn("zhipu sign reconcile: no effective rate this round",
		zap.String("reason", reason),
		zap.Bool("stale", result.Stale),
		zap.Time("window_start", result.WindowStart),
		zap.Time("window_end", result.WindowEnd),
	)
	return result
}

func (r *ZhipuSignReconciler) logApplied(result ZhipuSignReconcileResult) {
	logger.L().Info("zhipu sign reconcile: effective channel rate updated",
		zap.Float64("effective_rate", result.EffectiveRate),
		zap.Float64("target_rate", ZhipuSignedChannelFactor),
		zap.Float64("actual_credits", result.ActualCredits),
		zap.Float64("expected_credits", result.ExpectedCredits),
		zap.Int("accounts", result.Accounts),
		zap.Time("window_start", result.WindowStart),
		zap.Time("window_end", result.WindowEnd),
	)
	if !result.Deviation {
		return
	}
	logger.L().Warn("zhipu sign reconcile: effective rate deviates from the signed channel factor",
		zap.Float64("effective_rate", result.EffectiveRate),
		zap.Float64("threshold", result.Threshold),
		zap.String("suggested_action", zhipuSignReconcileSuggestedAction()),
	)
}

// effectiveConfig 返回生效签名配置：#28 的配置面可用时读它（运行层覆盖热生效），
// 否则用构造期解析好的部署层兜底值。
func (r *ZhipuSignReconciler) effectiveConfig(ctx context.Context) ZhipuSignConfig {
	if r == nil {
		return zhipuSignProtocolDefaults()
	}
	if r.configSource != nil {
		return r.configSource.Effective(ctx)
	}
	return r.fallback
}

// interval 返回对账周期：配置值（默认 6h）与 1h 下限取大。
func (r *ZhipuSignReconciler) interval(effective ZhipuSignConfig) time.Duration {
	hours := effective.SignReconcileIntervalHours
	if hours <= 0 {
		hours = zhipuSignReconcileDefaultIntervalHours
	}
	interval := time.Duration(hours) * time.Hour
	if interval < zhipuSignReconcileMinWindow {
		interval = zhipuSignReconcileMinWindow
	}
	return interval
}

func (r *ZhipuSignReconciler) nowTime() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

// zhipuSignReconcileFallbackConfig 是配置面（#28）未接线时的兜底生效值：全局开关直读部署层
// （与 #22 的 zhipuClientSignEnabled 同口径——配置段缺失或未显式开启即视为关闭），周期与
// 偏离阈值取协议默认（zhipuSignProtocolDefaults，与 #28 的 defaults 同源，避免两处漂移）。
func zhipuSignReconcileFallbackConfig(cfg *config.Config) ZhipuSignConfig {
	effective := zhipuSignProtocolDefaults()
	effective.SignV4Enabled = false
	if cfg == nil {
		return effective
	}
	zhipu := cfg.Gateway.Zhipu
	effective.SignV4Enabled = zhipu.SignV4Enabled
	if zhipu.SignReconcileIntervalHours >= 1 {
		effective.SignReconcileIntervalHours = zhipu.SignReconcileIntervalHours
	}
	if zhipu.SignReconcileDeviationThreshold > 0 && zhipu.SignReconcileDeviationThreshold <= 1 {
		effective.SignReconcileDeviationThreshold = zhipu.SignReconcileDeviationThreshold
	}
	return effective
}

// zhipuSignReconcileDeviation 判定有效系数是否偏离。比较算子取自 #25 的内置指标注册项
// （">"），避免本文件与告警规则各写一份阈值语义而漂移；注册项缺失时回落到严格大于。
func zhipuSignReconcileDeviation(rate, threshold float64) bool {
	if metric, ok := OpsBuiltinAlertMetricFor(OpsMetricTypeZhipuSignEffectiveRate); ok &&
		strings.TrimSpace(metric.Operator) != "" {
		return compareMetric(rate, metric.Operator, threshold)
	}
	return rate > threshold
}

// zhipuSignReconcileSuggestedAction 返回 #25 注册的建议动作文案（design M3.1(d)：签名告警
// 文案必须给出处置建议）。注册项缺失时回落到等价文案。
func zhipuSignReconcileSuggestedAction() string {
	if metric, ok := OpsBuiltinAlertMetricFor(OpsMetricTypeZhipuSignEffectiveRate); ok &&
		strings.TrimSpace(metric.SuggestedAction) != "" {
		return metric.SuggestedAction
	}
	return "检查 X-Client-Version / 关停全局签名开关（gateway.zhipu.sign_v4_enabled）"
}

func zhipuSignReconcileRowKey(row domain.MonitorQuotaModelCredit) string {
	return row.Model + zhipuSignReconcileRowKeySeparator + row.Date
}

// zhipuSignReconcileDeltaByModel 计算「本轮读数 − 基线读数」的增量并按模型合并。
//
// credit-usage 是按自然日的累计值：增量 = 本轮读数 − 基线读数；本轮出现而基线没有的键
// （新自然日、新模型）按「从零累计」处理。负差（上游重算/回补）与非有限值一律按 0 处理，
// 避免坏数据把比值算成负数或 NaN。
func zhipuSignReconcileDeltaByModel(
	baseline, current map[int64]zhipuSignReconcileReading,
) []zhipuSignReconcileDelta {
	merged := make(map[string]*zhipuSignReconcileDelta, 8)
	for accountID, reading := range current {
		previous := baseline[accountID]
		for key, row := range reading {
			entry, ok := merged[row.Model]
			if !ok {
				entry = &zhipuSignReconcileDelta{model: row.Model}
				merged[row.Model] = entry
			}
			if prev, found := previous[key]; found {
				entry.input += zhipuSignReconcileNonNegative(row.InputTokens - prev.InputTokens)
				entry.cached += zhipuSignReconcileNonNegative(row.CachedTokens - prev.CachedTokens)
				entry.output += zhipuSignReconcileNonNegative(row.OutputTokens - prev.OutputTokens)
				entry.credits += zhipuSignReconcileNonNegative(row.Credits - prev.Credits)
				continue
			}
			entry.input += zhipuSignReconcileNonNegative(row.InputTokens)
			entry.cached += zhipuSignReconcileNonNegative(row.CachedTokens)
			entry.output += zhipuSignReconcileNonNegative(row.OutputTokens)
			entry.credits += zhipuSignReconcileNonNegative(row.Credits)
		}
	}

	models := make([]string, 0, len(merged))
	for model := range merged {
		models = append(models, model)
	}
	sort.Strings(models)
	deltas := make([]zhipuSignReconcileDelta, 0, len(models))
	for _, model := range models {
		deltas = append(deltas, *merged[model])
	}
	return deltas
}

// zhipuSignReconcileAggregate 汇总窗口增量：实际积分合计与期望积分合计。
func zhipuSignReconcileAggregate(deltas []zhipuSignReconcileDelta, start, end time.Time) (actual, expected float64) {
	for _, delta := range deltas {
		actual += delta.credits
		expected += zhipuSignReconcileMeanEffectiveCost(delta, start, end)
	}
	return actual, expected
}

// zhipuSignReconcileRate 汇总窗口增量并算出有效系数。actual/expected 非正或系数
// 非有限/非正时返回原因码（空串表示产出成功）；除 invalid_rate 按失败处理外，
// 其余原因码都属「本轮没有可用数据」的跳过口径。防除零与「绝不产生 NaN/Inf 指标」
// 的硬性检查都在这里。
func zhipuSignReconcileRate(deltas []zhipuSignReconcileDelta, start, end time.Time) (actual, expected, rate float64, reason string) {
	actual, expected = zhipuSignReconcileAggregate(deltas, start, end)
	if !zhipuSignReconcileFinite(actual) || actual <= 0 {
		return 0, 0, 0, ZhipuSignReconcileReasonNoUsage
	}
	if !zhipuSignReconcileFinite(expected) || expected <= 0 {
		return 0, 0, 0, ZhipuSignReconcileReasonNoExpectedCost
	}
	rate = actual / expected
	if !zhipuSignReconcileFinite(rate) || rate <= 0 {
		return 0, 0, 0, ZhipuSignReconcileReasonInvalidRate
	}
	return actual, expected, rate, ""
}

// zhipuSignReconcileMeanEffectiveCost 返回单个模型增量的期望积分：对窗口逐分钟采样
// ZhipuEffectiveCost（签名渠道）取算术平均。窗口按整分钟切分（长窗口的最后不足一分钟的
// 尾巴忽略）——时段系数的边界在整点，分钟级采样的误差可忽略，而公式仍然只有
// ZhipuEffectiveCost 一个事实源。
func zhipuSignReconcileMeanEffectiveCost(delta zhipuSignReconcileDelta, start, end time.Time) float64 {
	span := end.Sub(start)
	if span <= 0 {
		return ZhipuEffectiveCost(delta.model, delta.input, delta.cached, delta.output, end, true)
	}
	samples := int(span / zhipuSignReconcileSampleStep)
	if samples < 1 {
		samples = 1
	}
	total := 0.0
	at := start
	for i := 0; i < samples; i++ {
		total += ZhipuEffectiveCost(delta.model, delta.input, delta.cached, delta.output, at, true)
		at = at.Add(zhipuSignReconcileSampleStep)
	}
	return total / float64(samples)
}

func zhipuSignReconcileFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func zhipuSignReconcileNonNegative(value float64) float64 {
	if !zhipuSignReconcileFinite(value) || value < 0 {
		return 0
	}
	return value
}
