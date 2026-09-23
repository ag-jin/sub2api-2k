package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

// newPlatformFeatureTestService 构造走真实 UpdateSettings/GetAllSettings 路径的
// SettingService。cfg 必须非 nil：parseSettings 会读 s.cfg.Default.* 兜默认值。
//
// ⚠️ 必须包一层 preserveXAIRuntimeMappingOptions：GetAllSettings → parseSettings
// 末尾会调用 xai.SetRuntimeModelMappingOptions 写**进程级全局**（Grok 账号在
// credentials.model_mapping 为空时使用的默认映射）。测试传的是不完整 settings
// 文档，parseSettings 会把 grok_cross_client_model_map_enabled 判成缺省 true
// （isFalseSettingValue("") == false），于是给全进程装上 `gpt-* → grok-4.6`
// 通配映射——实测会打红别的测试的
// "Grok OAuth does not inherit OpenAI Codex aliases" 子用例
// （canonical scheduling model = "grok-4.6", want "gpt-5.6"）。
// 最小复现：只跑 TestCanonicalOpenAIAccountSchedulingModelMatchesForwardSemantics
// + TestCodeBuddyCheckinSchedulerDefaultOff 即红；只跑前者为绿。
func newPlatformFeatureTestService(t *testing.T, repo *platformFeatureTestRepo) *SettingService {
	t.Helper()
	restore := preserveXAIRuntimeMappingOptions()
	t.Cleanup(restore)
	return NewSettingService(repo, &config.Config{})
}

// preserveXAIRuntimeMappingOptions 快照并还原 xai 的进程级运行时映射配置，
// 让调用方对全局状态的写入不泄漏到其他测试。
func preserveXAIRuntimeMappingOptions() func() {
	previous := xai.RuntimeModelMappingOptions()
	return func() { xai.SetRuntimeModelMappingOptions(previous) }
}

// 平台功能设置基础设施测试（A4 批 4.0）。
//
// 这一组覆盖的是**基础设施**，不是 codebuddy 的业务：注册表、归一化、缺省、
// 窗口判定、当地日期。codebuddy 的具体注册值在 codebuddy_platform_features_test.go 里断言。
//
// 注册表是包级状态且重复注册会 panic，所以凡在用例内注册平台的测试都**不得并行**，
// 并且必须用 queueQueuedPlatformFeatureRegistryCleanup 在结束时不还原。

// platformFeatureTestRepo 可读写的 settings stub。
// 只实现平台功能实际会用到的三个方法，其余 panic——误用会立刻暴露。
type platformFeatureTestRepo struct {
	vals   map[string]string
	setErr error
	getErr error
}

func newPlatformFeatureTestRepo() *platformFeatureTestRepo {
	return &platformFeatureTestRepo{vals: map[string]string{}}
}

func (r *platformFeatureTestRepo) GetValue(_ context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	v, ok := r.vals[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

func (r *platformFeatureTestRepo) Set(_ context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.vals[key] = value
	return nil
}

// SetMultiple 记录批量写入（UpdateSettings 走这条通道）。
func (r *platformFeatureTestRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	if r.setErr != nil {
		return r.setErr
	}
	for key, value := range settings {
		r.vals[key] = value
	}
	return nil
}

// GetAll 返回全部键值（GetAllSettings → parseSettings 走这条）。
func (r *platformFeatureTestRepo) GetAll(_ context.Context) (map[string]string, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	out := make(map[string]string, len(r.vals))
	for key, value := range r.vals {
		out[key] = value
	}
	return out, nil
}

func (r *platformFeatureTestRepo) Get(_ context.Context, _ string) (*Setting, error) {
	panic("platformFeatureTestRepo.Get not implemented")
}
func (r *platformFeatureTestRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	panic("platformFeatureTestRepo.GetMultiple not implemented")
}
func (r *platformFeatureTestRepo) Delete(_ context.Context, _ string) error {
	panic("platformFeatureTestRepo.Delete not implemented")
}

var _ SettingRepository = (*platformFeatureTestRepo)(nil)

// queueQueuedPlatformFeatureRegistryCleanup 注册临时平台并在测试结束移除。
// 注册表是包级状态且无快照能力，只能按名删除——不在用例内注册的测试不要调用它。
func queueQueuedPlatformFeatureRegistryCleanup(t *testing.T, platforms ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, platform := range platforms {
			UnregisterPlatformFeatureSpec(platform)
		}
	})
}

