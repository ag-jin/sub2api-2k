package service

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
)

// 智谱签名 V4 的配置面（design M3.1(d)(e) / 票 28）。
//
// 配置事实源分两层：
//
//  1. 部署层：cfg.Gateway.Zhipu 的 sign_* 键（#01 定义，viper SetDefaults 保证有值）；
//  2. 运行层：system_settings 里同名的 gateway.zhipu.sign_* 覆盖（本票新增）。
//
// 生效值 = 运行层覆盖 > 部署层 > 协议默认。运行层只能由管理端写入，写入前强制校验，
// 且写入即推送签名器（SetOptions）——协议漂移是「人工确认后的显式修改」，设计明令
// 不存在任何自动跟随官方版本的路径。
//
// 热更新机制沿用仓库既有 settings 范式（setting_service.go / setting_gateway_runtime.go）：
// system_settings 表 + SettingRepository + 进程内短 TTL 缓存；差异在于本票把缓存
// 失效做成写穿（Update 内 invalidate + SetOptions），因此同一实例内在下一次 Sign
// 就生效，不存在「TTL 内仍读旧值」的窗口。
const (
	// SettingKeyZhipuSignV4Enabled 是全局签名开关的运行期覆盖键。
	SettingKeyZhipuSignV4Enabled = "gateway.zhipu.sign_v4_enabled"
	// SettingKeyZhipuSignClientVersion 是 X-Client-Version 的运行期覆盖键。
	SettingKeyZhipuSignClientVersion = "gateway.zhipu.sign_client_version"
	// SettingKeyZhipuSignPowBits 是 PoW 难度位数的运行期覆盖键。
	SettingKeyZhipuSignPowBits = "gateway.zhipu.sign_pow_bits"
	// SettingKeyZhipuSignKeyTTLMinutes 是握手私钥缓存 TTL（分钟）的运行期覆盖键。
	SettingKeyZhipuSignKeyTTLMinutes = "gateway.zhipu.sign_key_ttl_minutes"
	// SettingKeyZhipuSignHandshakeBackoffSeconds 是每 key 重握手最小间隔（秒）的运行期覆盖键。
	SettingKeyZhipuSignHandshakeBackoffSeconds = "gateway.zhipu.sign_handshake_backoff_seconds"
	// SettingKeyZhipuSignAccountCircuitBreakThreshold 是账号级熔断阈值（5 分钟窗口内验签失效次数）的运行期覆盖键。
	SettingKeyZhipuSignAccountCircuitBreakThreshold = "gateway.zhipu.sign_account_circuit_break_threshold"
	// SettingKeyZhipuSignFailPolicy 是签名失败策略的运行期覆盖键。
	SettingKeyZhipuSignFailPolicy = "gateway.zhipu.sign_fail_policy"
	// SettingKeyZhipuSignAlertEnabled 是签名告警总开关的运行期覆盖键。
	SettingKeyZhipuSignAlertEnabled = "gateway.zhipu.sign_alert_enabled"
	// SettingKeyZhipuSignReconcileIntervalHours 是费率对账周期（小时）的运行期覆盖键。
	SettingKeyZhipuSignReconcileIntervalHours = "gateway.zhipu.sign_reconcile_interval_hours"
	// SettingKeyZhipuSignReconcileDeviationThreshold 是有效系数偏差阈值（L2 对账）的运行期覆盖键。
	SettingKeyZhipuSignReconcileDeviationThreshold = "gateway.zhipu.sign_reconcile_deviation_threshold"
)

// 签名失败策略枚举（design M3.1(c)）：open = 剥离签名头降级（降级即 1.0 系数，必须告警），
// closed = 返回可 failover 错误，交给调度器换账号。
const (
	ZhipuSignFailPolicyOpen   = "open"
	ZhipuSignFailPolicyClosed = "closed"
)

// 管理端写入的校验边界。越界值会直接破坏签名（全量账号受影响）或拖垮数据面，故拒绝写入。
// 上限取「明显大于协议需求且不会自伤」的值：PoW 位数的搜索空间随位数指数增长。
const (
	zhipuSignPowBitsMax                = 16
	zhipuSignKeyTTLMinutesMax          = 7 * 24 * 60
	zhipuSignHandshakeBackoffMaxSecond = 3600
	zhipuSignCircuitBreakThresholdMax  = 10000
	zhipuSignReconcileIntervalHoursMax = 168
	zhipuSignClientVersionMaxLength    = 64
)

