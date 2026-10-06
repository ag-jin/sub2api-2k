package service

import (
	"strings"
	"time"
)

// Ops alert rule/event models.
//
// NOTE: These are admin-facing DTOs and intentionally keep JSON naming aligned
// with the existing ops dashboard frontend (backup style).

const (
	OpsAlertStatusFiring         = "firing"
	OpsAlertStatusResolved       = "resolved"
	OpsAlertStatusManualResolved = "manual_resolved"
)

// 内置指标类型名（跨票契约，不得改名）：
//   - 票 25 在评估器里注册数据源，并按这些名字生成事件与邮件；
//   - 票 27 用 OpsMetricTypeZhipuSignEffectiveRate 上报 L2 对账有效系数；
//   - 票 30 的面板 / 票 31 的三级告警演练按这些名字匹配规则。
const (
	// OpsMetricTypeZhipuSignFailWindow 是智谱签名 L1 失效窗口指标（design M3.1(b)(d)）：
	// 当前 5 分钟桶内「VERIFY_* 验签失效 + 握手失败 + 降级无签名发送」的次数之和。
	// 默认阈值 0（出现即告警）：0.67 渠道系数失效必须先被看见，再谈处置。
	OpsMetricTypeZhipuSignFailWindow = "zhipu_sign_fail_window"
	// OpsMetricTypeZhipuSignEffectiveRate 是 L2 费率对账有效系数（票 27 上报）：
	// 0.67 = 签名生效；≈1.0 = 签名被静默按无签名计费（无报错的失效路径）。
	// 默认规则 "> 0.70" 告警。
	OpsMetricTypeZhipuSignEffectiveRate = "zhipu_sign_effective_rate"
)

// OpsBuiltinAlertMetric 是一个内置指标类型的注册项：默认规则参数 + 规则文案 + 建议动作。
//
// 这里的默认值只是「管理端建规则」与「票 31 演练」的起点：运行期阈值、严重度、冷却、
// 静默一律以 ops_alert_rules 表为准，内置指标类型不做任何硬编码短路。
type OpsBuiltinAlertMetric struct {
	MetricType string
	// Operator/Threshold 是默认告警条件：L1 用 ">" 0（出现即告警），L2 用 ">" 0.70
	// （偏离 0.67 基准即「签名被静默按 1.0 计费」）。
	Operator  string
	Threshold float64
	// WindowMinutes 是指标自身的统计窗口：L1 = 5 分钟桶；L2 = 对账周期（默认 6 小时）。
	// 评估器按它渲染文案，避免规则里的 window_minutes 与指标实际窗口不一致。
	WindowMinutes    int
	SustainedMinutes int
	CooldownMinutes  int
	// Severity 是默认严重度，取值同 validOpsAlertSeverities（P0–P3）。
	Severity string
	// Name/Description 是默认规则文案；Description 不含任何凭据。
	Name        string
	Description string
	// SuggestedAction 是告警事件与默认邮件文案附带的建议动作（design M3.1(d) 硬性要求：
	// 文案必须给出处置建议，且只提配置键，不提任何凭据）。
	SuggestedAction string
	// Platform 是 OpsAlertSilence 的 platform 维度取值，也是默认规则的 filters.platform。
	Platform string
	// NotifyEmail 是默认规则的邮件开关（实际发送仍受 ops 邮件通道配置约束）。
	NotifyEmail bool
}

// opsBuiltinAlertMetrics 是全部内置指标类型的注册表。顺序稳定：票 30 面板与文档按它渲染。
var opsBuiltinAlertMetrics = []OpsBuiltinAlertMetric{
	{
		MetricType:       OpsMetricTypeZhipuSignFailWindow,
		Operator:         ">",
		Threshold:        0,
		WindowMinutes:    5,
		SustainedMinutes: 0,
		CooldownMinutes:  30,
		Severity:         "P1",
		Name:             "智谱签名失效（L1 实时窗口）",
		Description:      "5 分钟窗口内出现 VERIFY_* 验签失效、握手失败或降级无签名发送（计数出现即告警）",
		SuggestedAction:  "检查 X-Client-Version 是否为 0.16.9；若上游持续拒绝签名，先关停全局签名开关（gateway.zhipu.sign_v4_enabled）",
		Platform:         "zhipu",
		NotifyEmail:      true,
	},
	{
		MetricType:       OpsMetricTypeZhipuSignEffectiveRate,
		Operator:         ">",
		Threshold:        0.70,
		WindowMinutes:    360,
		SustainedMinutes: 0,
		CooldownMinutes:  360,
		Severity:         "P1",
		Name:             "智谱签名有效系数偏离（L2 费率对账）",
		Description:      "费率对账有效系数 > 0.70（0.67 = 签名生效；≈1.0 = 签名被静默按无签名计费）",
		SuggestedAction:  "检查 X-Client-Version 是否为 0.16.9；若上游已接受签名但系数仍为 1.0，先关停全局签名开关（gateway.zhipu.sign_v4_enabled）止损",
		Platform:         "zhipu",
		NotifyEmail:      true,
	},
}