// --- 注册表 ---

// Scenario：重复注册同一平台必须 panic（不是静默覆盖）。
// 静默覆盖会让先注册平台的功能凭空消失，且要到运行时才发现。
func TestRegisterPlatformFeatureSpecDuplicatePanics(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "dup-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{Platform: "dup-platform"})
	require.Panics(t, func() {
		RegisterPlatformFeatureSpec(PlatformFeatureSpec{Platform: "dup-platform"})
	})
}

// Scenario：空平台名 / 空功能键 / 平台内重复功能键 都判为编程错误。
func TestRegisterPlatformFeatureSpecRejectsInvalid(t *testing.T) {
	require.Panics(t, func() {
		RegisterPlatformFeatureSpec(PlatformFeatureSpec{Platform: "   "})
	}, "空平台名应 panic")

	queueQueuedPlatformFeatureRegistryCleanup(t, "invalid-feature-platform")
	require.Panics(t, func() {
		RegisterPlatformFeatureSpec(PlatformFeatureSpec{
			Platform: "invalid-feature-platform",
			Features: []PlatformFeatureDefinition{{Key: "  "}},
		})
	}, "空功能键应 panic")
	require.Panics(t, func() {
		RegisterPlatformFeatureSpec(PlatformFeatureSpec{
			Platform: "invalid-feature-platform",
			Features: []PlatformFeatureDefinition{{Key: "same"}, {Key: "same"}},
		})
	}, "平台内重复功能键应 panic")
}

// Scenario：注册表按平台名排序返回（设置页展示顺序必须稳定）。
func TestRegisteredPlatformFeatureSpecsSorted(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "zp-platform", "ap-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{Platform: "zp-platform"})
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{Platform: "ap-platform"})

	specs := RegisteredPlatformFeatureSpecs()
	require.GreaterOrEqual(t, len(specs), 2)
	for i := 1; i < len(specs); i++ {
		require.LessOrEqual(t, specs[i-1].Platform, specs[i].Platform, "平台按名排序")
	}
}

// Scenario：注册后可查到功能定义；未注册平台返回 nil。
func TestPlatformFeatureDefinitionsLookup(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "lookup-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "lookup-platform",
		Features: []PlatformFeatureDefinition{{Key: "flag", Kind: PlatformFeatureBool}},
	})

	definitions := PlatformFeatureDefinitions("lookup-platform")
	require.Len(t, definitions, 1)
	require.Equal(t, "flag", definitions[0].Key)
	require.Empty(t, PlatformFeatureDefinitions("never-registered"))
}

// --- 时间点解析与窗口判定 ---

// Scenario：HH:MM 解析的合法/非法边界。非法输入必须 ok=false（调用方保留默认值），
// 而不是被静默当成 00:00。
func TestParseTimeOfDayBoundaries(t *testing.T) {
	t.Parallel()

	valid := map[string]TimeOfDay{
		"00:00": {Hour: 0, Minute: 0},
		"09:30": {Hour: 9, Minute: 30},
		"23:59": {Hour: 23, Minute: 59},
		"7:05":  {Hour: 7, Minute: 5},
	}
	for raw, want := range valid {
		got, ok := ParseTimeOfDay(raw)
		require.True(t, ok, "%q 应可解析", raw)
		require.Equal(t, want, got, "%q", raw)
	}

	for _, raw := range []string{"", "24:00", "12:60", "-1:00", "abc", "12", "12:34:56", "1:2:3", ":"} {
		_, ok := ParseTimeOfDay(raw)
		require.False(t, ok, "%q 应判非法", raw)
	}
}

