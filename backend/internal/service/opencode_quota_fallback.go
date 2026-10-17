package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	opencodepkg "github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/tidwall/gjson"
)

// OpenCode 额度保护（quota_fallback）：付费额度窗口触顶时，把管理员配置过的付费
// 模型换成手选替代目标，并把该账号的其余付费模型从候选中排除；替代目标直连不受
// 付费钱包冷却影响。绝不写整账号停调（SetRateLimited / TempUnschedulable /
// BlockAccountScheduling），也绝不悄悄回退付费原模型。
//
// 契约：.scratch/opencode-free-quota/contract.md（schema 已冻结）。
// 本文件是唯一仲裁点：配置解析/校验、状态机、单次映射解析、付费钱包冷却写入都
// 集中在此；调度/转发/错误归因/用量/协议路由只调用这里的共享解析，不复制判定。
//
// 范围：仅 platform=opencode / opencode_go、type=apikey、account_mode=go
// （含未设置时的默认 Go）。Zen 模式与其它平台一律零变化。
const (
	// OpenCodeQuotaFallbackCredentialKey 是账号 credentials 中的配置块名。
	OpenCodeQuotaFallbackCredentialKey = "quota_fallback"

	// 状态：enabled=false / 配置缺失 → disabled（行为与基线逐字一致）。
	OpenCodeQuotaFallbackStateDisabled       = "disabled"
	OpenCodeQuotaFallbackStateNormal         = "normal"
	OpenCodeQuotaFallbackStateFallbackActive = "fallback_active"
	OpenCodeQuotaFallbackStateUnknown        = "unknown"

	// 机器可读短码（供 DTO/UI 展示，不是用户文案）。
	OpenCodeQuotaFallbackReasonThresholdReached   = "threshold_reached"
	OpenCodeQuotaFallbackReasonLastKnownTriggered = "last_known_triggered"
	OpenCodeQuotaFallbackReasonNoSnapshot         = "no_snapshot"
	OpenCodeQuotaFallbackReasonSnapshotStale      = "snapshot_stale"
	OpenCodeQuotaFallbackReasonSnapshotInvalid    = "snapshot_invalid"
	OpenCodeQuotaFallbackReasonRefreshFailed      = "refresh_failed"
	OpenCodeQuotaFallbackReasonUnauthorized       = "unauthorized"

	OpenCodeQuotaFallbackDefaultThresholdPercent = 98
	OpenCodeQuotaFallbackMinThresholdPercent     = 1
	OpenCodeQuotaFallbackMaxThresholdPercent     = 100
	OpenCodeQuotaFallbackMaxMappingEntries       = 64
	OpenCodeQuotaFallbackMaxModelIDLength        = 128

	// OpenCodeQuotaFallbackWalletScope 是 paid-wallet 冷却在 extra.model_rate_limits
	// 下的作用域：只挡该账号的付费模型，替代目标与直连免费模型不受影响。作用域名带
	// "opencode:" 前缀，避免与真实上游模型 ID 撞键。
	OpenCodeQuotaFallbackWalletScope = "opencode:paid_wallet"

	// OpenCodeQuotaFallbackWalletReasonPrefix 是新 reason 前缀：既有的按前缀清除器
	// （cn_balance_low / cn_quota_exhausted / cn_concurrency_limit）不会误清它，
	// 阈值停调（TempUnschedState JSON）也不会误判为同一条记录。
	OpenCodeQuotaFallbackWalletReasonPrefix = "opencode_quota_wallet"

	// OpenCodeQuotaFallbackSnapshotStaleAfter 是快照新鲜度上界：超过它仍没有新的
	// ok 快照即视为未知（不假装 0%，也不宣称保护已生效）。
	//
	// 取值依据既有刷新契约，不引入新先例：OpenCodeGoUsageService 默认自动刷新
	// 间隔 15 分钟（opencodeGoUsageDefaultIntervalMinutes）、允许的最小间隔 5 分钟
	// （opencodeGoUsageMinIntervalMinutes，也是 SQL due 过滤的硬地板）；失败刷新按
	// interval×2^n 退避，首次退避即 2×interval。取 2× 默认间隔 = 30 分钟：低于它的
	// 快照视为可用，高于它的（含退避后未成功的）一律 unknown 保守停付费源。
	OpenCodeQuotaFallbackSnapshotStaleAfter = 30 * time.Minute

	openCodeQuotaFallbackWindow5h      = "5h"
	openCodeQuotaFallbackWindowWeekly  = "weekly"
	openCodeQuotaFallbackWindowMonthly = "monthly"
)

// OpenCodeQuotaFallbackConfig 是 credentials.quota_fallback 的规范化形态。
// 零值（Enabled=false）= 功能关闭；ThresholdPercent 缺省为 98。
type OpenCodeQuotaFallbackConfig struct {
	Enabled          bool
	ThresholdPercent int
	ModelMapping     map[string]string
}

func (c OpenCodeQuotaFallbackConfig) thresholdPercent() int {
	if c.ThresholdPercent >= OpenCodeQuotaFallbackMinThresholdPercent &&
		c.ThresholdPercent <= OpenCodeQuotaFallbackMaxThresholdPercent {
		return c.ThresholdPercent
	}
	return OpenCodeQuotaFallbackDefaultThresholdPercent
}