// zhipuSignConfigCacheTTL 是覆盖层的进程内缓存时长。签名热路径每个请求都会读到它，
// 故不允许每次都打 DB；写穿（Update）保证同一实例内立刻生效，TTL 只兜住
// 「别的实例改了设置」与「直接改库」这两类跨实例场景。
const zhipuSignConfigCacheTTL = 30 * time.Second

// zhipuSignClientVersionPattern 是 X-Client-Version 的合法形态（semver 风格）。
// 版本是签名消息的一部分（signed field），空值/空白/任意字符串会让全部账号验签失败。
var zhipuSignClientVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// 管理面写入的错误 reason（前端 29 按 reason 做本地化，message 只作兜底展示）。
const (
	// ZhipuSignConfigReasonInvalid 表示请求里的某个键取值非法，未写入任何键。
	ZhipuSignConfigReasonInvalid = "ZHIPU_SIGN_CONFIG_INVALID"
	// ZhipuSignConfigReasonEmpty 表示请求未携带任何签名配置键。
	ZhipuSignConfigReasonEmpty = "ZHIPU_SIGN_CONFIG_EMPTY"
	// ZhipuSignConfigReasonUnavailable 表示设置存储不可用（构造缺依赖）。
	ZhipuSignConfigReasonUnavailable = "ZHIPU_SIGN_CONFIG_UNAVAILABLE"
)

// zhipuSignConfigInvalid 构造一条可读的校验错误：键名 + 允许范围 + 实际取值。
func zhipuSignConfigInvalid(message string) error {
	return infraerrors.BadRequest(ZhipuSignConfigReasonInvalid, message)
}

// ZhipuSignConfig 是签名配置的生效值快照。JSON 字段名与 cfg.Gateway.Zhipu 的键名一致，
// 前端 29 与运维排查用同一套名字。
type ZhipuSignConfig struct {
	SignV4Enabled                    bool    `json:"sign_v4_enabled"`
	SignClientVersion                string  `json:"sign_client_version"`
	SignPowBits                      int     `json:"sign_pow_bits"`
	SignKeyTTLMinutes                int     `json:"sign_key_ttl_minutes"`
	SignHandshakeBackoffSeconds      int     `json:"sign_handshake_backoff_seconds"`
	SignAccountCircuitBreakThreshold int     `json:"sign_account_circuit_break_threshold"`
	SignFailPolicy                   string  `json:"sign_fail_policy"`
	SignAlertEnabled                 bool    `json:"sign_alert_enabled"`
	SignReconcileIntervalHours       int     `json:"sign_reconcile_interval_hours"`
	SignReconcileDeviationThreshold  float64 `json:"sign_reconcile_deviation_threshold"`
}

// ZhipuSignConfigView 是管理面读取的响应体：生效值 + 由运行期覆盖过的键（排序）。
// OverriddenKeys 让「哪些键偏离了部署配置」对前端与审计可见，回滚时按它逐个还原。
type ZhipuSignConfigView struct {
	ZhipuSignConfig
	OverriddenKeys []string `json:"overridden_keys"`
}

// ZhipuSignConfigUpdate 是管理面更新请求：指针字段表示「只有传了的键才改」，
// 未传的键保持当前生效值（部分更新，避免前端陈旧表单把未展示的键覆盖回旧值）。
type ZhipuSignConfigUpdate struct {
	SignV4Enabled                    *bool    `json:"sign_v4_enabled"`
	SignClientVersion                *string  `json:"sign_client_version"`
	SignPowBits                      *int     `json:"sign_pow_bits"`
	SignKeyTTLMinutes                *int     `json:"sign_key_ttl_minutes"`
	SignHandshakeBackoffSeconds      *int     `json:"sign_handshake_backoff_seconds"`
	SignAccountCircuitBreakThreshold *int     `json:"sign_account_circuit_break_threshold"`
	SignFailPolicy                   *string  `json:"sign_fail_policy"`
	SignAlertEnabled                 *bool    `json:"sign_alert_enabled"`
	SignReconcileIntervalHours       *int     `json:"sign_reconcile_interval_hours"`
	SignReconcileDeviationThreshold  *float64 `json:"sign_reconcile_deviation_threshold"`
}