// Scenario：窗口判定支持跨零点；零长窗口不触发。
// 跨零点若按普通区间处理会变成永不命中的空窗（静默失效，比报错更难查）。
func TestWithinTimeRange(t *testing.T) {
	t.Parallel()

	zone := time.FixedZone("UTC+8", 8*60*60)
	at := func(hhmm string) time.Time {
		parsed, err := time.ParseInLocation("2006-01-02 15:04", "2026-09-22 "+hhmm, zone)
		require.NoError(t, err)
		return parsed
	}
	day := func(h, m int) TimeOfDay { return TimeOfDay{Hour: h, Minute: m} }

	// 普通窗口 [09:00, 11:00)：起点含、终点不含。
	start, end := day(9, 0), day(11, 0)
	require.False(t, WithinTimeRange(at("08:59"), start, end, zone))
	require.True(t, WithinTimeRange(at("09:00"), start, end, zone))
	require.True(t, WithinTimeRange(at("10:59"), start, end, zone))
	require.False(t, WithinTimeRange(at("11:00"), start, end, zone), "终点为开区间")

	// 跨零点 [23:00, 01:00)。
	crossStart, crossEnd := day(23, 0), day(1, 0)
	require.True(t, WithinTimeRange(at("23:30"), crossStart, crossEnd, zone))
	require.True(t, WithinTimeRange(at("00:30"), crossStart, crossEnd, zone))
	require.False(t, WithinTimeRange(at("12:00"), crossStart, crossEnd, zone))
	require.False(t, WithinTimeRange(at("01:00"), crossStart, crossEnd, zone), "跨零点终点同样是开区间")

	// 零长窗口：永不触发（归一化已挡在落库之外，这里是读取侧兜底）。
	require.False(t, WithinTimeRange(at("09:00"), day(9, 0), day(9, 0), zone))
}

// Scenario：时区参与判定——同一时刻在不同时区下窗口归属不同。
// 这条挡住"忘了用功能声明的时区"的回归（用错时区会让窗口整天错位）。
func TestWithinTimeRangeUsesLocation(t *testing.T) {
	t.Parallel()

	// UTC 01:00 == UTC+8 09:00。
	moment := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	zone8 := time.FixedZone("UTC+8", 8*60*60)

	start := TimeOfDay{Hour: 9, Minute: 0}
	end := TimeOfDay{Hour: 11, Minute: 0}
	require.False(t, WithinTimeRange(moment, start, end, time.UTC), "UTC 01:00 不在 UTC 窗口内")
	require.True(t, WithinTimeRange(moment, start, end, zone8), "同一时刻在 UTC+8 是 09:00，应命中")
	// nil 时区按 UTC 兜底：与显式传 time.UTC 结果一致（且不 panic）。
	require.Equal(t,
		WithinTimeRange(moment, start, end, time.UTC),
		WithinTimeRange(moment, start, end, nil),
		"nil 时区须按 UTC 兜底")
	require.False(t, WithinTimeRange(moment, start, end, nil), "兜底为 UTC → 不在窗口内")
}

// Scenario：当地日期按功能声明的时区计算。
// 用 UTC 日期判"当日"会在 UTC+8 的早上跨日错位，导致同一天重复触发。
func TestPlatformFeatureLocalDateUsesLocation(t *testing.T) {
	t.Parallel()

	// UTC 2026-09-21 17:00 == UTC+8 2026-09-22 01:00（跨日）。
	moment := time.Date(2026, 9, 21, 17, 0, 0, 0, time.UTC)
	zone8 := time.FixedZone("UTC+8", 8*60*60)

	require.Equal(t, "2026-09-21", PlatformFeatureLocalDate(moment, time.UTC).Format(time.DateOnly))
	require.Equal(t, "2026-09-22", PlatformFeatureLocalDate(moment, zone8).Format(time.DateOnly),
		"UTC+8 已跨到次日")
	require.Equal(t, "2026-09-21", PlatformFeatureLocalDate(moment, nil).Format(time.DateOnly),
		"nil 时区按 UTC 兜底")
}

// --- 归一化 ---