// OpsBuiltinAlertMetrics 返回全部内置指标类型的注册项副本。
func OpsBuiltinAlertMetrics() []OpsBuiltinAlertMetric {
	return append([]OpsBuiltinAlertMetric(nil), opsBuiltinAlertMetrics...)
}

// OpsBuiltinAlertMetricFor 按指标类型名返回注册项；未注册返回 ok=false。
func OpsBuiltinAlertMetricFor(metricType string) (OpsBuiltinAlertMetric, bool) {
	name := strings.TrimSpace(metricType)
	for _, metric := range opsBuiltinAlertMetrics {
		if metric.MetricType == name {
			return metric, true
		}
	}
	return OpsBuiltinAlertMetric{}, false
}

// OpsBuiltinAlertRule 返回内置指标类型的默认规则模板（ID=0 = 未落库；每次调用返回新副本，
// 调用方可安全修改后再落库）。未注册的指标类型返回 ok=false。
func OpsBuiltinAlertRule(metricType string) (*OpsAlertRule, bool) {
	metric, ok := OpsBuiltinAlertMetricFor(metricType)
	if !ok {
		return nil, false
	}
	rule := &OpsAlertRule{
		Name:             metric.Name,
		Description:      metric.Description,
		Enabled:          true,
		Severity:         metric.Severity,
		MetricType:       metric.MetricType,
		Operator:         metric.Operator,
		Threshold:        metric.Threshold,
		WindowMinutes:    metric.WindowMinutes,
		SustainedMinutes: metric.SustainedMinutes,
		CooldownMinutes:  metric.CooldownMinutes,
		NotifyEmail:      metric.NotifyEmail,
	}
	if metric.Platform != "" {
		rule.Filters = map[string]any{"platform": metric.Platform}
	}
	return rule, true
}

// OpsBuiltinAlertRules 返回全部内置指标类型的默认规则模板。
func OpsBuiltinAlertRules() []*OpsAlertRule {
	rules := make([]*OpsAlertRule, 0, len(opsBuiltinAlertMetrics))
	for _, metric := range opsBuiltinAlertMetrics {
		if rule, ok := OpsBuiltinAlertRule(metric.MetricType); ok {
			rules = append(rules, rule)
		}
	}
	return rules
}

type OpsAlertRule struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Enabled  bool   `json:"enabled"`
	Severity string `json:"severity"`

	MetricType string  `json:"metric_type"`
	Operator   string  `json:"operator"`
	Threshold  float64 `json:"threshold"`

	WindowMinutes    int `json:"window_minutes"`
	SustainedMinutes int `json:"sustained_minutes"`
	CooldownMinutes  int `json:"cooldown_minutes"`

	NotifyEmail bool `json:"notify_email"`

	Filters map[string]any `json:"filters,omitempty"`

	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type OpsAlertEvent struct {
	ID       int64  `json:"id"`
	RuleID   int64  `json:"rule_id"`
	Severity string `json:"severity"`
	Status   string `json:"status"`

	Title       string `json:"title"`
	Description string `json:"description"`

	MetricValue    *float64 `json:"metric_value,omitempty"`
	ThresholdValue *float64 `json:"threshold_value,omitempty"`

	Dimensions map[string]any `json:"dimensions,omitempty"`

	FiredAt    time.Time  `json:"fired_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`

	EmailSent bool      `json:"email_sent"`
	CreatedAt time.Time `json:"created_at"`
}

type OpsAlertSilence struct {
	ID int64 `json:"id"`

	RuleID   int64   `json:"rule_id"`
	Platform string  `json:"platform"`
	GroupID  *int64  `json:"group_id,omitempty"`
	Region   *string `json:"region,omitempty"`

	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`

	CreatedBy *int64    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type OpsAlertEventFilter struct {
	Limit int

	// Cursor pagination (descending by fired_at, then id).
	BeforeFiredAt *time.Time
	BeforeID      *int64

	// Optional filters.
	Status    string
	Severity  string
	EmailSent *bool

	StartTime *time.Time
	EndTime   *time.Time

	// Dimensions filters (best-effort).
	Platform string
	GroupID  *int64
}
