package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 平台级功能设置（A4 批 4.0）。
//
// 背景：签到、保活之类的"平台级功能"需要按平台分别开关。每个平台的开关由该平台
// 自己的代码注册进来，设置页聚合展示，而不是把 codebuddy 的名字写进设置结构里
// ——后者每加一个平台就要改一遍设置层。
//
// 持久化复用既有 settings 通道（SettingRepository，JSON blob 单键），与
// SettingKeyDefaultPlatformQuotas（map[platform]...）同款形态，不新建表。
//
// 分层：本文件只做"注册表 + 归一化 + 校验"，不含任何 codebuddy 逻辑。具体平台的
// 默认值由各平台在 init/构造时通过 RegisterPlatformFeatureSpec 注册。

// PlatformFeatureKind 描述一个功能开关的取值形态。设置页据此渲染对应控件。
type PlatformFeatureKind string

const (
	// PlatformFeatureBool 开关（设置页渲染为 toggle）。
	PlatformFeatureBool PlatformFeatureKind = "bool"
	// PlatformFeatureTimeRange 时间段（设置页渲染为两个 HH:MM 输入）。
	PlatformFeatureTimeRange PlatformFeatureKind = "time_range"
)

// PlatformFeatureStorageKey 是平台功能设置在 settings 表中的单键名。
const PlatformFeatureStorageKey = "platform_features"

// TimeOfDay 一天中的某个时刻（HH:MM，所属时区由功能自己声明）。
type TimeOfDay struct {
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
}

// Format 渲染成 HH:MM（设置页展示 / 日志用）。
func (t TimeOfDay) Format() string {
	return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute)
}

// Minutes 返回当日分钟偏移，供窗口判定使用。
func (t TimeOfDay) Minutes() int { return t.Hour*60 + t.Minute }

// ParseTimeOfDay 解析 "HH:MM"。非法输入返回 ok=false（调用方据此保留默认值）。
func ParseTimeOfDay(raw string) (TimeOfDay, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return TimeOfDay{}, false
	}
	hour, err := parseBoundedInt(parts[0], 0, 23)
	if err != nil {
		return TimeOfDay{}, false
	}
	minute, err := parseBoundedInt(parts[1], 0, 59)
	if err != nil {
		return TimeOfDay{}, false
	}
	return TimeOfDay{Hour: hour, Minute: minute}, true
}

func parseBoundedInt(raw string, min, max int) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty")
	}
	value := 0
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric")
		}
		value = value*10 + int(r-'0')
		if value > max {
			return 0, fmt.Errorf("out of range")
		}
	}
	if value < min {
		return 0, fmt.Errorf("out of range")
	}
	return value, nil
}

// PlatformFeatureDefinition 一个平台的平台级功能声明（各平台注册）。
type PlatformFeatureDefinition struct {
	// Key 功能标识（平台内唯一），如 "checkin"。
	Key string
	// Kind 取值形态，决定设置页控件与归一化方式。
	Kind PlatformFeatureKind
	// Title / Description 供设置页直接展示的文案（可留空，前端按 Key 兜底 i18n）。
	// 放在注册声明里而不是只靠前端 i18n：新平台注册一个功能后设置页无需改前端
	// 就能显示出来，这才是「基础设施容纳多平台」的实际含义。
	Title       string
	Description string
	// EnabledByDefault 缺省是否开启。
	// 签到这类"对外写操作"必须 false：没显式开启就不该打上游。
	EnabledByDefault bool
	// Timezone 时间段语义所属时区（仅 time_range 有意义）。
	Timezone *time.Location
	// DefaultStart / DefaultEnd 时间段默认值（仅 time_range 有意义）。
	DefaultStart TimeOfDay
	DefaultEnd   TimeOfDay
}

// PlatformFeatureSpec 平台在注册表里的归属：平台名 + 该平台的功能清单。
type PlatformFeatureSpec struct {
	Platform string
	Features []PlatformFeatureDefinition
}

// PlatformFeatureValue 落库形态的单个功能取值。
// 只有 Kind 对应的字段会被写入/读取，其余字段保持零值。
type PlatformFeatureValue struct {
	Enabled bool       `json:"enabled"`
	Start   *TimeOfDay `json:"start,omitempty"`
	End     *TimeOfDay `json:"end,omitempty"`
}

// PlatformFeatureSettings 落库文档：map[platform]map[featureKey]value。
// 外层 map 让"没配过的平台"自然缺省，不需要为每个平台都写一条。
type PlatformFeatureSettings map[string]map[string]PlatformFeatureValue