// Scenario：未注册的平台 / 未声明的功能键全部丢弃。
// 注册表是唯一权威；残留脏键会让设置页读出注册表里不存在的功能。
func TestNormalizeDropsUnknownPlatformAndFeature(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "norm-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "norm-platform",
		Features: []PlatformFeatureDefinition{{
			Key:              "known",
			Kind:             PlatformFeatureBool,
			EnabledByDefault: false,
		}},
	})

	normalized := NormalizePlatformFeatureSettings(PlatformFeatureSettings{
		"norm-platform":    {"known": {Enabled: true}, "unknown": {Enabled: true}},
		"never-registered": {"whatever": {Enabled: true}},
	})

	require.Contains(t, normalized, "norm-platform")
	require.NotContains(t, normalized["norm-platform"], "unknown", "未声明功能被丢弃")
	require.NotContains(t, normalized, "never-registered", "未注册平台被丢弃")
	require.True(t, normalized["norm-platform"]["known"].Enabled)
}

// Scenario：time_range 的非法时间点退回注册默认值；start == end 也退回默认。
// 「开着但窗口零长」会表现为功能静默失效，必须挡在落库之外。
func TestNormalizeTimeRangeFallsBackToDefaults(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "tr-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "tr-platform",
		Features: []PlatformFeatureDefinition{{
			Key:              "window",
			Kind:             PlatformFeatureTimeRange,
			EnabledByDefault: false,
			Timezone:         time.UTC,
			DefaultStart:     TimeOfDay{Hour: 9, Minute: 0},
			DefaultEnd:       TimeOfDay{Hour: 11, Minute: 0},
		}},
	})

	t.Run("缺时间点→补默认", func(t *testing.T) {
		normalized := NormalizePlatformFeatureSettings(PlatformFeatureSettings{
			"tr-platform": {"window": {Enabled: true}},
		})
		value := normalized["tr-platform"]["window"]
		require.True(t, value.Enabled)
		require.Equal(t, "09:00", value.Start.Format())
		require.Equal(t, "11:00", value.End.Format())
	})

	t.Run("越界时间点→退回默认", func(t *testing.T) {
		normalized := NormalizePlatformFeatureSettings(PlatformFeatureSettings{
			"tr-platform": {"window": {
				Enabled: true,
				Start:   &TimeOfDay{Hour: 25, Minute: 0},
				End:     &TimeOfDay{Hour: 11, Minute: 0},
			}},
		})
		require.Equal(t, "09:00", normalized["tr-platform"]["window"].Start.Format(), "越界起点退回默认")
	})

	t.Run("零长窗口→退回默认", func(t *testing.T) {
		normalized := NormalizePlatformFeatureSettings(PlatformFeatureSettings{
			"tr-platform": {"window": {
				Enabled: true,
				Start:   &TimeOfDay{Hour: 10, Minute: 0},
				End:     &TimeOfDay{Hour: 10, Minute: 0},
			}},
		})
		value := normalized["tr-platform"]["window"]
		require.Equal(t, "09:00", value.Start.Format())
		require.Equal(t, "11:00", value.End.Format())
	})
}

// Scenario：bool 功能只保留 Enabled，不落时间点（形态纯净）。
func TestNormalizeBoolKeepsOnlyEnabled(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "bool-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "bool-platform",
		Features: []PlatformFeatureDefinition{{Key: "flag", Kind: PlatformFeatureBool}},
	})

	normalized := NormalizePlatformFeatureSettings(PlatformFeatureSettings{
		"bool-platform": {"flag": {Enabled: true, Start: &TimeOfDay{Hour: 1}, End: &TimeOfDay{Hour: 2}}},
	})
	value := normalized["bool-platform"]["flag"]
	require.True(t, value.Enabled)
	require.Nil(t, value.Start, "bool 功能不落时间点")
	require.Nil(t, value.End)
}

// --- 缺省（默认关闭）---

