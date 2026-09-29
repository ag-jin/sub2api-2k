package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// 立即执行的**接线**测试（provider 层）。
//
// ## 为什么必须测 provider 而不能只测注册表
//
// 注册表与派发函数各自的单测都会绿——它们只验证"注册了就能派发"。
// 但真正的失效模式是**provider 忘了注册**：功能在设置页有开关、有窗口，
// 却没有"立即执行"按钮，而所有单测仍然全绿。
//
// 本轮已发生过同类事故（`growth/channels` 的 handler 手写筛选字段导致
// 新字段静默丢失，三层测试全绿、只有真跑 HTTP 的门禁 C 抓到）。
// 所以这里直接调用 **wire 的 provider 函数**，断言它真的注册了执行者。

// codebuddyImmediateFeatures 本轮要求具备"立即执行"的 codebuddy 功能。
//
// ⚠️ 这是一份**期望清单**，不是从注册表推导的：推导出来的是"当前注册了什么"，
// 而这条测试要问的是"该注册的注册了没"。新增支持立即执行的功能时才改这里。
var codebuddyImmediateFeatures = []string{
	CodeBuddyCheckinFeatureKey,
	CodeBuddyActivityFeatureKey,
	CodeBuddyGrowthFeatureKey,
}

// Scenario：构造 codebuddy 调度器的 provider **确实注册了**立即执行者。
//
// 直接调用 wire 的 `ProvideCodeBuddy*Scheduler`，而不是自己调
// `RegisterPlatformFeatureImmediateRunner`——后者测的是注册表本身，
// 前者才验证"生产装配路径里没有漏掉这一步"。
func TestCodeBuddyProvidersRegisterImmediateRunners(t *testing.T) {
	// 每个用例前清空这三个功能的注册，确保断言的是**本次 provider 的副作用**
	// 而不是别的测试留下的状态。
	for _, key := range codebuddyImmediateFeatures {
		UnregisterPlatformFeatureImmediateRunner(PlatformCodeBuddy, key)
	}
	t.Cleanup(func() {
		for _, key := range codebuddyImmediateFeatures {
			UnregisterPlatformFeatureImmediateRunner(PlatformCodeBuddy, key)
		}
	})

	// 前置：清空后应当都不支持（否则本测试的"注册后支持"断言无意义）。
	for _, key := range codebuddyImmediateFeatures {
		require.False(t, PlatformFeatureSupportsImmediateRun(PlatformCodeBuddy, key),
			"前置：%s 在清空后不应支持立即执行", key)
	}

	// 构造 provider 所需的最小依赖。
	settingRepo := newPlatformFeatureTestRepo()
	settingService := newPlatformFeatureTestService(t, settingRepo)
	// 用 codebuddyAdminTestRepo：它实现了 ListByPlatform / UpdateExtra，
	// 而 activity 的 stub 只覆写 GetByID（嵌 nil 的接口，调别的会 panic）。
	accountRepo := newCodebuddyAdminTestRepo()
	adminService := NewCodeBuddyAdminService(nil, accountRepo, nil)

	// 逐个调用 provider（与 wire 的装配路径同一个函数）。
	ProvideCodeBuddyCheckinScheduler(adminService, settingService, nil)
	ProvideCodeBuddyActivityScheduler(adminService, accountRepo, settingService)
	ProvideCodeBuddyGrowthScheduler(adminService, accountRepo, settingService)

	// 断言：三个功能现在都支持立即执行。
	for _, key := range codebuddyImmediateFeatures {
		require.True(t, PlatformFeatureSupportsImmediateRun(PlatformCodeBuddy, key),
			"provider 未注册 %s 的立即执行者——设置页不会出现'立即执行'按钮，而所有单测仍会绿", key)
	}
}

// Scenario：注册的执行者**可被派发函数调用**（不只是"注册表里有条目"）。
//
// 上一条证明"注册了"，这条证明"注册的是能跑的东西"——两件事可能各自出错：
// 注册了一个 nil 闭包、或注册到一个拼错的键上。
func TestCodeBuddyImmediateRunnersAreActuallyDispatchable(t *testing.T) {
	for _, key := range codebuddyImmediateFeatures {
		UnregisterPlatformFeatureImmediateRunner(PlatformCodeBuddy, key)
	}
	t.Cleanup(func() {
		for _, key := range codebuddyImmediateFeatures {
			UnregisterPlatformFeatureImmediateRunner(PlatformCodeBuddy, key)
		}
	})

	settingRepo := newPlatformFeatureTestRepo()
	settingService := newPlatformFeatureTestService(t, settingRepo)
	// 用 codebuddyAdminTestRepo：它实现了 ListByPlatform / UpdateExtra，
	// 而 activity 的 stub 只覆写 GetByID（嵌 nil 的接口，调别的会 panic）。
	accountRepo := newCodebuddyAdminTestRepo()
	adminService := NewCodeBuddyAdminService(nil, accountRepo, nil)

	ProvideCodeBuddyCheckinScheduler(adminService, settingService, nil)
	ProvideCodeBuddyActivityScheduler(adminService, accountRepo, settingService)
	ProvideCodeBuddyGrowthScheduler(adminService, accountRepo, settingService)

	// 无候选账号 → 各执行者应返回"0 个账号"的结果而不是报错或 panic。
	// 这同时证明闭包非 nil、且能走到执行体（不是注册了个空壳）。
	for _, key := range codebuddyImmediateFeatures {
		result, err := settingService.RunPlatformFeatureNow(context.Background(), PlatformCodeBuddy, key)
		require.NoError(t, err, "%s 的立即执行不应报错（无账号时应返回空结果）", key)
		require.NotNil(t, result, "%s 的立即执行应返回结果", key)
		require.NotEmpty(t, result.Summary, "%s 的结果应带一句可展示的文案", key)
		t.Logf("%s → %s", key, result.Summary)
	}
}

// Scenario：**抽奖类/未授权通道不会被立即执行带上**（合规边界不因按钮而放宽）。
//
// "立即执行"改变的是**触发方式**，不是动作的合规级别。成长链的立即执行走
// `RunCodeBuddyGrowthAllNow`（只跑已授权自动的通道），所以即使管理员点按钮，
// 抽奖也不会被跑。这条测试钉住这个语义。
func TestCodeBuddyGrowthImmediateRunKeepsLotteryOut(t *testing.T) {
	// 直接核对通道集合（按钮只是触发器，通道集合由授权决定）。
	keys := codebuddy.CodeBuddyGrowthAutoRunnableChannelKeys()
	require.NotContains(t, keys, codebuddy.CodeBuddyGrowthChannelLottery,
		"抽奖未获自动授权，立即执行也不得带上它（触发方式不改变合规级别）")

	// 而三条已授权的应在其中（否则按钮点了领不到东西）。
	for _, key := range []string{
		codebuddy.CodeBuddyGrowthChannelAdopt,
		codebuddy.CodeBuddyGrowthChannelNightCat,
		codebuddy.CodeBuddyGrowthChannelSchool,
	} {
		require.Contains(t, keys, key, "%s 已授权，应立即执行能跑到", key)
	}
}