// ParseOpenCodeQuotaFallback 强校验 credentials.quota_fallback 块。
// 保存侧（创建/更新）与读取侧共用同一解析；enabled 缺省或 false 时，映射内容按
// 自由 JSON 容忍（纯 no-op），类型错误仍然报错。enabled=true 时逐条严格校验：
// 至少一条映射、key/value 非空且不相等、不允许链式（target 不得是另一条 source）、
// 不允许通配符、target 不得是通用网关不可用的 gemini-/jev- 模型。
func ParseOpenCodeQuotaFallback(raw map[string]any) (OpenCodeQuotaFallbackConfig, error) {
	config := OpenCodeQuotaFallbackConfig{ThresholdPercent: OpenCodeQuotaFallbackDefaultThresholdPercent}
	if raw == nil {
		return config, nil
	}

	if enabledRaw, ok := raw["enabled"]; ok && enabledRaw != nil {
		enabled, ok := enabledRaw.(bool)
		if !ok {
			return OpenCodeQuotaFallbackConfig{}, fmt.Errorf("quota_fallback.enabled must be a boolean")
		}
		config.Enabled = enabled
	}

	if thresholdRaw, ok := raw["threshold_percent"]; ok && thresholdRaw != nil {
		threshold, ok := openCodeQuotaFallbackIntValue(thresholdRaw)
		if !ok || threshold < OpenCodeQuotaFallbackMinThresholdPercent || threshold > OpenCodeQuotaFallbackMaxThresholdPercent {
			return OpenCodeQuotaFallbackConfig{}, fmt.Errorf(
				"quota_fallback.threshold_percent must be an integer between %d and %d",
				OpenCodeQuotaFallbackMinThresholdPercent, OpenCodeQuotaFallbackMaxThresholdPercent)
		}
		config.ThresholdPercent = threshold
	}

	mappingRaw, hasMapping := raw["model_mapping"]
	if !hasMapping || mappingRaw == nil {
		if config.Enabled {
			return OpenCodeQuotaFallbackConfig{}, fmt.Errorf("quota_fallback.model_mapping must contain at least one entry when enabled")
		}
		return config, nil
	}

	mapping, err := openCodeQuotaFallbackMapping(mappingRaw)
	if err != nil {
		return OpenCodeQuotaFallbackConfig{}, err
	}
	config.ModelMapping = mapping
	if !config.Enabled {
		return config, nil
	}
	if len(mapping) == 0 {
		return OpenCodeQuotaFallbackConfig{}, fmt.Errorf("quota_fallback.model_mapping must contain at least one entry when enabled")
	}
	if len(mapping) > OpenCodeQuotaFallbackMaxMappingEntries {
		return OpenCodeQuotaFallbackConfig{}, fmt.Errorf("quota_fallback.model_mapping supports at most %d entries", OpenCodeQuotaFallbackMaxMappingEntries)
	}
	sources := make([]string, 0, len(mapping))
	for source := range mapping {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		if err := openCodeQuotaFallbackValidateEntry(source, mapping[source], mapping); err != nil {
			return OpenCodeQuotaFallbackConfig{}, err
		}
	}
	return config, nil
}

// openCodeQuotaFallbackValidateEntry 校验单条 source→target（enabled=true 时的严格项）。
func openCodeQuotaFallbackValidateEntry(source, target string, mapping map[string]string) error {
	if source == "" {
		return fmt.Errorf("quota_fallback.model_mapping keys must be non-empty model IDs")
	}
	if target == "" {
		return fmt.Errorf("quota_fallback.model_mapping[%q] must set a non-empty target model ID", source)
	}
	if len(source) > OpenCodeQuotaFallbackMaxModelIDLength || len(target) > OpenCodeQuotaFallbackMaxModelIDLength {
		return fmt.Errorf("quota_fallback.model_mapping[%q] model IDs must be at most %d characters", source, OpenCodeQuotaFallbackMaxModelIDLength)
	}
	if strings.Contains(source, "*") || strings.Contains(target, "*") {
		// 映射只按正常最终上游 ID 精确匹配；通配符会静默永不命中或把上游模型名
		// 原样带 * 发出去，属于配置错误。
		return fmt.Errorf("quota_fallback.model_mapping[%q] must use exact model IDs without wildcards", source)
	}
	if source == target {
		return fmt.Errorf("quota_fallback.model_mapping[%q] target must differ from the source model", source)
	}
	if _, chained := mapping[target]; chained {
		// 只做一层替换：target 再是另一条 source 会让二跳语义不确定。
		return fmt.Errorf("quota_fallback.model_mapping[%q] target %q must not itself be a mapped source", source, target)
	}
	return nil
}

// openCodeQuotaFallbackMapping 解析 model_mapping（string→string record）。
func openCodeQuotaFallbackMapping(raw any) (map[string]string, error) {
	out := make(map[string]string)
	switch values := raw.(type) {
	case map[string]any:
		for key, value := range values {
			target, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("quota_fallback.model_mapping[%q] must be a string model ID", key)
			}
			out[strings.TrimSpace(key)] = strings.TrimSpace(target)
		}
	case map[string]string:
		for key, value := range values {
			out[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	default:
		return nil, fmt.Errorf("quota_fallback.model_mapping must be an object of model ID strings")
	}
	return out, nil
}

// openCodeQuotaFallbackIntValue 读取整数型配置值（JSON 反序列化后为 float64）。
func openCodeQuotaFallbackIntValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case float32:
		if float64(value) != math.Trunc(float64(value)) {
			return 0, false
		}
		return int(value), true
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
			return 0, false
		}
		return int(value), true
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