// zhipuSignRuntime 是本票驱动的签名器接缝：#20 的 SetOptions 热更新接缝与只读
// KeyStatus 状态接缝。生产实现是 *zcodesign.Signer（ProvideZhipuClientSigner 的返回值）。
type zhipuSignRuntime interface {
	SetOptions(options zcodesign.SignerOptions)
	KeyStatus(apiKeyID string) zcodesign.KeyStatus
}

// ZhipuSignConfigService 是签名配置面：读取生效配置、校验并写入管理端修改、
// 汇总状态读取面（供前端 29 与 L3 漂移演练 31）。
type ZhipuSignConfigService struct {
	cfg         *config.Config
	settingRepo SettingRepository
	accountRepo zhipuSignAccountLister
	runtime     zhipuSignRuntime
	// circuitBreak 是账号级熔断运行时状态的只读来源（票 24 的 *ZhipuSignAlerts；
	// 装配期经 SetCircuitBreakReader 注入）。为 nil 时状态投影报「未知」。
	circuitBreak zhipuSignCircuitBreakReader
	// now 是时钟接缝：测试用它跨过缓存 TTL，生产用 time.Now。
	now func() time.Time

	mu         sync.Mutex
	cached     *zhipuSignConfigSnapshot
	applied    zcodesign.SignerOptions
	appliedSet bool
}

// zhipuSignConfigSnapshot 是一次 DB 读取的结果：生效值 + 覆盖键 + 过期时刻。
type zhipuSignConfigSnapshot struct {
	config    ZhipuSignConfig
	overrides []string
	expiresAt time.Time
}

// NewZhipuSignConfigService 构造签名配置面。settingRepo 为 nil 时只有部署层配置可用
// （测试装配）；accountRepo 为 nil 时状态接口返回空账号列表；runtime 为 nil 时配置面
// 仍可读写，只是不会推送签名器。
func NewZhipuSignConfigService(
	cfg *config.Config,
	settingRepo SettingRepository,
	accountRepo zhipuSignAccountLister,
	runtime zhipuSignRuntime,
) *ZhipuSignConfigService {
	return &ZhipuSignConfigService{
		cfg:         cfg,
		settingRepo: settingRepo,
		accountRepo: accountRepo,
		runtime:     runtime,
		now:         time.Now,
	}
}

// SetCircuitBreakReader 注入账号级熔断运行时状态的只读来源（票 24 的
// *ZhipuSignAlerts；装配期由 wire 调用一次）。传入 nil 等价于未接线：状态投影按
// runtime_state_available=false 渲染为「未知」。装配完成后运行期只读该字段。
func (s *ZhipuSignConfigService) SetCircuitBreakReader(reader zhipuSignCircuitBreakReader) {
	if s == nil {
		return
	}
	s.circuitBreak = reader
}

// zhipuSignSettingKeys 是全部可热更新的键（一次 GetMultiple 读完）。
func zhipuSignSettingKeys() []string {
	return []string{
		SettingKeyZhipuSignV4Enabled,
		SettingKeyZhipuSignClientVersion,
		SettingKeyZhipuSignPowBits,
		SettingKeyZhipuSignKeyTTLMinutes,
		SettingKeyZhipuSignHandshakeBackoffSeconds,
		SettingKeyZhipuSignAccountCircuitBreakThreshold,
		SettingKeyZhipuSignFailPolicy,
		SettingKeyZhipuSignAlertEnabled,
		SettingKeyZhipuSignReconcileIntervalHours,
		SettingKeyZhipuSignReconcileDeviationThreshold,
	}
}