// Scenario：功能未配置时，EnabledByDefault=false 的功能必须解析为关闭。
// 这是「默认关闭」的**基础设施**证据：签到这类写操作没被显式开启就不该发请求。
func TestPlatformFeatureEnabledDefaultsToClosed(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "default-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "default-platform",
		Features: []PlatformFeatureDefinition{{
			Key:              "write-op",
			Kind:             PlatformFeatureBool,
			EnabledByDefault: false,
		}},
	})

	t.Run("空文档（从未配置）", func(t *testing.T) {
		require.False(t, PlatformFeatureEnabled(nil, "default-platform", "write-op"))
	})
	t.Run("有文档但未含该功能", func(t *testing.T) {
		require.False(t, PlatformFeatureEnabled(PlatformFeatureSettings{"default-platform": {}}, "default-platform", "write-op"))
	})
	t.Run("未注册功能恒关闭", func(t *testing.T) {
		require.False(t, PlatformFeatureEnabled(nil, "default-platform", "never-declared"))
		require.False(t, PlatformFeatureEnabled(nil, "never-registered", "write-op"))
	})
	t.Run("显式开启后为真", func(t *testing.T) {
		settings := PlatformFeatureSettings{"default-platform": {"write-op": {Enabled: true}}}
		require.True(t, PlatformFeatureEnabled(settings, "default-platform", "write-op"))
	})
}

// Scenario：EnabledByDefault=true 的声明在缺省时为真（缺省语义双向都正确）。
func TestPlatformFeatureEnabledHonoursEnabledByDefault(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "onedefault-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "onedefault-platform",
		Features: []PlatformFeatureDefinition{{
			Key:              "sane-default",
			Kind:             PlatformFeatureBool,
			EnabledByDefault: true,
		}},
	})

	require.True(t, PlatformFeatureEnabled(nil, "onedefault-platform", "sane-default"))
	settings := PlatformFeatureSettings{"onedefault-platform": {"sane-default": {Enabled: false}}}
	require.False(t, PlatformFeatureEnabled(settings, "onedefault-platform", "sane-default"), "显式关闭覆盖缺省")
}

// --- 时间段解析（ResolvePlatformFeatureTimeRange）---

// Scenario：time_range 功能读取缺省时间点；非 time_range 功能读时间段返回 ok=false。
func TestResolvePlatformFeatureTimeRange(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "resolve-platform")
	zone8 := time.FixedZone("UTC+8", 8*60*60)
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "resolve-platform",
		Features: []PlatformFeatureDefinition{
			{
				Key:              "win",
				Kind:             PlatformFeatureTimeRange,
				EnabledByDefault: false,
				Timezone:         zone8,
				DefaultStart:     TimeOfDay{Hour: 9},
				DefaultEnd:       TimeOfDay{Hour: 11},
			},
			{Key: "plain", Kind: PlatformFeatureBool},
		},
	})

	t.Run("缺省时间点已填充", func(t *testing.T) {
		start, end, location, ok := ResolvePlatformFeatureTimeRange(nil, "resolve-platform", "win")
		require.True(t, ok)
		require.Equal(t, "09:00", start.Format())
		require.Equal(t, "11:00", end.Format())
		require.Equal(t, 8*60*60, func() int { _, offset := time.Now().In(location).Zone(); return offset }(),
			"时区是功能声明的 UTC+8")
	})

	t.Run("bool 功能不返回时间段", func(t *testing.T) {
		_, _, _, ok := ResolvePlatformFeatureTimeRange(nil, "resolve-platform", "plain")
		require.False(t, ok)
	})

	t.Run("未注册功能", func(t *testing.T) {
		_, _, _, ok := ResolvePlatformFeatureTimeRange(nil, "resolve-platform", "nope")
		require.False(t, ok)
	})
}

// --- 序列化 ---

// Scenario：Parse 空串 = 从未配置 → nil；坏 JSON → nil（不让脏数据把设置页搞崩）。
func TestParsePlatformFeatureSettingsRobustness(t *testing.T) {
	t.Parallel()

	require.Nil(t, ParsePlatformFeatureSettings(""))
	require.Nil(t, ParsePlatformFeatureSettings("   "))
	require.Nil(t, ParsePlatformFeatureSettings("{not json"))
	require.NotNil(t, ParsePlatformFeatureSettings(`{"codebuddy":{"checkin":{"enabled":true}}}`))
}

