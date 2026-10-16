//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 以下为重构前各处手写的平台列表 / switch，作为平台清单派生结果的等价基准；
// 重构后新登记的平台（Command Code、Cline）按同类平台（OpenCode）的位置补入。
// 本地存量平台 CodeBuddy 与 card 血统的 opencode 按合并前前端顺序补入 deepseek 与
// minimax 之间；组合分组相关列表沿用合并前的显式列表，不含二者。
var (
	legacyAllPlatforms = []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformOpenCode, PlatformCodeBuddy,
		PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
	}
	legacySchedulerSnapshotPlatforms = []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyCompositeMatchingPlatforms = legacySchedulerSnapshotPlatforms
	legacyMultiProtocolProviders     = []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformCommandCode, PlatformCline}
	// 组合分组的请求目标边界（合并前的显式列表 / 迁移 238 的 target_platform CHECK）：
	// 本地存量平台 CodeBuddy 与 opencode 不在其中，登记后仍不得成为组合目标。
	legacyCompositeTargetPlatforms = legacySchedulerSnapshotPlatforms
	platformProbeValues            = append(append([]string{}, legacyAllPlatforms...), PlatformComposite, "", "moonshot", "Kimi", "openai ", "glm", "bogus")
	platformProbeAccountTypes      = []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken, AccountTypeUpstream, ""}
)

func legacyIsCNProvider(platform string) bool {
	switch platform {
	case PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax:
		return true
	}
	return false
}

func legacyIsOpenAICompatible(platform string) bool {
	return platform == PlatformOpenAI || platform == PlatformGrok || legacyIsCNProvider(platform) || platform == PlatformOpenCodeGo || platform == PlatformCommandCode || platform == PlatformCline ||
		// 合并前 IsOpenAICompatible 显式含 card 血统 opencode（api_key）与 CodeBuddy。
		platform == PlatformOpenCode || platform == PlatformCodeBuddy
}

func legacyNormalizeOpenAICompatiblePlatform(platform string) string {
	switch platform {
	case PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformCommandCode, PlatformCline,
		// 合并前 NormalizeOpenAICompatiblePlatform 保留 opencode 存量值与 codebuddy 原值。
		PlatformOpenCode, PlatformCodeBuddy:
		return platform
	}
	return PlatformOpenAI
}

func legacyIsUpstreamBillingProbeIdentity(platform, accountType string) bool {
	if accountType != AccountTypeAPIKey {
		return false
	}
	// 上游改为 data-driven（IsConcretePlatform）后新登记的具体平台一律纳入；
	// 合并前显式列表不含本地两个平台，此处按登记后语义补入（已报备的放宽）。
	return legacyIsConcretePlatform(platform)
}

func legacyIsHeaderOverrideEligible(platform, accountType string) bool {
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformCommandCode, PlatformCline,
		// 合并前显式含 card 血统 opencode 与 CodeBuddy（同为 API Key 形态上游）。
		PlatformOpenCode, PlatformCodeBuddy:
		return accountType == AccountTypeAPIKey
	case PlatformGrok:
		return accountType == AccountTypeAPIKey || accountType == AccountTypeOAuth
	}
	return false
}

func legacyIsConcretePlatform(platform string) bool {
	for _, p := range legacyAllPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

// legacyIsConcreteRequestPlatform 是组合分组的请求目标边界（合并前显式列表），
// 与账号 / 分组合法性（IsConcretePlatform）不同：不含本地两个存量平台。
func legacyIsConcreteRequestPlatform(platform string) bool {
	for _, p := range legacyCompositeTargetPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

func TestPlatformListDerivedListsMatchLegacy(t *testing.T) {
	require.Equal(t, legacyAllPlatforms, AllowedQuotaPlatforms)
	require.Equal(t, legacySchedulerSnapshotPlatforms, schedulerSnapshotPlatforms())
	require.Equal(t, legacyCompositeMatchingPlatforms, matchingPlatforms(PlatformComposite))
	require.Equal(t, []string{PlatformKimi}, matchingPlatforms(PlatformKimi))

	var multiProtocol []string
	for _, platform := range legacyAllPlatforms {
		if IsMultiProtocolAPIKeyProvider(platform) {
			multiProtocol = append(multiProtocol, platform)
		}
	}
	require.Equal(t, legacyMultiProtocolProviders, multiProtocol)
}

func TestPlatformListDerivedPredicatesMatchLegacy(t *testing.T) {
	for _, platform := range platformProbeValues {
		require.Equal(t, legacyIsCNProvider(platform), IsCNProvider(platform), platform)
		require.Equal(t, legacyIsConcreteRequestPlatform(platform), isConcreteRequestPlatform(platform), platform)
		require.Equal(t, legacyNormalizeOpenAICompatiblePlatform(platform), NormalizeOpenAICompatiblePlatform(platform), platform)
		require.Equal(t, legacyIsOpenAICompatible(platform), (&Account{Platform: platform}).IsOpenAICompatible(), platform)
		for _, accountType := range platformProbeAccountTypes {
			require.Equal(t, legacyIsUpstreamBillingProbeIdentity(platform, accountType),
				IsUpstreamBillingProbeIdentity(platform, accountType), "%s/%s", platform, accountType)
			require.Equal(t, legacyIsHeaderOverrideEligible(platform, accountType),
				(&Account{Platform: platform, Type: accountType}).IsHeaderOverrideEligible(), "%s/%s", platform, accountType)
		}
	}
}

// 多协议 API Key 供应商经 OpenAI 网关转发（NormalizeOpenAICompatiblePlatform、
// countTokens 路由等依赖此约束），ProviderProfile 登记的平台必须在平台清单中声明 openai 网关。
func TestProviderProfilesUseOpenAIGateway(t *testing.T) {
	for platform := range providerProfiles {
		spec, ok := domain.LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, domain.PlatformGatewayOpenAI, spec.Gateway, platform)
	}
}

// 本地存量平台（CodeBuddy、card 血统的 opencode）保持合并前的"非多协议供应商"语义：
// 不登记 provider profile，各自专属分支（CodeBuddy 静态模型清单与专用错误归类、
// OpenCode 的 go/zen 端点与协议规则）不被通用 pipeline 覆盖；同时仍按 OpenAI 网关
// 平台归一，且不作组合分组目标（合并前的组合边界）。
func TestLocalPlatformsKeepDedicatedBranches(t *testing.T) {
	for _, platform := range []string{PlatformCodeBuddy, PlatformOpenCode} {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey}
		require.Nil(t, LookupProviderProfile(platform), platform)
		require.False(t, IsMultiProtocolAPIKeyProvider(platform), platform)
		require.False(t, account.IsMultiProtocolAPIKey(), platform)
		require.False(t, account.RoutesProtocolByInbound(), platform)
		require.False(t, account.routesByModel(), platform)
		require.Equal(t, platform, NormalizeOpenAICompatiblePlatform(platform), platform)
		require.True(t, account.IsOpenAICompatible(), platform)
		require.True(t, domain.UsesOpenAIGateway(platform), platform)
		require.False(t, domain.IsCompositeTargetPlatform(platform), platform)
		require.NotContains(t, matchingPlatforms(PlatformComposite), platform)
	}
	// 对照：opencode_go 保持 profile 登记（go/zen 端点与按模型分流不被本地兼容逻辑影响）。
	require.NotNil(t, LookupProviderProfile(PlatformOpenCodeGo))
	require.True(t, IsMultiProtocolAPIKeyProvider(PlatformOpenCodeGo))
	require.True(t, domain.IsCompositeTargetPlatform(PlatformOpenCodeGo))
}