var platformFeatureRegistry = map[string][]PlatformFeatureDefinition{}

// RegisterPlatformFeatureSpec 由平台包注册自己的功能声明。
//
// 重复注册同一平台会 panic：这是编译期就能暴露的编程错误（两个包抢同一个平台名），
// 静默覆盖会让其中一个平台的功能凭空消失。各平台在自己的 init 里调用。
func RegisterPlatformFeatureSpec(spec PlatformFeatureSpec) {
	platform := strings.TrimSpace(spec.Platform)
	if platform == "" {
		panic("RegisterPlatformFeatureSpec: empty platform")
	}
	if _, exists := platformFeatureRegistry[platform]; exists {
		panic("RegisterPlatformFeatureSpec: duplicate platform " + platform)
	}
	seen := map[string]struct{}{}
	features := make([]PlatformFeatureDefinition, 0, len(spec.Features))
	for _, feature := range spec.Features {
		key := strings.TrimSpace(feature.Key)
		if key == "" {
			panic("RegisterPlatformFeatureSpec: empty feature key for platform " + platform)
		}
		if _, dup := seen[key]; dup {
			panic("RegisterPlatformFeatureSpec: duplicate feature " + key + " for platform " + platform)
		}
		seen[key] = struct{}{}
		feature.Key = key
		features = append(features, feature)
	}
	platformFeatureRegistry[platform] = features
}

// PlatformFeatureDefinitions 返回指定平台的已注册功能（未注册平台返回 nil）。
func PlatformFeatureDefinitions(platform string) []PlatformFeatureDefinition {
	return platformFeatureRegistry[strings.TrimSpace(platform)]
}

// RegisteredPlatformFeatureSpecs 返回全部已注册平台（按平台名排序，便于稳定展示）。
func RegisteredPlatformFeatureSpecs() []PlatformFeatureSpec {
	platforms := make([]string, 0, len(platformFeatureRegistry))
	for platform := range platformFeatureRegistry {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)

	specs := make([]PlatformFeatureSpec, 0, len(platforms))
	for _, platform := range platforms {
		specs = append(specs, PlatformFeatureSpec{
			Platform: platform,
			Features: platformFeatureRegistry[platform],
		})
	}
	return specs
}

// UnregisterPlatformFeatureSpec 撤销注册（仅测试使用：注册表是包级状态，
// 测试之间必须能还原，否则同平台重复注册会 panic 到下一个测试）。
func UnregisterPlatformFeatureSpec(platform string) {
	delete(platformFeatureRegistry, strings.TrimSpace(platform))
}

// NormalizePlatformFeatureSettings 把任意来源的文档归一化成落库形态：
//   - 未注册的平台 / 未声明功能：丢弃（注册表是唯一权威，避免残留脏键被设置页读出来）
//   - time_range：缺失或非法的时间点退回注册声明的默认值；
//     start == end 视为无意义的零长窗口 → 同样退回默认（否则"窗口内永不触发"会
//     表现为功能静默失效，比报错更难查）
//   - bool：只保留 Enabled
func NormalizePlatformFeatureSettings(raw PlatformFeatureSettings) PlatformFeatureSettings {
	if len(raw) == 0 {
		return PlatformFeatureSettings{}
	}
	out := PlatformFeatureSettings{}
	for platform, features := range raw {
		definitions := PlatformFeatureDefinitions(platform)
		if len(definitions) == 0 || len(features) == 0 {
			continue
		}
		normalized := map[string]PlatformFeatureValue{}
		for _, definition := range definitions {
			value, ok := features[definition.Key]
			if !ok {
				continue
			}
			normalized[definition.Key] = normalizePlatformFeatureValue(definition, value)
		}
		if len(normalized) > 0 {
			out[platform] = normalized
		}
	}
	return out
}

func normalizePlatformFeatureValue(definition PlatformFeatureDefinition, value PlatformFeatureValue) PlatformFeatureValue {
	out := PlatformFeatureValue{Enabled: value.Enabled}
	if definition.Kind != PlatformFeatureTimeRange {
		return out
	}

	start := definition.DefaultStart
	if value.Start != nil && validTimeOfDay(*value.Start) {
		start = *value.Start
	}
	end := definition.DefaultEnd
	if value.End != nil && validTimeOfDay(*value.End) {
		end = *value.End
	}
	if start.Minutes() == end.Minutes() {
		// 零长窗口：退回默认区间，别让"功能开着但永不触发"这种静默失效上线。
		start = definition.DefaultStart
		end = definition.DefaultEnd
	}
	out.Start = &start
	out.End = &end
	return out
}