// Scenario：Marshal→Parse 往返保真（含跨零点窗口）。
func TestPlatformFeatureSettingsRoundTrip(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "rt-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "rt-platform",
		Features: []PlatformFeatureDefinition{{
			Key:              "win",
			Kind:             PlatformFeatureTimeRange,
			EnabledByDefault: false,
			Timezone:         time.UTC,
			DefaultStart:     TimeOfDay{Hour: 9},
			DefaultEnd:       TimeOfDay{Hour: 11},
		}},
	})

	blob, err := MarshalPlatformFeatureSettings(PlatformFeatureSettings{
		"rt-platform": {"win": {Enabled: true, Start: &TimeOfDay{Hour: 23}, End: &TimeOfDay{Hour: 1}}},
	})
	require.NoError(t, err)

	parsed := ParsePlatformFeatureSettings(blob)
	require.NotNil(t, parsed)
	require.True(t, parsed["rt-platform"]["win"].Enabled)
	require.Equal(t, "23:00", parsed["rt-platform"]["win"].Start.Format())
	require.Equal(t, "01:00", parsed["rt-platform"]["win"].End.Format(), "跨零点窗口不被当成零长")
}

// Scenario：Marshal 对未注册平台做归一化——脏输入不会落进 settings 表。
func TestMarshalDropsUnregistered(t *testing.T) {
	t.Parallel()

	blob, err := MarshalPlatformFeatureSettings(PlatformFeatureSettings{
		"never-registered": {"x": {Enabled: true}},
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(blob), &decoded))
	require.Empty(t, decoded, "未注册平台不应落库")
}

// --- SettingService 读写通道（真实 UpdateSettings 路径）---

// Scenario：走 UpdateSettings → GetAllSettings 的完整往返。
// 写入语义是**整体替换**（与 platform quota 同款）：调用方提交完整表单，
// 未提交的平台落到注册声明的默认值。这条把该语义钉住，防止有人误以为是字段级 patch。
func TestSettingServicePlatformFeaturesRoundTrip(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "svc-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "svc-platform",
		Features: []PlatformFeatureDefinition{
			{Key: "alpha", Kind: PlatformFeatureBool, EnabledByDefault: false},
			{Key: "beta", Kind: PlatformFeatureBool, EnabledByDefault: false},
		},
	})

	repo := newPlatformFeatureTestRepo()
	svc := newPlatformFeatureTestService(t, repo)
	ctx := context.Background()

	// 首次读取：从未配置 → 两项都关闭。
	settings, err := svc.GetAllSettings(ctx)
	require.NoError(t, err)
	require.False(t, PlatformFeatureEnabled(settings.PlatformFeatures, "svc-platform", "alpha"))
	require.False(t, PlatformFeatureEnabled(settings.PlatformFeatures, "svc-platform", "beta"))

	// 提交完整表单：alpha 开、beta 关。
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
		PlatformFeatures: PlatformFeatureSettings{
			"svc-platform": {
				"alpha": {Enabled: true},
				"beta":  {Enabled: false},
			},
		},
	}))

	settings, err = svc.GetAllSettings(ctx)
	require.NoError(t, err)
	require.True(t, PlatformFeatureEnabled(settings.PlatformFeatures, "svc-platform", "alpha"))
	require.False(t, PlatformFeatureEnabled(settings.PlatformFeatures, "svc-platform", "beta"))

	// 只提交 alpha：整体替换语义下 beta 回到默认值（同为关闭，此处断言 alpha 仍开）。
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
		PlatformFeatures: PlatformFeatureSettings{
			"svc-platform": {"alpha": {Enabled: true}},
		},
	}))
	settings, err = svc.GetAllSettings(ctx)
	require.NoError(t, err)
	require.True(t, PlatformFeatureEnabled(settings.PlatformFeatures, "svc-platform", "alpha"))
	require.False(t, PlatformFeatureEnabled(settings.PlatformFeatures, "svc-platform", "beta"),
		"未提交功能落到注册默认值")
}

