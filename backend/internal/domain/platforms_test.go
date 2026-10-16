package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 重构前各处手写的平台列表，作为平台清单派生结果的等价基准；重构后新登记的平台
// （Command Code、Cline）按同类平台（OpenCode）的位置补入。
//
// 本地存量平台 codebuddy 与 card 血统的 opencode（PlatformOpenCode）按合并前的
// 前端顺序补入 deepseek 与 minimax 之间（frontend/src/constants/platforms.ts 的
// CONCRETE_PLATFORM_OPTIONS）；组合分组的等价基准沿用合并前的显式列表，不含二者。
var (
	legacyDisplayOrder = []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformOpenCode, PlatformCodeBuddy,
		PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyCompositePrecedence = []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyOpenAIGateway = []string{
		PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformOpenCode, PlatformCodeBuddy,
		PlatformMiniMax, PlatformOpenCodeGo,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyCNProviders    = []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax}
	legacyLiteLLMByPlatf = map[string]string{
		PlatformAnthropic: "anthropic", PlatformOpenAI: "openai", PlatformGemini: "gemini",
		PlatformAntigravity: "anthropic", PlatformGrok: "xai", PlatformKimi: "moonshot",
		PlatformZhipu: "zhipu", PlatformDeepseek: "deepseek", PlatformMiniMax: "minimax",
		PlatformOpenCodeGo: "opencode-go", PlatformTypeSafe: "typesafe", PlatformCommandCode: "", PlatformCline: "",
		// 合并前 channel_handler 的 platformToLiteLLMProvider 没有这两个平台的价源，
		// 登记后仍为空（保持"定价目录不支持"的既有行为）。
		PlatformOpenCode: "", PlatformCodeBuddy: "",
	}
	// 本地存量平台：可作账号/分组平台，但不是组合分组的请求目标。合并前组合链路
	// 各处（isConcreteRequestPlatform、matchingPlatforms、compositeDefaultModelsListCandidateIDs、
	// schedulerSnapshotPlatforms 与迁移 238 的 target_platform CHECK）都显式排除二者。
	legacyNonCompositeTargets = []string{PlatformOpenCode, PlatformCodeBuddy}
)

func TestPlatformListMatchesLegacyLists(t *testing.T) {
	require.Equal(t, legacyDisplayOrder, ConcretePlatformIDs())
	require.Equal(t, legacyCompositePrecedence, CompositePrecedencePlatformIDs())
	require.Equal(t, legacyOpenAIGateway, PlatformIDsWhere(func(spec PlatformSpec) bool {
		return spec.Gateway == PlatformGatewayOpenAI
	}))
	require.Equal(t, legacyCNProviders, PlatformIDsWhere(func(spec PlatformSpec) bool { return spec.CNProvider }))
	for platform, provider := range legacyLiteLLMByPlatf {
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, provider, spec.LiteLLMProvider, platform)
	}
}

func TestPlatformListPredicates(t *testing.T) {
	for _, platform := range legacyDisplayOrder {
		require.True(t, IsConcretePlatform(platform), platform)
		require.True(t, IsGroupPlatform(platform), platform)
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.NotEmpty(t, spec.DisplayName, platform)
		require.NotEmpty(t, spec.Gateway, platform)
	}
	require.False(t, IsConcretePlatform(PlatformComposite))
	require.True(t, IsGroupPlatform(PlatformComposite))
	for _, invalid := range []string{"", "moonshot", "Kimi", "openai ", "glm", "bogus"} {
		require.False(t, IsConcretePlatform(invalid), invalid)
		require.False(t, IsGroupPlatform(invalid), invalid)
		require.False(t, UsesOpenAIGateway(invalid), invalid)
		require.False(t, IsCompositeTargetPlatform(invalid), invalid)
	}
}

// 本地存量平台（codebuddy、card 血统的 opencode）在合并上游 data-driven 平台清单后
// 必须保持登记：分组校验（IsGroupPlatform）与 OpenAI 网关归属（UsesOpenAIGateway）
// 失配会让本地 codebuddy / opencode 分组整体被拒、调度把平台归一成 openai。
// 二者同时保留合并前的组合边界：可作账号/分组平台，但不是组合分组目标。
func TestLocalPlatformsRegisteredWithLocalBoundaries(t *testing.T) {
	for _, platform := range legacyNonCompositeTargets {
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, PlatformGatewayOpenAI, spec.Gateway, platform)
		require.NotEmpty(t, spec.DisplayName, platform)
		require.False(t, spec.CNProvider, platform)
		require.Empty(t, spec.LiteLLMProvider, platform)
		require.True(t, IsConcretePlatform(platform), platform)
		require.True(t, IsGroupPlatform(platform), platform)
		require.True(t, UsesOpenAIGateway(platform), platform)
		require.False(t, IsCNProviderPlatform(platform), platform)
		require.False(t, IsCompositeTargetPlatform(platform), platform)
	}
	for _, platform := range legacyCompositePrecedence {
		require.True(t, IsCompositeTargetPlatform(platform), platform)
	}
	require.False(t, IsCompositeTargetPlatform(PlatformComposite))
}

func TestPlatformsReturnsCopy(t *testing.T) {
	platforms := Platforms()
	platforms[0].ID = "mutated"
	require.Equal(t, PlatformAnthropic, Platforms()[0].ID)
}