// GetOpenCodeQuotaFallback 宽松读取账号生效的额度保护配置。
// 仅 OpenCode 平台（opencode / opencode_go）、type=apikey、Go 模式的账号生效；
// 其它账号、缺省或脏数据一律返回零值（= 功能关闭，行为与基线一致），调用方无需
// 再自行判断平台/mode。
func (a *Account) GetOpenCodeQuotaFallback() OpenCodeQuotaFallbackConfig {
	if a == nil || !a.IsOpenCodeGo() || a.Type != AccountTypeAPIKey || !a.IsOpenCodeGoPlan() {
		return OpenCodeQuotaFallbackConfig{}
	}
	if a.Credentials == nil {
		return OpenCodeQuotaFallbackConfig{}
	}
	raw, ok := a.Credentials[OpenCodeQuotaFallbackCredentialKey].(map[string]any)
	if !ok || raw == nil {
		return OpenCodeQuotaFallbackConfig{}
	}
	config, err := ParseOpenCodeQuotaFallback(raw)
	if err != nil {
		// 运行期脏配置绝不执行：按关闭处理并保持旧行为（保存侧已强校验）。
		return OpenCodeQuotaFallbackConfig{}
	}
	return config
}

// OpenCodeQuotaFallbackWindow 是触发判定的窗口证据（DTO 由 Status 结构转换）。
type OpenCodeQuotaFallbackWindow struct {
	Window      string
	UsedPercent float64
	ResetsAt    *time.Time
}

// OpenCodeQuotaFallbackDecision 是 cfg+快照算出的唯一判定结果。
type OpenCodeQuotaFallbackDecision struct {
	State string
	// TriggeringWindows 仅在 fallback_active 非空（窗口顺序固定 5h → weekly → monthly）。
	TriggeringWindows []OpenCodeQuotaFallbackWindow
	// EffectiveMapping 在 fallback_active / unknown 非空，是 cfg.ModelMapping 的
	// 副本。unknown 下它只用于识别管理员手选的直连目标（绝不自动改写付费 source），
	// 不能因证据缺失而丢掉配置。
	EffectiveMapping map[string]string
	// SnapshotAt 是最后一份 ok 证据的 fetched_at（无证据时为 nil）。
	SnapshotAt *time.Time
	Reason     string
}

// EvaluateOpenCodeQuotaFallback 是纯状态机（无 I/O）：
//
//	disabled        enabled=false 或配置缺失，行为零变化。
//	fallback_active 最新 ok 快照任一窗口 percent >= threshold；或最后 ok 证据曾触发
//	               （快照随后失败/陈旧/reset 已过都不误恢复）；或付费钱包冷却生效。
//	normal         新鲜 ok 快照全部窗口 < threshold（唯一恢复路径，且必须是新的成功快照）。
//	unknown        从未有过任何 ok 证据（缺失/从未成功）：既不自动切替代，也不放行
//	               该账号的付费源候选；lastOk 为最后一份 ok 证据（可为 nil，实现复用
//	               现有快照 Data，此时退回到 snap.Data 且仅在 snap.FetchedAt 存在时可信）。
func EvaluateOpenCodeQuotaFallback(cfg OpenCodeQuotaFallbackConfig, snap *OpenCodeGoUsageSnapshot, lastOk *OpenCodeGoUsageData, now time.Time) OpenCodeQuotaFallbackDecision {
	if !cfg.Enabled {
		return OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateDisabled}
	}
	threshold := cfg.thresholdPercent()

	if snap != nil && snap.Status == OpenCodeGoUsageStatusOK && snap.Data != nil && snap.FetchedAt != nil &&
		openCodeQuotaFallbackUsageDataValid(snap.Data) {
		fresh := now.Sub(*snap.FetchedAt) <= OpenCodeQuotaFallbackSnapshotStaleAfter
		windows := openCodeQuotaFallbackWindows(snap.Data)
		if triggering := openCodeQuotaFallbackTriggeringWindows(windows, threshold); len(triggering) > 0 {
			// 新鲜快照触发 = threshold_reached；陈旧快照触发 = 保留的最后 ok 证据触发，
			// 理由必须区分（陈旧证据下不得宣称"当前快照已越阈"）。
			reason := OpenCodeQuotaFallbackReasonLastKnownTriggered
			if fresh {
				reason = OpenCodeQuotaFallbackReasonThresholdReached
			}
			return openCodeQuotaFallbackTriggeredDecision(cfg, triggering, snap.FetchedAt, reason)
		}
		if fresh {
			return OpenCodeQuotaFallbackDecision{
				State:      OpenCodeQuotaFallbackStateNormal,
				SnapshotAt: snap.FetchedAt,
			}
		}
	}

	evidence := lastOk
	if evidence == nil && snap != nil && snap.FetchedAt != nil {
		evidence = snap.Data
	}
	if evidence != nil && openCodeQuotaFallbackUsageDataValid(evidence) {
		windows := openCodeQuotaFallbackWindows(evidence)
		if triggering := openCodeQuotaFallbackTriggeringWindows(windows, threshold); len(triggering) > 0 {
			return openCodeQuotaFallbackTriggeredDecision(cfg, triggering, openCodeQuotaFallbackSnapshotAt(snap), OpenCodeQuotaFallbackReasonLastKnownTriggered)
		}
	}

	return OpenCodeQuotaFallbackDecision{
		State: OpenCodeQuotaFallbackStateUnknown,
		// unknown 只放行 target 直连，不放行付费 source；target 集合必须来自配置，
		// 否则调度把直连目标误判为未配置替代的付费模型（gateway 429/no upstream call）。
		EffectiveMapping: openCodeQuotaFallbackMappingCopy(cfg.ModelMapping),
		SnapshotAt:       openCodeQuotaFallbackSnapshotAt(snap),
		Reason:           openCodeQuotaFallbackUnknownReason(snap),
	}
}