func validTimeOfDay(value TimeOfDay) bool {
	return value.Hour >= 0 && value.Hour <= 23 && value.Minute >= 0 && value.Minute <= 59
}

// MarshalPlatformFeatureSettings 序列化为 settings 表单键文本。
func MarshalPlatformFeatureSettings(settings PlatformFeatureSettings) (string, error) {
	normalized := NormalizePlatformFeatureSettings(settings)
	blob, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal platform feature settings: %w", err)
	}
	return string(blob), nil
}

// ParsePlatformFeatureSettings 解析 settings 表单键文本。
// 空串 = 从未配置 → 返回 nil（调用方回落到注册声明的默认值）。
func ParsePlatformFeatureSettings(raw string) PlatformFeatureSettings {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	parsed := PlatformFeatureSettings{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil
	}
	return parsed
}

// --- 管理面读写（A4 批 4.0 的 HTTP 端点直接用这两条）---

// PlatformFeatureReadItem 一个功能项及其**当前生效值**（已按注册声明补齐默认）。
type PlatformFeatureReadItem struct {
	Key         string
	Kind        PlatformFeatureKind
	Title       string
	Description string
	Value       PlatformFeatureValue
}

// PlatformFeatureReadGroup 一个平台的分组。
type PlatformFeatureReadGroup struct {
	Platform string
	Features []PlatformFeatureReadItem
}