// zhipuSignProtocolDefaults 是「部署层整段缺失」（cfg 为 nil 或 gateway.zhipu 全零值）
// 时的协议默认值，逐字对齐 config.go 的 viper SetDefaults（#01）与 zcodesign 常量。
func zhipuSignProtocolDefaults() ZhipuSignConfig {
	return ZhipuSignConfig{
		SignV4Enabled:                    true,
		SignClientVersion:                zcodesign.DefaultClientVersion,
		SignPowBits:                      zcodesign.DefaultPowBits,
		SignKeyTTLMinutes:                int(zcodesign.DefaultSignKeyTTL / time.Minute),
		SignHandshakeBackoffSeconds:      int(zcodesign.DefaultHandshakeBackoff / time.Second),
		SignAccountCircuitBreakThreshold: 10,
		SignFailPolicy:                   ZhipuSignFailPolicyOpen,
		SignAlertEnabled:                 true,
		SignReconcileIntervalHours:       6,
		SignReconcileDeviationThreshold:  0.70,
	}
}

// defaults 返回部署层（cfg.Gateway.Zhipu）的默认配置，越界值回落到协议默认：
// 配置文件写错时宁可退回协议默认，也不能把非法位数/TTL 推给签名器。
func (s *ZhipuSignConfigService) defaults() ZhipuSignConfig {
	if s == nil || s.cfg == nil || s.cfg.Gateway.Zhipu == (config.GatewayZhipuConfig{}) {
		return zhipuSignProtocolDefaults()
	}
	zhipu := s.cfg.Gateway.Zhipu
	return sanitizeZhipuSignConfig(ZhipuSignConfig{
		SignV4Enabled:                    zhipu.SignV4Enabled,
		SignClientVersion:                zhipu.SignClientVersion,
		SignPowBits:                      zhipu.SignPowBits,
		SignKeyTTLMinutes:                zhipu.SignKeyTTLMinutes,
		SignHandshakeBackoffSeconds:      zhipu.SignHandshakeBackoffSeconds,
		SignAccountCircuitBreakThreshold: zhipu.SignAccountCircuitBreakThreshold,
		SignFailPolicy:                   zhipu.SignFailPolicy,
		SignAlertEnabled:                 zhipu.SignAlertEnabled,
		SignReconcileIntervalHours:       zhipu.SignReconcileIntervalHours,
		SignReconcileDeviationThreshold:  zhipu.SignReconcileDeviationThreshold,
	})
}

func sanitizeZhipuSignConfig(in ZhipuSignConfig) ZhipuSignConfig {
	defaults := zhipuSignProtocolDefaults()
	in.SignClientVersion = strings.TrimSpace(in.SignClientVersion)
	if !validZhipuSignClientVersion(in.SignClientVersion) {
		in.SignClientVersion = defaults.SignClientVersion
	}
	if in.SignPowBits < 0 || in.SignPowBits > zhipuSignPowBitsMax {
		in.SignPowBits = defaults.SignPowBits
	}
	if in.SignKeyTTLMinutes < 1 || in.SignKeyTTLMinutes > zhipuSignKeyTTLMinutesMax {
		in.SignKeyTTLMinutes = defaults.SignKeyTTLMinutes
	}
	if in.SignHandshakeBackoffSeconds < 0 || in.SignHandshakeBackoffSeconds > zhipuSignHandshakeBackoffMaxSecond {
		in.SignHandshakeBackoffSeconds = defaults.SignHandshakeBackoffSeconds
	}
	if in.SignAccountCircuitBreakThreshold < 1 || in.SignAccountCircuitBreakThreshold > zhipuSignCircuitBreakThresholdMax {
		in.SignAccountCircuitBreakThreshold = defaults.SignAccountCircuitBreakThreshold
	}
	switch in.SignFailPolicy {
	case ZhipuSignFailPolicyOpen, ZhipuSignFailPolicyClosed:
	default:
		in.SignFailPolicy = defaults.SignFailPolicy
	}
	if in.SignReconcileIntervalHours < 1 || in.SignReconcileIntervalHours > zhipuSignReconcileIntervalHoursMax {
		in.SignReconcileIntervalHours = defaults.SignReconcileIntervalHours
	}
	if in.SignReconcileDeviationThreshold <= 0 || in.SignReconcileDeviationThreshold > 1 {
		in.SignReconcileDeviationThreshold = defaults.SignReconcileDeviationThreshold
	}
	return in
}

func validZhipuSignClientVersion(version string) bool {
	return version != "" &&
		len(version) <= zhipuSignClientVersionMaxLength &&
		zhipuSignClientVersionPattern.MatchString(version)
}