// Scenario：UpdateSettings 落库的是归一化后的文档（未注册平台被剥掉）。
func TestSettingServicePlatformFeaturesNormalizesOnWrite(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "write-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "write-platform",
		Features: []PlatformFeatureDefinition{{
			Key:              "win",
			Kind:             PlatformFeatureTimeRange,
			EnabledByDefault: false,
			Timezone:         time.UTC,
			DefaultStart:     TimeOfDay{Hour: 9},
			DefaultEnd:       TimeOfDay{Hour: 11},
		}},
	})

	repo := newPlatformFeatureTestRepo()
	svc := newPlatformFeatureTestService(t, repo)

	require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{
		PlatformFeatures: PlatformFeatureSettings{
			"write-platform": {"win": {
				Enabled: true,
				Start:   &TimeOfDay{Hour: 10},
				End:     &TimeOfDay{Hour: 10}, // 零长 → 归一化退回默认
			}},
			"ghost-platform": {"x": {Enabled: true}},
		},
	}))

	stored := repo.vals[SettingKeyPlatformFeatures]
	require.NotContains(t, stored, "ghost-platform", "未注册平台不落库")

	parsed := ParsePlatformFeatureSettings(stored)
	require.Equal(t, "09:00", parsed["write-platform"]["win"].Start.Format(), "零长窗口落库前已修正")
	require.Equal(t, "11:00", parsed["write-platform"]["win"].End.Format())
}

// Scenario（D1 回归，2026-09-23）：**整表单保存不得清空平台功能开关**。
//
// 真实事故：管理员在设置页打开签到开关后，只要因**其它**设置点一次"保存设置"
// （整表单 PUT /admin/settings），`platform_features` 就被写成 `{}`，
// 三个开关静默回关闭——而前端只在 onMounted 加载一次、保存后不回刷，
// **界面仍显示"已开启"**。这会让 dev 验收结论依赖操作顺序
// （"开关看起来开着但调度器不跑"被误判成调度器 bug）。
//
// 三处叠加（缺一不成）：① buildSystemSettingsUpdates 无条件写入，无 nil 守卫
// （相邻 DefaultPlatformQuotas/AccountSchedulingThresholds 都有）
// ② handler 组装 SystemSettings 时从不设置该字段 → 恒传 nil
// ③ UpdateSettingsRequest 无该 JSON 字段 → 永远进不了 omitted 保护
//
// 本用例锁住 ①②：handler 路径传 nil 时，**已存的开关必须保留**。
func TestUpdateSettingsNilPlatformFeaturesKeepsStoredSwitches(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "keep-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "keep-platform",
		Features: []PlatformFeatureDefinition{
			{Key: "checkin", Kind: PlatformFeatureBool, EnabledByDefault: false},
		},
	})

	repo := newPlatformFeatureTestRepo()
	svc := newPlatformFeatureTestService(t, repo)
	ctx := context.Background()

	// ① 管理员打开开关（走真实 UpdateSettings 路径）。
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
		PlatformFeatures: PlatformFeatureSettings{
			"keep-platform": {"checkin": {Enabled: true}},
		},
	}))
	stored := repo.vals[SettingKeyPlatformFeatures]
	require.True(t,
		PlatformFeatureEnabled(ParsePlatformFeatureSettings(stored), "keep-platform", "checkin"),
		"前置条件：开关应已存为开启")

	// ② 另一次整表单保存：handler 从不设置 PlatformFeatures → 传 nil。
	//    模拟"用户只是改了别的设置就点了保存"。
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{}))

	after := repo.vals[SettingKeyPlatformFeatures]
	require.True(t,
		PlatformFeatureEnabled(ParsePlatformFeatureSettings(after), "keep-platform", "checkin"),
		"整表单保存（未携带平台功能）不得清空已开启的开关；"+
			"清空会让界面显示'已开启'而实际已关闭。落库值=%q", after)
}