// OpenCodeQuotaFallbackLastKnownOKData 返回快照中保留的最后一份 ok 证据。
// OpenCodeGoUsageService.persistFailure 保留 previous.Data/FetchedAt，所以失败的
// 刷新不会抹掉最后 ok 证据；没有 fetched_at 的数据视为不可信（nil）。
func OpenCodeQuotaFallbackLastKnownOKData(snap *OpenCodeGoUsageSnapshot) *OpenCodeGoUsageData {
	if snap == nil || snap.Data == nil || snap.FetchedAt == nil {
		return nil
	}
	return snap.Data
}

func openCodeQuotaFallbackWindows(data *OpenCodeGoUsageData) []OpenCodeQuotaFallbackWindow {
	if data == nil {
		return nil
	}
	return []OpenCodeQuotaFallbackWindow{
		openCodeQuotaFallbackWindowFromUsage(openCodeQuotaFallbackWindow5h, data.Rolling),
		openCodeQuotaFallbackWindowFromUsage(openCodeQuotaFallbackWindowWeekly, data.Weekly),
		openCodeQuotaFallbackWindowFromUsage(openCodeQuotaFallbackWindowMonthly, data.Monthly),
	}
}

// openCodeQuotaFallbackWindowFromUsage 归一单个窗口：上游显式 rate-limited 时即便
// percent 尚未刷新到 100 也按已用满处理（与 openCodeWindowFromSnapshot 同约定），
// 避免"上游已拒绝、策略还认为低于阈值"而继续放行付费模型。
func openCodeQuotaFallbackWindowFromUsage(name string, window OpenCodeGoUsageWindow) OpenCodeQuotaFallbackWindow {
	percent := window.Percent
	if window.Status == string(opencodepkg.UsageStatusRateLimited) && percent < 100 {
		percent = 100
	}
	return OpenCodeQuotaFallbackWindow{
		Window:      name,
		UsedPercent: percent,
		ResetsAt:    openCodeQuotaFallbackResetPtr(window.ResetsAt),
	}
}

// openCodeQuotaFallbackUsageDataValid 报告三个窗口的观测是否可用于判定：status 必须
// 是已知状态（ok / rate-limited），percent 必须有限且非负。窗口缺失（零值 status）、
// 异常状态或 NaN/Inf 一律视为无效——绝不能被当成"全部窗口低于阈值"，否则会误报
// normal 并放行付费流量。
func openCodeQuotaFallbackUsageDataValid(data *OpenCodeGoUsageData) bool {
	if data == nil {
		return false
	}
	for _, window := range []OpenCodeGoUsageWindow{data.Rolling, data.Weekly, data.Monthly} {
		switch window.Status {
		case string(opencodepkg.UsageStatusOK), string(opencodepkg.UsageStatusRateLimited):
		default:
			return false
		}
		if math.IsNaN(window.Percent) || math.IsInf(window.Percent, 0) || window.Percent < 0 {
			return false
		}
	}
	return true
}

func openCodeQuotaFallbackResetPtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	reset := value
	return &reset
}

// openCodeQuotaFallbackTriggeringWindows 只返回达到阈值的窗口（>= 语义，含浮点边界）。
func openCodeQuotaFallbackTriggeringWindows(windows []OpenCodeQuotaFallbackWindow, threshold int) []OpenCodeQuotaFallbackWindow {
	var triggering []OpenCodeQuotaFallbackWindow
	for _, window := range windows {
		if window.UsedPercent >= float64(threshold) {
			triggering = append(triggering, window)
		}
	}
	return triggering
}

func openCodeQuotaFallbackTriggeredDecision(cfg OpenCodeQuotaFallbackConfig, windows []OpenCodeQuotaFallbackWindow, snapshotAt *time.Time, reason string) OpenCodeQuotaFallbackDecision {
	return OpenCodeQuotaFallbackDecision{
		State:             OpenCodeQuotaFallbackStateFallbackActive,
		TriggeringWindows: windows,
		EffectiveMapping:  openCodeQuotaFallbackMappingCopy(cfg.ModelMapping),
		SnapshotAt:        snapshotAt,
		Reason:            reason,
	}
}

func openCodeQuotaFallbackMappingCopy(mapping map[string]string) map[string]string {
	if len(mapping) == 0 {
		return nil
	}
	out := make(map[string]string, len(mapping))
	for key, value := range mapping {
		out[key] = value
	}
	return out
}

func openCodeQuotaFallbackSnapshotAt(snap *OpenCodeGoUsageSnapshot) *time.Time {
	if snap == nil {
		return nil
	}
	return snap.FetchedAt
}

func openCodeQuotaFallbackUnknownReason(snap *OpenCodeGoUsageSnapshot) string {
	if snap == nil {
		return OpenCodeQuotaFallbackReasonNoSnapshot
	}
	switch snap.Status {
	case OpenCodeGoUsageStatusFailed:
		return OpenCodeQuotaFallbackReasonRefreshFailed
	case OpenCodeGoUsageStatusUnauthorized:
		return OpenCodeQuotaFallbackReasonUnauthorized
	case OpenCodeGoUsageStatusOK:
		if snap.Data == nil {
			return OpenCodeQuotaFallbackReasonNoSnapshot
		}
		if !openCodeQuotaFallbackUsageDataValid(snap.Data) {
			return OpenCodeQuotaFallbackReasonSnapshotInvalid
		}
		return OpenCodeQuotaFallbackReasonSnapshotStale
	}
	return OpenCodeQuotaFallbackReasonNoSnapshot
}