// 转发、探测与计费中"多协议供应商"类判断改为查 profile：现有平台上与改动前的
// 国产厂商 / 国产厂商 ∪ OpenCode 判断逐一相同。
func TestProviderProfileAccountPredicatesMatchLegacy(t *testing.T) {
	for _, platform := range platformProbeValues {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey}
		// Cline 只有 Chat Completions 一个端点，按入站协议分流（其余入站转换）。
		require.Equal(t, legacyIsCNProvider(platform) || platform == PlatformCline, account.RoutesProtocolByInbound(), platform)
		require.Equal(t, legacyIsCNProvider(platform) || platform == PlatformOpenCodeGo || platform == PlatformCommandCode || platform == PlatformCline, account.IsMultiProtocolAPIKey(), platform)
	}
	var nilAccount *Account
	require.False(t, nilAccount.RoutesProtocolByInbound())
}

// 仅在 providerProfiles 中登记的新供应商按分流方式自动获得对应行为：按入站协议
// 分流的与国产厂商一致，按模型分流的与 OpenCode 一致。
func TestNewlyRegisteredProviderInheritsRoutingBehaviour(t *testing.T) {
	const byInbound, byModel = "test_inbound_provider", "test_model_provider"
	providerProfiles[byInbound] = &ProviderProfile{
		Platform: byInbound, DefaultMode: AccountModePayG, Routing: ProviderRoutingByInbound,
		Modes: map[string]ProviderEndpoints{AccountModePayG: {BaseURLs: map[string]string{
			APIProtocolChatCompletions: "https://inbound.example.com/v1",
			APIProtocolAnthropic:       "https://inbound.example.com/anthropic",
		}}},
	}
	providerProfiles[byModel] = &ProviderProfile{
		Platform: byModel, DefaultMode: AccountModePayG, Routing: ProviderRoutingByModel,
		Modes: map[string]ProviderEndpoints{AccountModePayG: {BaseURLs: map[string]string{
			APIProtocolChatCompletions: "https://model.example.com/v1",
		}}},
	}
	t.Cleanup(func() {
		delete(providerProfiles, byInbound)
		delete(providerProfiles, byModel)
	})

	inbound := &Account{Platform: byInbound, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "sk-test", "api_protocol": APIProtocolAdaptive,
		"base_url": "https://inbound.example.com/v1",
	}}
	require.True(t, inbound.IsMultiProtocolAPIKey())
	require.True(t, inbound.RoutesProtocolByInbound())
	// 无原生 Responses 端点的 adaptive 账号回退 Chat Completions（与 GLM 相同）。
	require.True(t, shouldForwardOpenAIResponsesViaRawChatCompletions(inbound))
	require.True(t, shouldEstimateOpenAIInputTokensLocally(inbound))
	require.Equal(t, "https://inbound.example.com/v1", upstreamModelRegistryBaseURL(inbound))

	model := &Account{Platform: byModel, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "sk-test", "api_protocol": APIProtocolAdaptive,
		"base_url": "https://model.example.com/v1",
	}}
	require.True(t, model.IsMultiProtocolAPIKey())
	require.False(t, model.RoutesProtocolByInbound())
	// 按模型分流：protocol_rules 决定协议，不按 Responses 能力标记回退（与 OpenCode 相同）。
	require.False(t, shouldForwardOpenAIResponsesViaRawChatCompletions(model))
	require.Equal(t, "https://model.example.com/v1", upstreamModelRegistryBaseURL(model))
}