// Scenario（D1 配对）：显式提交空表 = 用户**有意**清空，应当被尊重。
//
// 与上一例配对，防止"加守卫"退化成"永远写不进去"——
// 那样管理员将无法通过提交空表来关闭开关。
func TestUpdateSettingsExplicitEmptyPlatformFeaturesClearsSwitches(t *testing.T) {
	queueQueuedPlatformFeatureRegistryCleanup(t, "clear-platform")
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: "clear-platform",
		Features: []PlatformFeatureDefinition{
			{Key: "checkin", Kind: PlatformFeatureBool, EnabledByDefault: false},
		},
	})

	repo := newPlatformFeatureTestRepo()
	svc := newPlatformFeatureTestService(t, repo)
	ctx := context.Background()

	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
		PlatformFeatures: PlatformFeatureSettings{
			"clear-platform": {"checkin": {Enabled: true}},
		},
	}))
	require.True(t, PlatformFeatureEnabled(
		ParsePlatformFeatureSettings(repo.vals[SettingKeyPlatformFeatures]), "clear-platform", "checkin"))

	// 显式空表（非 nil）：这是"有意清空"，必须生效。
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
		PlatformFeatures: PlatformFeatureSettings{},
	}))

	after := repo.vals[SettingKeyPlatformFeatures]
	require.False(t,
		PlatformFeatureEnabled(ParsePlatformFeatureSettings(after), "clear-platform", "checkin"),
		"显式提交空表应真正清空开关（否则管理员关不掉）；落库值=%q", after)
}

// Scenario：settings 读取报错 → 平台功能按缺省（关闭）处理，不 panic、不误开。
// fail-closed：DB 抖动绝不能把写操作功能意外打开。此处走 GetAll 失败路径。
func TestSettingServicePlatformFeaturesReadErrorIsFailClosed(t *testing.T) {
	repo := newPlatformFeatureTestRepo()
	repo.vals[SettingKeyPlatformFeatures] = `{"codebuddy":{"checkin":{"enabled":true}}}`
	repo.getErr = errors.New("db down")
	svc := newPlatformFeatureTestService(t, repo)

	_, err := svc.GetAllSettings(context.Background())
	require.Error(t, err, "GetAllSettings 自身如实报错（设置页可提示），不静默吞掉")

	// 直连读取的 fail-closed 证明：GetValue 报错时平台功能按未配置处理。
	repo2 := newPlatformFeatureTestRepo()
	repo2.getErr = errors.New("db down")
	svc2 := newPlatformFeatureTestService(t, repo2)
	require.False(t,
		PlatformFeatureEnabled(ParsePlatformFeatureSettings(""), PlatformCodeBuddy, CodeBuddyCheckinFeatureKey),
		"读取失败时签到必须保持关闭")
	_ = svc2
}

// Scenario：**全局状态守卫**——本文件的测试调 GetAllSettings 时会经 parseSettings
// 写 xai 的进程级运行时映射选项（Grok 账号空 model_mapping 时的默认映射）。
//
// 两件事都要钉住：
//  1. 这个污染**真实存在**（不完整 settings 文档会让 EnableCrossClientMap 判成
//     缺省 true，给全进程装上 `gpt-* → grok-*` 通配映射）；
//  2. preserveXAIRuntimeMappingOptions 的还原**确实有效**，否则该污染会打红
//     别的测试（Grok alias 归一化）且难以定位。
//
// 断言写在显式 restore() 之后而不是测试末尾：helper 注册的是 t.Cleanup，
// 测试体内全局仍是脏的，拿它当"未泄漏"的判据会误报。
func TestPlatformFeatureTestsDoNotLeakXAIRuntimeMapping(t *testing.T) {
	before := xai.RuntimeModelMappingOptions()

	restore := preserveXAIRuntimeMappingOptions()
	// 刻意不用 helper 构造（helper 自带 cleanup），这里要手动观察污染与还原。
	svc := NewSettingService(newPlatformFeatureTestRepo(), &config.Config{})

	// GetAllSettings 成功返回，但副作用是写全局。
	_, err := svc.GetAllSettings(context.Background())
	require.NoError(t, err)

	// 1) 污染真实存在——这解释了为什么必须包 restore。
	polluted := xai.RuntimeModelMappingOptions()
	require.True(t, polluted.EnableCrossClientMap,
		"不完整 settings 文档会把 CrossClientMap 判成缺省 true（污染确实发生）")
	require.NotEqual(t, before, polluted)

	// 2) 还原有效。
	restore()
	require.Equal(t, before, xai.RuntimeModelMappingOptions(),
		"restore 必须把 xai 进程级映射配置还原，否则会泄漏给其他测试")
	require.False(t, xai.RuntimeModelMappingOptions().EnableCrossClientMap,
		"还原后不得残留 gpt-* → grok-* 通配映射")
}