// Effective 返回当前生效配置（运行层覆盖 > 部署层 > 协议默认）。签名热路径直接调用它：
// 命中进程内缓存时只有一次原子读，未命中时一次 GetMultiple。
func (s *ZhipuSignConfigService) Effective(ctx context.Context) ZhipuSignConfig {
	return s.ConfigView(ctx).ZhipuSignConfig
}

// ConfigView 返回生效配置与运行期覆盖键列表（管理面 GET 的响应体）。
func (s *ZhipuSignConfigService) ConfigView(ctx context.Context) ZhipuSignConfigView {
	if s == nil {
		return ZhipuSignConfigView{ZhipuSignConfig: zhipuSignProtocolDefaults()}
	}
	snapshot := s.snapshot(ctx)
	overrides := snapshot.overrides
	if overrides == nil {
		// 保证 JSON 是 [] 而不是 null：前端按数组渲染「已修改」标记。
		overrides = []string{}
	}
	return ZhipuSignConfigView{
		ZhipuSignConfig: snapshot.config,
		OverriddenKeys:  overrides,
	}
}

// snapshot 返回缓存的生效值快照，过期或不存在时从 system_settings 重读。
func (s *ZhipuSignConfigService) snapshot(ctx context.Context) zhipuSignConfigSnapshot {
	s.mu.Lock()
	if s.cached != nil && !s.now().After(s.cached.expiresAt) {
		cached := *s.cached
		cached.overrides = append([]string(nil), s.cached.overrides...)
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()
	return s.refresh(ctx)
}

// refresh 重读运行层覆盖并组装生效值。
//
// DB 故障时回落到部署层/协议默认，且**不写缓存**：一次抖动不会让覆盖值消失 30 秒
// （例如 sign_v4_enabled 被短暂改回打开），下一个请求立刻重试。
func (s *ZhipuSignConfigService) refresh(ctx context.Context) zhipuSignConfigSnapshot {
	defaults := s.defaults()
	if s.settingRepo == nil {
		return s.storeSnapshot(zhipuSignConfigSnapshot{config: defaults})
	}
	stored, err := s.settingRepo.GetMultiple(ctx, zhipuSignSettingKeys())
	if err != nil {
		return zhipuSignConfigSnapshot{config: defaults}
	}
	effective, overrides := applyZhipuSignOverrides(defaults, stored)
	return s.storeSnapshot(zhipuSignConfigSnapshot{
		config:    effective,
		overrides: overrides,
	})
}

func (s *ZhipuSignConfigService) storeSnapshot(snapshot zhipuSignConfigSnapshot) zhipuSignConfigSnapshot {
	snapshot.expiresAt = s.now().Add(zhipuSignConfigCacheTTL)
	s.mu.Lock()
	s.cached = &snapshot
	s.mu.Unlock()
	return snapshot
}

// invalidateCache 丢弃缓存（Update 的写穿语义：下一次读取必然回源 DB）。
func (s *ZhipuSignConfigService) invalidateCache() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

// applyZhipuSignOverrides 把运行层覆盖叠加到部署层默认值上，并返回真正生效的覆盖键
// （排序，保证响应稳定）。存量非法值（直接改库等绕过校验的写入）按「该键未覆盖」
// 处理：生效值退回默认，且不改写 DB。
func applyZhipuSignOverrides(defaults ZhipuSignConfig, stored map[string]string) (ZhipuSignConfig, []string) {
	config := defaults
	overridden := make([]string, 0, len(stored))
	record := func(key string) {
		overridden = append(overridden, key)
	}

	if raw, ok := stored[SettingKeyZhipuSignV4Enabled]; ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			config.SignV4Enabled = parsed
			record(SettingKeyZhipuSignV4Enabled)
		}
	}
	if raw, ok := stored[SettingKeyZhipuSignClientVersion]; ok {
		if version := strings.TrimSpace(raw); validZhipuSignClientVersion(version) {
			config.SignClientVersion = version
			record(SettingKeyZhipuSignClientVersion)
		}
	}
	if parsed, ok := zhipuSignStoredInt(stored, SettingKeyZhipuSignPowBits); ok && parsed >= 0 && parsed <= zhipuSignPowBitsMax {
		config.SignPowBits = parsed
		record(SettingKeyZhipuSignPowBits)
	}
	if parsed, ok := zhipuSignStoredInt(stored, SettingKeyZhipuSignKeyTTLMinutes); ok && parsed >= 1 && parsed <= zhipuSignKeyTTLMinutesMax {
		config.SignKeyTTLMinutes = parsed
		record(SettingKeyZhipuSignKeyTTLMinutes)
	}
	if parsed, ok := zhipuSignStoredInt(stored, SettingKeyZhipuSignHandshakeBackoffSeconds); ok && parsed >= 0 && parsed <= zhipuSignHandshakeBackoffMaxSecond {
		config.SignHandshakeBackoffSeconds = parsed
		record(SettingKeyZhipuSignHandshakeBackoffSeconds)
	}
	if parsed, ok := zhipuSignStoredInt(stored, SettingKeyZhipuSignAccountCircuitBreakThreshold); ok && parsed >= 1 && parsed <= zhipuSignCircuitBreakThresholdMax {
		config.SignAccountCircuitBreakThreshold = parsed
		record(SettingKeyZhipuSignAccountCircuitBreakThreshold)
	}
	if raw, ok := stored[SettingKeyZhipuSignFailPolicy]; ok {
		switch policy := strings.ToLower(strings.TrimSpace(raw)); policy {
		case ZhipuSignFailPolicyOpen, ZhipuSignFailPolicyClosed:
			config.SignFailPolicy = policy
			record(SettingKeyZhipuSignFailPolicy)
		}
	}
	if raw, ok := stored[SettingKeyZhipuSignAlertEnabled]; ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			config.SignAlertEnabled = parsed
			record(SettingKeyZhipuSignAlertEnabled)
		}
	}
	if parsed, ok := zhipuSignStoredInt(stored, SettingKeyZhipuSignReconcileIntervalHours); ok && parsed >= 1 && parsed <= zhipuSignReconcileIntervalHoursMax {
		config.SignReconcileIntervalHours = parsed
		record(SettingKeyZhipuSignReconcileIntervalHours)
	}
	if raw, ok := stored[SettingKeyZhipuSignReconcileDeviationThreshold]; ok {
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && parsed > 0 && parsed <= 1 {
			config.SignReconcileDeviationThreshold = parsed
			record(SettingKeyZhipuSignReconcileDeviationThreshold)
		}
	}
	sort.Strings(overridden)
	return config, overridden
}

