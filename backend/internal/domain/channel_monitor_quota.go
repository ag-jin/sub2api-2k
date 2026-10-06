package domain

import "time"

// 渠道监控「配额模式」的归一化配额快照类型。
//
// 配额模式监控不直接对接上游，而是关联一个已有账号，复用账号侧的用量服务
// （AccountUsageService / CNProviderQuotaService / CNProviderBalanceService），
// 把各平台形态各异的用量数据归一成 MonitorQuotaSnapshot，随检测历史持久化
// 到 channel_monitor_histories.quota（JSONB），供管理端与用户端渲染。
//
// 类型放在 domain 包是因为 ent schema（internal/domain 的下游）需要引用它做
// field.JSON 序列化；service 不能被 ent import（会造成循环依赖）。

// MonitorQuotaTier 单个用量窗口的快照。
//
// Window 取值约定（与前端 monitorCommon.quota.windows.* 标签一一对应）：
//   - "5h"         5 小时滚动窗口（Claude/Codex/Kimi/Zhipu coding plan）
//   - "7d"         7 天窗口（Claude/Codex）
//   - "7d-sonnet"  Claude 7 天 Sonnet 独立额度
//   - "7d-fable"   Claude 7 天 Fable 独立额度
//   - "weekly"     周窗口（Kimi/Zhipu coding plan）
//   - "daily"      日窗口（Gemini 日配额 / Grok 日请求）
//   - "30d"        30 天窗口（Grok 月度）
//   - "total"      无窗口语义的总量额度（Antigravity per-model 等）
//
// 同一 Window 可能出现多条（Gemini 多档日配额、Antigravity per-model、
// Grok requests/tokens），用 Label 区分：Label 是机器 token（requests/tokens/
// shared/pro/flash 或模型名），前端已知 token 走 i18n，未知原样展示。
type MonitorQuotaTier struct {
	Window      string  `json:"window"`
	Label       string  `json:"label,omitempty"`
	UsedPercent float64 `json:"used_percent"` // 0-100+；仅有绝对值时按 used/limit 计算
	Used        float64 `json:"used,omitempty"`
	Limit       float64 `json:"limit,omitempty"`
	ResetAt     string  `json:"reset_at,omitempty"` // RFC3339；未知时留空
}

// MonitorQuotaSnapshot 一次配额查询的完整快照。
//
// Source 取值：
//   - "usage"      海外平台（AccountUsageService.GetUsage）
//   - "cn_quota"   国产 Coding Plan（CNProviderQuotaService.QueryUsage）
//   - "cn_balance" 国产按量付费余额（CNProviderBalanceService.QueryBalance）
type MonitorQuotaSnapshot struct {
	Source    string             `json:"source"`
	Success   bool               `json:"success"`
	Tiers     []MonitorQuotaTier `json:"tiers,omitempty"`
	Balance   *float64           `json:"balance,omitempty"`    // cn_balance 主余额
	Balances  []MonitorBalance   `json:"balances,omitempty"`   // 多币种余额（如 DeepSeek CNY+USD）
	Currency  string             `json:"currency,omitempty"`   // 主余额币种
	PlanLevel string             `json:"plan_level,omitempty"` // 套餐等级（如智谱 level）
	// BalanceLow 余额低于阈值或账号被上游标记不可用（仅 cn_balance 来源）。
	// 抓取器按 Gateway.CNProviders.BalanceThreshold 判定，口径与账号停调
	// （CNProviderBalanceCheckService.checkOne）一致：任一币种达标即健康。
	BalanceLow bool `json:"balance_low,omitempty"`
	// CredentialInvalid 上游 401/403 鉴权失败（区别于网络/解析错误），
	// 检测状态据此推导 failed 而非 error。
	CredentialInvalid bool      `json:"credential_invalid,omitempty"`
	Error             string    `json:"error,omitempty"` // Success=false 时的错误摘要
	FetchedAt         time.Time `json:"fetched_at"`
	// ModelCredits 登录态智谱账号的逐模型逐日积分明细（design M4）。可选：
	// 非托管账号、数据源失败或未接线时不出现；一期不参与状态判定。
	ModelCredits []MonitorQuotaModelCredit `json:"model_credits,omitempty"`
	// ResetCards 登录态智谱账号的可用重置卡（只读展示，R0）。可选：
	// 采集失败/无卡时不出现，前端须静默降级。
	ResetCards []MonitorResetCard `json:"reset_cards,omitempty"`
	// NeedsRelogin 登录态凭据需重登（读 accounts.extra 的
	// zhipu_needs_relogin，契约见票 09）。可选，不影响 Success/状态判定。
	NeedsRelogin bool `json:"needs_relogin,omitempty"`
}

// MonitorBalance 单币种余额条目。
type MonitorBalance struct {
	Currency string  `json:"currency"`
	Balance  float64 `json:"balance"`
}

// MonitorQuotaModelCredit 登录态智谱账号在某一自然日的单模型积分明细
// （credit-usage/usage-detail 的 modelDataList 逐日展开；design M4）。
//
// Date 是上游响应 xTime 的日历日（Asia/Shanghai），与账号本地时区无关。
// InputTokens 为未缓存输入（uncachedInputTokensUsage），与 CachedTokens、
// OutputTokens 一起对应计费三项，Credits 取当日 totalCreditsUsage。
type MonitorQuotaModelCredit struct {
	Model        string  `json:"model"`
	Date         string  `json:"date"`
	InputTokens  float64 `json:"input_tokens"`
	CachedTokens float64 `json:"cached_tokens"`
	OutputTokens float64 `json:"output_tokens"`
	Credits      float64 `json:"credits"`
}

// MonitorResetCard 登录态智谱账号的一张可用重置卡（只读，design M4 / 决策 B4）。
//
// R0：仅观测，永不使用。本类型是重置卡在本系统的**全部**形态——只承载展示数据，
// 不提供卡片 id、不提供消耗/兑换字段，也不允许出现任何「使用重置卡」的写路径
// （校验：scripts/check_r0_invariant.sh，票 13）。
//
// Type 取值 "five_hour"（5 小时窗口重置）或 "week"（周窗口重置）；
// ExpireAt 是上游 expire_at 的原样字符串（RFC3339，缺失时留空）。
// 本类型只描述快照内的只读展示数据：系统不提供任何「使用重置卡」路径（R0）。
type MonitorResetCard struct {
	Type     string `json:"type"`
	ExpireAt string `json:"expire_at"`
}