// OpenCodeQuotaFallbackRequestOutcome 是单次请求的判定结果（调度与转发共用）。
type OpenCodeQuotaFallbackRequestOutcome struct {
	// EffectiveUpstreamModel 是允许放行时应当发往上游的模型（未命中映射时=正常上游模型）。
	EffectiveUpstreamModel string
	// DispatchAllowed=false 表示该账号不得服务本次请求（付费模型无替代配置）。
	DispatchAllowed bool
	// MappingApplied 表示出站模型来自管理员手选替代目标。
	MappingApplied bool
}

// ResolveRequest 把判定落到单次请求上（唯一映射入口，绝不做第二跳）：
//   - fallback_active：命中映射的 source → target；target 直连放行；其余付费模型拒选。
//   - unknown：只放行配置里的 target 直连（不自动选 target、也不放行付费 source）。
//   - normal / disabled：原样放行，零变化。
func (d OpenCodeQuotaFallbackDecision) ResolveRequest(normalUpstreamModel string) OpenCodeQuotaFallbackRequestOutcome {
	model := strings.TrimSpace(normalUpstreamModel)
	outcome := OpenCodeQuotaFallbackRequestOutcome{EffectiveUpstreamModel: model, DispatchAllowed: true}

	switch d.State {
	case OpenCodeQuotaFallbackStateFallbackActive:
		if target, ok := d.EffectiveMapping[model]; ok && strings.TrimSpace(target) != "" {
			outcome.EffectiveUpstreamModel = strings.TrimSpace(target)
			outcome.MappingApplied = true
			return outcome
		}
		if d.isReplacementTarget(model) {
			return outcome
		}
		outcome.DispatchAllowed = false
		return outcome
	case OpenCodeQuotaFallbackStateUnknown:
		if d.isReplacementTarget(model) {
			return outcome
		}
		outcome.DispatchAllowed = false
		return outcome
	default:
		return outcome
	}
}

func (d OpenCodeQuotaFallbackDecision) isReplacementTarget(model string) bool {
	if model == "" {
		return false
	}
	for _, target := range d.EffectiveMapping {
		if strings.TrimSpace(target) == model {
			return true
		}
	}
	return false
}

// applyOpenCodeQuotaFallbackWalletEvidence 已移除：判定输入只保留冻结契约的
// cfg + opencode_go_usage_snapshot（429 探测已并入同一快照），钱包冷却只作为
// 付费模型的额外门禁（model_rate_limit.go 的 paid-wallet key），不再参与状态机。

// openCodeQuotaFallbackRequestOutcomeForAccount 是全链唯一判定入口：配置 → 快照 →
// 单次请求结果。调度/转发/协议路由都必须经由此函数取 effective 模型与录用结论，
// 不得各自重复解析。判定输入只有冻结契约里的 cfg+snapshot（last-known-ok 证据）。
func openCodeQuotaFallbackRequestOutcomeForAccount(account *Account, normalUpstreamModel string, now time.Time) OpenCodeQuotaFallbackRequestOutcome {
	outcome, _ := openCodeQuotaFallbackRequestEvaluation(account, normalUpstreamModel, now)
	return outcome
}

// openCodeQuotaFallbackRequestEvaluation 是 above 的实现，同时返回决策本身
// （仅用于映射命中时的结构化日志，不对外暴露）。
func openCodeQuotaFallbackRequestEvaluation(account *Account, normalUpstreamModel string, now time.Time) (OpenCodeQuotaFallbackRequestOutcome, OpenCodeQuotaFallbackDecision) {
	cfg := account.GetOpenCodeQuotaFallback()
	if !cfg.Enabled {
		return OpenCodeQuotaFallbackRequestOutcome{
			EffectiveUpstreamModel: strings.TrimSpace(normalUpstreamModel),
			DispatchAllowed:        true,
		}, OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateDisabled}
	}
	snap := decodeOpenCodeGoUsageSnapshot(account.Extra)
	decision := EvaluateOpenCodeQuotaFallback(cfg, snap, OpenCodeQuotaFallbackLastKnownOKData(snap), now)
	return decision.ResolveRequest(normalUpstreamModel), decision
}

// openCodeQuotaFallbackRoutingModelForBody 是 upstreamRoutingModel 的 legacy 分支：
// legacy opencode 没有 provider profile（routesByModel=false），默认不参与按模型协议
// 分流。额度保护把请求改写到管理员手选目标、或请求本身就是配置的直连目标时返回该
// 目标，让协议按目标能力选择（feature-off/未命中时返回 ""，行为与基线一致，避免
// 影响既有 gemini/jev 判定）。
func openCodeQuotaFallbackRoutingModelForBody(account *Account, body []byte, defaultMappedModel string) string {
	if account == nil || !account.IsOpenCodeGo() || account.Type != AccountTypeAPIKey {
		return ""
	}
	if !account.GetOpenCodeQuotaFallback().Enabled {
		return ""
	}
	normal := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, strings.TrimSpace(gjson.GetBytes(body, "model").String()), defaultMappedModel))
	outcome := openCodeQuotaFallbackRequestOutcomeForAccount(account, normal, time.Now())
	if outcome.MappingApplied {
		return outcome.EffectiveUpstreamModel
	}
	// 直连管理员手选目标：正常解析结果本身就是配置的 target 时，同样按目标自身
	// 的协议集合分流。不能因为本次没有发生 source→target 改写就落回 legacy
	// opencode 的协议兜底（gate 期望 target 直连走 /v1/chat/completions）。
	if account.usesOpenCodeQuotaFallbackTargetProtocol(normal) {
		return normal
	}
	return ""
}