func zhipuSignStoredInt(stored map[string]string, key string) (int, bool) {
	raw, ok := stored[key]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// zhipuSignUpdateKeysList 是空请求错误里列出的全部可更新键（前端与文档的键清单事实源）。
const zhipuSignUpdateKeysList = "sign_v4_enabled, sign_client_version, sign_pow_bits, " +
	"sign_key_ttl_minutes, sign_handshake_backoff_seconds, sign_account_circuit_break_threshold, " +
	"sign_fail_policy, sign_alert_enabled, sign_reconcile_interval_hours, sign_reconcile_deviation_threshold"

// settingsValues 把部分更新请求转成 system_settings 的键值对，并在转换时完成校验：
// 任何非法值都让整次更新失败（不做「写一半」），保证磁盘上不会出现半合法的配置。
func (u ZhipuSignConfigUpdate) settingsValues() (map[string]string, error) {
	values := make(map[string]string, 10)
	if u.SignV4Enabled != nil {
		values[SettingKeyZhipuSignV4Enabled] = strconv.FormatBool(*u.SignV4Enabled)
	}
	if u.SignClientVersion != nil {
		version := strings.TrimSpace(*u.SignClientVersion)
		if !validZhipuSignClientVersion(version) {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_client_version must be a semver-like string such as 0.16.9 (at most %d characters), got %q",
				zhipuSignClientVersionMaxLength, version))
		}
		values[SettingKeyZhipuSignClientVersion] = version
	}
	if u.SignPowBits != nil {
		if *u.SignPowBits < 0 || *u.SignPowBits > zhipuSignPowBitsMax {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_pow_bits must be an integer in [0,%d], got %d", zhipuSignPowBitsMax, *u.SignPowBits))
		}
		values[SettingKeyZhipuSignPowBits] = strconv.Itoa(*u.SignPowBits)
	}
	if u.SignKeyTTLMinutes != nil {
		if *u.SignKeyTTLMinutes < 1 || *u.SignKeyTTLMinutes > zhipuSignKeyTTLMinutesMax {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_key_ttl_minutes must be an integer in [1,%d], got %d", zhipuSignKeyTTLMinutesMax, *u.SignKeyTTLMinutes))
		}
		values[SettingKeyZhipuSignKeyTTLMinutes] = strconv.Itoa(*u.SignKeyTTLMinutes)
	}
	if u.SignHandshakeBackoffSeconds != nil {
		if *u.SignHandshakeBackoffSeconds < 0 || *u.SignHandshakeBackoffSeconds > zhipuSignHandshakeBackoffMaxSecond {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_handshake_backoff_seconds must be an integer in [0,%d], got %d",
				zhipuSignHandshakeBackoffMaxSecond, *u.SignHandshakeBackoffSeconds))
		}
		values[SettingKeyZhipuSignHandshakeBackoffSeconds] = strconv.Itoa(*u.SignHandshakeBackoffSeconds)
	}
	if u.SignAccountCircuitBreakThreshold != nil {
		if *u.SignAccountCircuitBreakThreshold < 1 || *u.SignAccountCircuitBreakThreshold > zhipuSignCircuitBreakThresholdMax {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_account_circuit_break_threshold must be an integer in [1,%d], got %d",
				zhipuSignCircuitBreakThresholdMax, *u.SignAccountCircuitBreakThreshold))
		}
		values[SettingKeyZhipuSignAccountCircuitBreakThreshold] = strconv.Itoa(*u.SignAccountCircuitBreakThreshold)
	}
	if u.SignFailPolicy != nil {
		policy := strings.ToLower(strings.TrimSpace(*u.SignFailPolicy))
		if policy != ZhipuSignFailPolicyOpen && policy != ZhipuSignFailPolicyClosed {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_fail_policy must be one of [%s %s], got %q",
				ZhipuSignFailPolicyOpen, ZhipuSignFailPolicyClosed, *u.SignFailPolicy))
		}
		values[SettingKeyZhipuSignFailPolicy] = policy
	}
	if u.SignAlertEnabled != nil {
		values[SettingKeyZhipuSignAlertEnabled] = strconv.FormatBool(*u.SignAlertEnabled)
	}
	if u.SignReconcileIntervalHours != nil {
		if *u.SignReconcileIntervalHours < 1 || *u.SignReconcileIntervalHours > zhipuSignReconcileIntervalHoursMax {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_reconcile_interval_hours must be an integer in [1,%d], got %d",
				zhipuSignReconcileIntervalHoursMax, *u.SignReconcileIntervalHours))
		}
		values[SettingKeyZhipuSignReconcileIntervalHours] = strconv.Itoa(*u.SignReconcileIntervalHours)
	}
	if u.SignReconcileDeviationThreshold != nil {
		if *u.SignReconcileDeviationThreshold <= 0 || *u.SignReconcileDeviationThreshold > 1 {
			return nil, zhipuSignConfigInvalid(fmt.Sprintf(
				"sign_reconcile_deviation_threshold must be a number in (0,1], got %s",
				strconv.FormatFloat(*u.SignReconcileDeviationThreshold, 'f', -1, 64)))
		}
		values[SettingKeyZhipuSignReconcileDeviationThreshold] = strconv.FormatFloat(*u.SignReconcileDeviationThreshold, 'f', -1, 64)
	}
	if len(values) == 0 {
		return nil, infraerrors.BadRequest(ZhipuSignConfigReasonEmpty,
			"no zhipu sign config key supplied; supply at least one of: "+zhipuSignUpdateKeysList)
	}
	return values, nil
}

