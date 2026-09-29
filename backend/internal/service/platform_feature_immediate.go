package service

import (
	"context"
	"strings"
	"sync"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 平台功能的**立即执行**能力（设置页的"立即执行"按钮）。
//
// ## 为什么需要它
//
// 平台功能只有"开关 + 时段"这一种形态时，用户唯一的触发手段是**等窗口**。
// 实测痛点：窗口设得晚（或当天已经错过），就只能等到第二天——用户的原话是
// 「设置的时间有时候太晚，会到第二天去」。
//
// 所以注册表额外声明"本功能支持立即执行"，设置页据此渲染按钮，
// 由服务端把 `(platform, key)` 派发到对应的执行者。
//
// ## 为什么用执行者注册表，而不是让设置页直接调各平台端点
//
// 设置页是**平台无关**的（`usePlatformFeatures` 不为任何具体平台写死）。
// 若让它自己拼"checkin 要调 /admin/codebuddy/accounts/checkin-all、
// growth 要调 /admin/codebuddy/growth/run-all"，那么每接一个新平台/新功能
// 都要改前端——正是注册表架构要避免的那种耦合。
//
// 现在前端只知道一件事："这个 feature 有立即执行按钮，点了就把 platform+key
// 发给服务端"。动作到端点的映射完全在服务端，新增功能只需在后端注册。
//
// ## 与"开关"的关系
//
// 立即执行**不读**功能开关：开关管的是"自动排程要不要跑"，
// 而立即执行是"人明确要求现在就跑"。被自动排程的开关挡住会让人点了没反应
// 且找不到原因（与各平台手动端点的既有口径一致）。
//
// 同理，立即执行也**不读**窗口——否则用户在窗口外想手动补一次就没法补，
// 而那正是本能力要解决的场景。

// PlatformFeatureRunResult 立即执行的统一回执。
//
// 各平台动作的回执形状差异很大（签到是四态计数、成长链是通道汇总、
// 抽奖是次数），设置页不该逐个解析。所以由**服务端**按自身语义生成
// 一句话 `Summary`，前端直接展示；原始回执放 `Detail` 供排查。
type PlatformFeatureRunResult struct {
	// Summary 一句话结果（服务端生成，前端直接展示）。
	Summary string `json:"summary"`
	// Detail 原始回执（形状随功能而异，前端不解析）。
	Detail any `json:"detail,omitempty"`
}

// PlatformFeatureImmediateRunner 某功能"立即执行"的执行者。
type PlatformFeatureImmediateRunner func(ctx context.Context) (*PlatformFeatureRunResult, error)

// platformFeatureImmediateKey 拼注册表的复合键。
//
// 用 `platform\x00key` 而不是嵌套 map：单层 map 让"注册表里有哪些功能支持
// 立即执行"可以用一次遍历回答（供守卫测试断言），嵌套则要两层遍历且容易漏。
func platformFeatureImmediateKey(platform, featureKey string) string {
	return strings.TrimSpace(platform) + "\x00" + strings.TrimSpace(featureKey)
}

var (
	platformFeatureImmediateMu      sync.RWMutex
	platformFeatureImmediateRunners = map[string]PlatformFeatureImmediateRunner{}
)

// RegisterPlatformFeatureImmediateRunner 注册某功能的立即执行者。
//
// 与 `RegisterPlatformFeatureSpec` 同风格（package 级注册表），但**不在 init()
// 里注册**：执行者是闭包，必须等依赖（服务/调度器）构造完成。
// 所以由 wire 的 provider 在构造完依赖后调用本函数——见
// `ProvideCodeBuddyCheckinScheduler` 等处的注册。
//
// 重复注册**覆盖**并保持静默：provider 在测试里可能被多次调用，
// 报错会让测试难以复用。生产路径每个功能只注册一次。
func RegisterPlatformFeatureImmediateRunner(
	platform, featureKey string,
	runner PlatformFeatureImmediateRunner,
) {
	if runner == nil {
		return
	}
	platformFeatureImmediateMu.Lock()
	defer platformFeatureImmediateMu.Unlock()
	platformFeatureImmediateRunners[platformFeatureImmediateKey(platform, featureKey)] = runner
}

// UnregisterPlatformFeatureImmediateRunner 反注册（仅供测试清理）。
func UnregisterPlatformFeatureImmediateRunner(platform, featureKey string) {
	platformFeatureImmediateMu.Lock()
	defer platformFeatureImmediateMu.Unlock()
	delete(platformFeatureImmediateRunners, platformFeatureImmediateKey(platform, featureKey))
}

// PlatformFeatureSupportsImmediateRun 报告该功能是否具备立即执行能力。
//
// 只**报告**注册表事实，不判断功能是否存在（调用方的 `platformFeatureLookup`
// 已经保证了存在性）。设置页据此决定是否渲染按钮。
func PlatformFeatureSupportsImmediateRun(platform, featureKey string) bool {
	platformFeatureImmediateMu.RLock()
	defer platformFeatureImmediateMu.RUnlock()
	_, ok := platformFeatureImmediateRunners[platformFeatureImmediateKey(platform, featureKey)]
	return ok
}

// RunPlatformFeatureNow 立即执行某平台功能（设置页按钮的后端入口）。
//
// 校验链：
//  1. 平台/功能必须在注册表里（否则 404 语义的 not found）——防止调用方
//     拼错 key 却得到一个含糊的 500；
//  2. 该功能必须注册过立即执行者（否则 400：这个功能不支持手动触发）。
func (s *SettingService) RunPlatformFeatureNow(
	ctx context.Context,
	platform, featureKey string,
) (*PlatformFeatureRunResult, error) {
	platform = strings.TrimSpace(platform)
	featureKey = strings.TrimSpace(featureKey)
	if platform == "" || featureKey == "" {
		return nil, infraerrors.BadRequest("PLATFORM_FEATURE_INVALID", "platform and key are required")
	}

	// ① 功能必须在注册表里（未注册 = 调用方拼错，不是"暂时不可用"）。
	found := false
	for _, spec := range RegisteredPlatformFeatureSpecs() {
		if spec.Platform != platform {
			continue
		}
		for _, definition := range spec.Features {
			if definition.Key == featureKey {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, infraerrors.NotFound("PLATFORM_FEATURE_NOT_FOUND",
			"platform feature not registered: "+platform+"/"+featureKey)
	}

	// ② 该功能必须支持立即执行。
	platformFeatureImmediateMu.RLock()
	runner := platformFeatureImmediateRunners[platformFeatureImmediateKey(platform, featureKey)]
	platformFeatureImmediateMu.RUnlock()
	if runner == nil {
		return nil, infraerrors.BadRequest("PLATFORM_FEATURE_NOT_RUNNABLE",
			"platform feature has no immediate action: "+platform+"/"+featureKey)
	}

	return runner(ctx)
}