// openCodeQuotaFallbackAllowsCapability 报告启用额度保护的 legacy opencode 账号是否
// 可按替代目标的能力放行某个入站端点能力：只有当至少一个已配置目标的协议集合
// （账号 protocol_rules → 目录元数据 → profile 内置规则）覆盖该能力所需协议时才放行。
// feature-off 或没有可用目标协议时不放行（保持 CC-only 原行为）。
func openCodeQuotaFallbackAllowsCapability(account *Account, capability OpenAIEndpointCapability) bool {
	if account == nil || !account.IsOpenCodeGo() || account.Type != AccountTypeAPIKey {
		return false
	}
	cfg := account.GetOpenCodeQuotaFallback()
	if !cfg.Enabled {
		return false
	}
	protocol, ok := openCodeQuotaFallbackProtocolForCapability(capability)
	if !ok {
		return false
	}
	for _, target := range cfg.ModelMapping {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if slices.Contains(account.modelProtocolSet(target, nil), protocol) {
			return true
		}
	}
	return false
}

func openCodeQuotaFallbackProtocolForCapability(capability OpenAIEndpointCapability) (string, bool) {
	switch capability {
	case OpenAIEndpointCapabilityChatCompletions:
		return APIProtocolChatCompletions, true
	case OpenAIEndpointCapabilityResponses:
		return APIProtocolResponses, true
	default:
		return "", false
	}
}

// applyOpenCodeQuotaFallbackUpstreamModel 把最终上游解析链的输出替换为 effective 目标。
// 未启用/未命中时原样返回（bit 级不变）。未获录用时同样返回正常上游模型：录用由
// 调度门禁（IsSchedulableForModelWithContext / 高级调度兼容判定）负责，映射阶段不
// 制造第二个判定，也不会悄悄回退付费原模型。
func applyOpenCodeQuotaFallbackUpstreamModel(account *Account, normalUpstreamModel string) string {
	if account == nil || !account.IsOpenCodeGo() {
		return normalUpstreamModel
	}
	outcome, decision := openCodeQuotaFallbackRequestEvaluation(account, normalUpstreamModel, time.Now())
	if outcome.MappingApplied {
		// 结构化映射日志：正常目标 → 实际替代目标 + 触发窗口/百分比 + 状态。
		// 只记模型名与窗口用量，不含任何 credentials / prompt 内容。
		logger.LegacyPrintf("service.opencode_quota_fallback",
			"[OpenCode] quota fallback mapped: %s -> %s (account: %s, state: %s, reason: %s, windows: %s)",
			strings.TrimSpace(normalUpstreamModel), outcome.EffectiveUpstreamModel, account.Name,
			decision.State, decision.Reason, openCodeQuotaFallbackWindowSummary(decision.TriggeringWindows))
	}
	return outcome.EffectiveUpstreamModel
}

// openCodeQuotaFallbackWindowSummary 渲染触发窗口摘要（5h=98.0% 形式），供结构化日志使用。
func openCodeQuotaFallbackWindowSummary(windows []OpenCodeQuotaFallbackWindow) string {
	if len(windows) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(windows))
	for _, window := range windows {
		parts = append(parts, fmt.Sprintf("%s=%.1f%%", window.Window, window.UsedPercent))
	}
	return strings.Join(parts, ", ")
}

// openCodeQuotaFallbackBlocksModel 报告额度保护是否拒绝本账号服务该请求模型：
// 仅当功能启用且正常上游模型在本状态下不可用时为 true。用于候选录用门禁。
func openCodeQuotaFallbackBlocksModel(account *Account, requestedModel string) bool {
	if account == nil || !account.IsOpenCodeGo() || account.Type != AccountTypeAPIKey {
		return false
	}
	if !account.GetOpenCodeQuotaFallback().Enabled {
		return false
	}
	// 与 normal resolver 同一解析（含 passthrough 分支），不自行二次映射。
	normalUpstream := resolveOpenAIAccountUpstreamNormalModel(account, requestedModel, false)
	return !openCodeQuotaFallbackRequestOutcomeForAccount(account, normalUpstream, time.Now()).DispatchAllowed
}

// openCodeQuotaFallbackRoutedTarget 返回本次请求在额度保护下实际会发出的替代目标
// （未发生替代时 ok=false）。供模型级冷却键使用：替代目标的自身冷却（401/403/rpm/
// model rate limit）必须同样作用于被改写的请求，而付费钱包冷却绝不作用于目标。
func openCodeQuotaFallbackRoutedTarget(account *Account, requestedModel string) (string, bool) {
	if account == nil || !account.IsOpenCodeGo() || account.Type != AccountTypeAPIKey {
		return "", false
	}
	if !account.GetOpenCodeQuotaFallback().Enabled {
		return "", false
	}
	// 与 normal resolver 同一解析（含 passthrough 分支），不自行二次映射。
	normalUpstream := resolveOpenAIAccountUpstreamNormalModel(account, requestedModel, false)
	outcome := openCodeQuotaFallbackRequestOutcomeForAccount(account, normalUpstream, time.Now())
	if !outcome.MappingApplied {
		return "", false
	}
	target := strings.TrimSpace(outcome.EffectiveUpstreamModel)
	if target == "" {
		return "", false
	}
	return target, true
}