// GetPlatformFeatureGroups 读取全部已注册平台的功能项及其生效值。
//
// 值一律经 platformFeatureLookup 补齐（缺省→注册默认），所以调用方拿到的
// 永远是可直接渲染/使用的完整值，不需要自己判空或再查注册表。
func (s *SettingService) GetPlatformFeatureGroups(ctx context.Context) ([]PlatformFeatureReadGroup, error) {
	if s == nil {
		return nil, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, PlatformFeatureStorageKey)
	if err != nil {
		// 未配置 / 读取失败 → 空文档：生效值全部落到注册声明默认（fail-closed：
		// 写操作类功能默认关闭，读失败不能把它意外打开）。
		raw = ""
	}
	settings := ParsePlatformFeatureSettings(raw)

	groups := make([]PlatformFeatureReadGroup, 0)
	for _, spec := range RegisteredPlatformFeatureSpecs() {
		group := PlatformFeatureReadGroup{Platform: spec.Platform}
		for _, definition := range spec.Features {
			_, value, ok := platformFeatureLookup(settings, spec.Platform, definition.Key)
			if !ok {
				continue
			}
			group.Features = append(group.Features, PlatformFeatureReadItem{
				Key:         definition.Key,
				Kind:        definition.Kind,
				Title:       definition.Title,
				Description: definition.Description,
				Value:       value,
			})
		}
		if len(group.Features) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

// PlatformFeatureUpdate 一次稀疏写入里的单项（整值替换该功能的取值）。
type PlatformFeatureUpdate struct {
	Platform string
	Key      string
	Value    PlatformFeatureValue
}

// UpdatePlatformFeatures 稀疏写入若干平台功能项：
//   - 只覆盖本次提交的项，其余平台/功能项的既有值原样保留；
//   - 未知平台或未声明功能 → 报错且**整次写不做**（不落半套配置）；
//   - 落库前整文档过 NormalizePlatformFeatureSettings：越界时间点退回注册默认、
//     零长窗口修正、未注册键丢弃。
func (s *SettingService) UpdatePlatformFeatures(ctx context.Context, updates []PlatformFeatureUpdate) error {
	if s == nil || len(updates) == 0 {
		return nil
	}
	for _, update := range updates {
		definitions := PlatformFeatureDefinitions(update.Platform)
		if len(definitions) == 0 {
			return fmt.Errorf("unknown platform %q", update.Platform)
		}
		found := false
		for _, definition := range definitions {
			if definition.Key == update.Key {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown platform feature %s/%s", update.Platform, update.Key)
		}
	}

	raw, err := s.settingRepo.GetValue(ctx, PlatformFeatureStorageKey)
	if err != nil {
		raw = "" // 未配置视为空文档（读改写：只加本次提交的项）
	}
	settings := ParsePlatformFeatureSettings(raw)
	if settings == nil {
		settings = PlatformFeatureSettings{}
	}
	for _, update := range updates {
		if settings[update.Platform] == nil {
			settings[update.Platform] = map[string]PlatformFeatureValue{}
		}
		value := update.Value
		// time_range 的时间点跟随注册默认补齐，避免只提交 enabled 时把窗口清空。
		for _, definition := range PlatformFeatureDefinitions(update.Platform) {
			if definition.Key != update.Key || definition.Kind != PlatformFeatureTimeRange {
				continue
			}
			if value.Start == nil {
				start := definition.DefaultStart
				value.Start = &start
			}
			if value.End == nil {
				end := definition.DefaultEnd
				value.End = &end
			}
		}
		settings[update.Platform][update.Key] = value
	}

	blob, err := MarshalPlatformFeatureSettings(settings)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, PlatformFeatureStorageKey, blob)
}

// platformFeatureLookup 从一份已解析文档里取某平台某功能的取值。
func platformFeatureLookup(
	settings PlatformFeatureSettings,
	platform string,
	featureKey string,
) (PlatformFeatureDefinition, PlatformFeatureValue, bool) {
	var definition PlatformFeatureDefinition
	found := false
	for _, candidate := range PlatformFeatureDefinitions(platform) {
		if candidate.Key == featureKey {
			definition = candidate
			found = true
			break
		}
	}
	if !found {
		return PlatformFeatureDefinition{}, PlatformFeatureValue{}, false
	}

	value := PlatformFeatureValue{Enabled: definition.EnabledByDefault}
	if features, ok := settings[platform]; ok {
		if stored, ok := features[featureKey]; ok {
			value = stored
		}
	}
	// 时间点跟随注册默认值补齐，调用方不需要再判空。
	if definition.Kind == PlatformFeatureTimeRange {
		start := definition.DefaultStart
		if value.Start != nil && validTimeOfDay(*value.Start) {
			start = *value.Start
		}
		end := definition.DefaultEnd
		if value.End != nil && validTimeOfDay(*value.End) {
			end = *value.End
		}
		value.Start = &start
		value.End = &end
	}
	return definition, value, true
}

// PlatformFeatureEnabled 报告某平台某功能是否开启（未注册功能恒 false）。
func PlatformFeatureEnabled(settings PlatformFeatureSettings, platform, featureKey string) bool {
	_, value, ok := platformFeatureLookup(settings, platform, featureKey)
	if !ok {
		return false
	}
	return value.Enabled
}

// ResolvePlatformFeatureTimeRange 读取某平台某功能的时间段与其所属时区。
// 非 time_range 功能返回 ok=false。
func ResolvePlatformFeatureTimeRange(
	settings PlatformFeatureSettings,
	platform string,
	featureKey string,
) (start, end TimeOfDay, location *time.Location, ok bool) {
	definition, value, found := platformFeatureLookup(settings, platform, featureKey)
	if !found || definition.Kind != PlatformFeatureTimeRange {
		return TimeOfDay{}, TimeOfDay{}, nil, false
	}
	location = definition.Timezone
	if location == nil {
		location = time.UTC
	}
	if value.Start == nil || value.End == nil {
		return TimeOfDay{}, TimeOfDay{}, location, false
	}
	return *value.Start, *value.End, location, true
}

// WithinTimeRange 判断 now 是否落在 [start, end) 窗口内（按 location 换算当地时刻）。
//
// 支持跨零点窗口（如 23:00–01:00）：起止分钟逆序时按"跨天"理解，否则这类配置会
// 变成一个永不命中的空窗。
func WithinTimeRange(now time.Time, start, end TimeOfDay, location *time.Location) bool {
	if location == nil {
		location = time.UTC
	}
	local := now.In(location)
	minutes := local.Hour()*60 + local.Minute()
	startMinutes := start.Minutes()
	endMinutes := end.Minutes()

	if startMinutes == endMinutes {
		// 零长窗口：不触发（归一化已把它挡在落库之外，这里是读取侧的兜底）。
		return false
	}
	if startMinutes < endMinutes {
		return minutes >= startMinutes && minutes < endMinutes
	}
	// 跨零点：窗口是 [start, 24:00) ∪ [00:00, end)。
	return minutes >= startMinutes || minutes < endMinutes
}

// PlatformFeatureLocalDate 返回 now 在指定时区下的当地日期（"当日"判定用）。
// 签到这类"每日一次"的功能必须按当地日期判重，用 UTC 日期会在 UTC+8 的早上
// 出现跨日错位。
func PlatformFeatureLocalDate(now time.Time, location *time.Location) time.Time {
	if location == nil {
		location = time.UTC
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}