// Update 校验并持久化管理端的配置修改（design M3.1(e)：协议漂移只能由管理员显式修改，
// 变更自身由管理面审计中间件留痕；不存在任何自动跟随官方版本的路径）。
//
// 写穿语义：校验通过 → SetMultiple → 立即失效进程内缓存 → 推送签名器 SetOptions，
// 因此同一实例的下一个 Sign 必然使用新值，不存在重启或缓存旧值的窗口。
func (s *ZhipuSignConfigService) Update(ctx context.Context, update ZhipuSignConfigUpdate) (ZhipuSignConfigView, error) {
	values, err := update.settingsValues()
	if err != nil {
		return ZhipuSignConfigView{}, err
	}
	if s == nil || s.settingRepo == nil {
		return ZhipuSignConfigView{}, infraerrors.InternalServer(ZhipuSignConfigReasonUnavailable,
			"system settings storage is not available; zhipu sign config cannot be persisted")
	}
	if err := s.settingRepo.SetMultiple(ctx, values); err != nil {
		return ZhipuSignConfigView{}, fmt.Errorf("update zhipu sign config: %w", err)
	}
	s.invalidateCache()
	view := s.ConfigView(ctx)
	// 立即推送到签名器：这是「改配置 → 下一个请求生效」的最后一步。
	s.ApplyToSigner(ctx)
	return view, nil
}