// usesOpenCodeQuotaFallbackTargetProtocol 报告该模型是管理员为本账号配置的替代目标：
// 此时上游协议必须按目标自身的 protocol_rules/内置 profile/目录元数据选择，不能沿用
// legacy opencode 的 CC 强制兜底（Step 类目标曾因沿用 DeepSeek 的 CC 协议被上游以
// "Model does not support this protocol" 拒绝）。直连目标同样适用。
// 调用方仅在账号不按模型分流（legacy opencode）时需要它——opencode_go 已由
// routesByModel 分支按目标分流。
func (a *Account) usesOpenCodeQuotaFallbackTargetProtocol(model string) bool {
	if a == nil || !a.IsOpenCodeGo() || a.Type != AccountTypeAPIKey {
		return false
	}
	cfg := a.GetOpenCodeQuotaFallback()
	if !cfg.Enabled {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, target := range cfg.ModelMapping {
		if strings.TrimSpace(target) == model {
			return true
		}
	}
	return false
}

// resolveOpenCodeQuotaFallbackProtocolFor 按替代目标的协议集合选择上游协议：
// 入站协议在集合内则同协议直通，否则取集合首项。集合来源见 modelProtocolSet
// （账号 protocol_rules → 上游目录元数据 → profile 内置规则），不盲沿 CC。
func (a *Account) resolveOpenCodeQuotaFallbackProtocolFor(model, inbound string, catalog []string) string {
	set := a.modelProtocolSet(model, catalog)
	if slices.Contains(set, inbound) {
		return inbound
	}
	return set[0]
}

// openCodeQuotaFallbackUnsupportedTargetAllowed 报告通用网关已知不支持的模型
// （gemini-* / jev-*）能否作为替代目标：仅在账号自带自定义 relay base_url，或
// 账号 protocol_rules 为该模型显式声明了原生协议时才允许（自定义 relay / 显式
// 协议声明的能力不能由平台静态名单替管理员否决）。
func openCodeQuotaFallbackUnsupportedTargetAllowed(credentials map[string]any, target string) bool {
	if credentials == nil {
		return false
	}
	if baseURL, _ := credentials["base_url"].(string); strings.TrimSpace(baseURL) != "" && !isOpenCodeGoBaseURL(baseURL) {
		return true
	}
	if raw, ok := credentials[protocolRulesCredentialKey]; ok && raw != nil {
		if rules, err := parseProtocolRules(raw); err == nil {
			if _, matched := matchProtocolRuleSet(target, rules); matched {
				return true
			}
		}
	}
	return false
}

// openCodeQuotaFallbackSnapshotNeedsRefresh 报告策略账号是否需要一次新快照：
// 缺失、非 ok、无 fetched_at，或最后 ok 证据已超过 horizon（maxWait）视为需要。
// 与 due SQL 的 policy 分支（fetched_at + maxWait）同一语义，避免策略账号先被
// RunDue 筛入、又在执行层被活动驱动的 due 检查挡回（unknown 永久悬挂）。
func openCodeQuotaFallbackSnapshotNeedsRefresh(snap *OpenCodeGoUsageSnapshot, now time.Time, horizon time.Duration) bool {
	if horizon <= 0 {
		horizon = OpenCodeQuotaFallbackSnapshotStaleAfter
	}
	if snap == nil || snap.Status != OpenCodeGoUsageStatusOK || snap.FetchedAt == nil {
		return true
	}
	return !now.Before(snap.FetchedAt.Add(horizon))
}

// persistOpenCodeQuotaFallbackWalletLimit 是启用保护后配额耗尽的唯一写入形态：
// 只写模型作用域的付费钱包冷却（extra.model_rate_limits[opencode:paid_wallet]），
// 不写整账号 rate_limit_reset_at / temp_unschedulable / 进程内 Block。
// 返回 true 表示已成功持久化。未启用保护的账号不走此函数（保持原整账号行为）。
func persistOpenCodeQuotaFallbackWalletLimit(ctx context.Context, repo AccountRepository, account *Account, until time.Time, source string) bool {
	if repo == nil || account == nil || account.ID <= 0 || !until.After(time.Now()) {
		return false
	}
	reason := OpenCodeQuotaFallbackWalletReasonPrefix + ": " + strings.TrimSpace(source)
	if err := repo.SetModelRateLimit(ctx, account.ID, OpenCodeQuotaFallbackWalletScope, until, reason); err != nil {
		slog.Warn("opencode_quota_fallback_wallet_limit_failed",
			"account_id", account.ID,
			"until", until.UTC(),
			"source", source,
			"error", err)
		return false
	}
	slog.Info("opencode_quota_fallback_wallet_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
		"source", source)
	return true
}

// ValidateOpenCodeQuotaFallbackCredentials 校验并放行 credentials.quota_fallback
// （创建/更新保存侧唯一校验）。返回 infraerrors BadRequest（400）供 handler 直接
// 返回；enabled=false 的空配置容忍存储（纯 no-op），enabled=true 时：
//   - 只允许 platform opencode / opencode_go、type=apikey、Go 模式（zen 拒绝）；
//   - 结构按 ParseOpenCodeQuotaFallback 严格校验。
func ValidateOpenCodeQuotaFallbackCredentials(platform, accountType string, credentials map[string]any) error {
	config, err := parseAndValidateOpenCodeQuotaFallbackBlock(credentials)
	if err != nil || config == nil || !config.Enabled {
		return err
	}
	if !isOpenCodeQuotaFallbackPlatform(platform) {
		return infraerrors.BadRequest("INVALID_OPENCODE_QUOTA_FALLBACK",
			"quota_fallback can only be enabled for OpenCode accounts (platform opencode / opencode_go)")
	}
	if accountType != AccountTypeAPIKey {
		return infraerrors.BadRequest("INVALID_OPENCODE_QUOTA_FALLBACK",
			"quota_fallback requires an API key OpenCode account")
	}
	if mode, _ := credentials["account_mode"].(string); strings.TrimSpace(mode) == AccountModeZen {
		return infraerrors.BadRequest("INVALID_OPENCODE_QUOTA_FALLBACK",
			"quota_fallback requires OpenCode Go mode; zen mode accounts have no subscription quota windows")
	}
	return nil
}

// parseAndValidateOpenCodeQuotaFallbackBlock 解析 credentials.quota_fallback 并校验
// 结构与替代目标能力；块缺失时返回 (nil, nil)。
func parseAndValidateOpenCodeQuotaFallbackBlock(credentials map[string]any) (*OpenCodeQuotaFallbackConfig, error) {
	if credentials == nil {
		return nil, nil
	}
	raw, ok := credentials[OpenCodeQuotaFallbackCredentialKey]
	if !ok || raw == nil {
		return nil, nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return nil, infraerrors.BadRequest("INVALID_OPENCODE_QUOTA_FALLBACK", "quota_fallback must be an object")
	}
	config, err := ParseOpenCodeQuotaFallback(block)
	if err != nil {
		return nil, infraerrors.BadRequest("INVALID_OPENCODE_QUOTA_FALLBACK", err.Error())
	}
	if !config.Enabled {
		return &config, nil
	}
	// 通用网关已知不支持的模型（gemini-* / jev-*）不能作为替代目标；账号自带
	// 自定义 relay base_url 或 protocol_rules 显式声明该模型协议时放行（能力由
	// 管理员声明，不由平台静态名单替其否决）。
	sources := make([]string, 0, len(config.ModelMapping))
	for source := range config.ModelMapping {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		target := config.ModelMapping[source]
		if !IsOpenCodeUnsupportedModel(target) || openCodeQuotaFallbackUnsupportedTargetAllowed(credentials, target) {
			continue
		}
		return nil, infraerrors.BadRequest("INVALID_OPENCODE_QUOTA_FALLBACK", fmt.Sprintf(
			"quota_fallback.model_mapping[%q] target %q is not served by the OpenCode standard gateway; "+
				"configure base_url or protocol_rules for that model first", source, target))
	}
	return &config, nil
}

func isOpenCodeQuotaFallbackPlatform(platform string) bool {
	return platform == PlatformOpenCodeGo || platform == PlatformOpenCode
}

// OpenCodeQuotaFallbackWindowStatus 是 DTO 中的窗口证据（只读，无 secret）。
type OpenCodeQuotaFallbackWindowStatus struct {
	Window      string     `json:"window"`
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}

// OpenCodeQuotaFallbackStatus 是 admin 账号 DTO 顶层的只读状态块。
// 仅 OpenCode 平台账号返回（其它平台 nil + omitempty）。
type OpenCodeQuotaFallbackStatus struct {
	Enabled           bool                                `json:"enabled"`
	State             string                              `json:"state"`
	ThresholdPercent  int                                 `json:"threshold_percent"`
	TriggeringWindows []OpenCodeQuotaFallbackWindowStatus `json:"triggering_windows,omitempty"`
	SnapshotAt        *time.Time                          `json:"snapshot_at,omitempty"`
	Reason            string                              `json:"reason,omitempty"`
}

// OpenCodeQuotaFallbackStatusFromAccount 由 cfg+快照现算状态（UI 只读展示，
// 不反向写配置，也不含任何 credentials secret）。
func OpenCodeQuotaFallbackStatusFromAccount(account *Account) *OpenCodeQuotaFallbackStatus {
	if account == nil || !account.IsOpenCodeGo() {
		return nil
	}
	cfg := account.GetOpenCodeQuotaFallback()
	threshold := cfg.ThresholdPercent
	if threshold < OpenCodeQuotaFallbackMinThresholdPercent || threshold > OpenCodeQuotaFallbackMaxThresholdPercent {
		threshold = OpenCodeQuotaFallbackDefaultThresholdPercent
	}
	status := &OpenCodeQuotaFallbackStatus{
		Enabled:          cfg.Enabled,
		State:            OpenCodeQuotaFallbackStateDisabled,
		ThresholdPercent: threshold,
	}
	if !cfg.Enabled {
		return status
	}
	now := time.Now()
	snap := decodeOpenCodeGoUsageSnapshot(account.Extra)
	decision := EvaluateOpenCodeQuotaFallback(cfg, snap, OpenCodeQuotaFallbackLastKnownOKData(snap), now)
	status.State = decision.State
	status.SnapshotAt = decision.SnapshotAt
	status.Reason = decision.Reason
	for _, window := range decision.TriggeringWindows {
		status.TriggeringWindows = append(status.TriggeringWindows, OpenCodeQuotaFallbackWindowStatus(window))
	}
	return status
}