// ApplyToSigner 把生效配置推送给 #20 的 Signer（SetOptions），仅在生效值变化时调用。
// 签名热路径（热更新装饰器）每个请求都会调用它，因此「管理端改配置 → 下一个签名请求
// 生效」既不依赖重启，也不依赖后台轮询。
func (s *ZhipuSignConfigService) ApplyToSigner(ctx context.Context) {
	if s == nil || s.runtime == nil {
		return
	}
	effective := s.Effective(ctx)
	options := zcodesign.SignerOptions{
		ClientVersion:    effective.SignClientVersion,
		KeyTTL:           time.Duration(effective.SignKeyTTLMinutes) * time.Minute,
		PowBits:          effective.SignPowBits,
		HandshakeBackoff: time.Duration(effective.SignHandshakeBackoffSeconds) * time.Second,
	}

	s.mu.Lock()
	if s.appliedSet && s.applied == options {
		s.mu.Unlock()
		return
	}
	s.applied, s.appliedSet = options, true
	s.mu.Unlock()

	s.runtime.SetOptions(options)
}

// zhipuSignHotReloadSigner 是 #20 签名器的配置热更新装饰器（票 28）：每次 Sign 前把
// 生效配置推给内层 Signer（仅在值变化时才 SetOptions），使管理端修改在下一个签名请求
// 生效。它不改变签名语义：Sign 与 Invalidate 一律直接委托，#22 的注入点与 #23 的
// 自愈路径无需任何改动。
type zhipuSignHotReloadSigner struct {
	inner  zhipuClientSigner
	config *ZhipuSignConfigService
}

// Sign 先同步生效配置，再按 #20 的语义签名。
func (s *zhipuSignHotReloadSigner) Sign(ctx context.Context, apiKey, sessionID string, h http.Header) error {
	s.config.ApplyToSigner(ctx)
	return s.inner.Sign(ctx, apiKey, sessionID, h)
}

// Invalidate 直接委托：#20 的私钥作废语义不因配置热更新而改变。
func (s *zhipuSignHotReloadSigner) Invalidate(apiKeyID string) {
	s.inner.Invalidate(apiKeyID)
}

// ProvideZhipuSignRuntime 装配签名配置面与热更新装饰器（票 28）。
//
// 返回的装饰器签名器替换 ProvideZhipuClientSigner 的返回值注入网关（cmd/server/wire_gen.go），
// 于是「配置热更新」落在签名器接缝上：#22/#23 的文件一行未改。
//
// signer 未提供（nil）或不是 #20 的 *zcodesign.Signer（拿不到 SetOptions/KeyStatus 的
// 替身）时，装饰器退化为原样透传：与管理端配置能不能读写成败无关，保持「零行为变化」。
func ProvideZhipuSignRuntime(
	cfg *config.Config,
	settingRepo SettingRepository,
	accountRepo zhipuSignAccountLister,
	signer zhipuClientSigner,
) (*ZhipuSignConfigService, zhipuClientSigner) {
	runtime, wired := signer.(zhipuSignRuntime)
	svc := NewZhipuSignConfigService(cfg, settingRepo, accountRepo, runtime)
	if !wired || signer == nil {
		return svc, signer
	}
	return svc, &zhipuSignHotReloadSigner{inner: signer, config: svc}
}
