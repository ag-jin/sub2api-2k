//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyProviderDefaultBaseURL 复述 provider profile 引入前 account.go 中逐平台
// switch 的默认端点逻辑，作为等价性基准。
func legacyProviderDefaultBaseURL(platform, mode, protocol string) string {
	coding := mode == AccountModeCoding
	zen := mode == AccountModeZen
	switch protocol {
	case APIProtocolAnthropic:
		switch platform {
		case PlatformKimi:
			if coding {
				return DefaultKimiCodingAnthropicBaseURL
			}
			return DefaultKimiPayGAnthropicBaseURL
		case PlatformZhipu:
			return DefaultZhipuAnthropicBaseURL
		case PlatformDeepseek:
			return DefaultDeepseekAnthropicBaseURL
		case PlatformMiniMax:
			return DefaultMiniMaxAnthropicBaseURL
		case PlatformOpenCodeGo:
			if zen {
				return DefaultOpenCodeZenAnthropicBaseURL
			}
			return DefaultOpenCodeGoAnthropicBaseURL
		}
	case APIProtocolChatCompletions, APIProtocolResponses:
		if platform == PlatformZhipu && protocol == APIProtocolResponses {
			// 唯一的函数级差异：旧 switch 对智谱 Responses 也返回 CC 基址，profile
			// 以"缺条目即不支持"建模后返回空串。所有运行时调用方都先经
			// UsesNativeCNResponses / SupportsNativeCNResponses 守卫，智谱不会走到这里。
			return ""
		}
		switch platform {
		case PlatformKimi:
			if coding {
				return DefaultKimiCodingBaseURL
			}
			return DefaultKimiPayGBaseURL
		case PlatformZhipu:
			if coding {
				return DefaultZhipuCodingBaseURL
			}
			return DefaultZhipuPayGBaseURL
		case PlatformDeepseek:
			return DefaultDeepseekBaseURL
		case PlatformMiniMax:
			return DefaultMiniMaxBaseURL
		case PlatformOpenCodeGo:
			if zen {
				return DefaultOpenCodeZenBaseURL
			}
			return DefaultOpenCodeGoBaseURL
		}
	}
	return ""
}

func TestProviderProfile_DefaultBaseURLsMatchLegacySwitches(t *testing.T) {
	t.Parallel()

	platforms := []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo}
	modes := []string{"", AccountModePayG, AccountModeCoding, AccountModeZen, AccountModeGo, " coding ", "unknown"}

	for _, platform := range platforms {
		for _, mode := range modes {
			legacyMode := mode
			if legacyMode == " coding " {
				legacyMode = AccountModeCoding
			}
			newAccount := func(protocol string) *Account {
				return &Account{
					Platform: platform,
					Type:     AccountTypeAPIKey,
					Credentials: map[string]any{
						"api_key":      "sk-test",
						"account_mode": mode,
						"api_protocol": protocol,
					},
				}
			}
			name := platform + "/" + mode

			chat := newAccount(APIProtocolChatCompletions)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolChatCompletions), chat.GetOpenAIBaseURL(), name)

			anthropic := newAccount(APIProtocolAnthropic)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolAnthropic), anthropic.GetAnthropicProtocolBaseURL(), name)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolChatCompletions), anthropic.GetOpenAIFormatBaseURL(), name)

			adaptive := newAccount(APIProtocolAdaptive)
			for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolResponses, APIProtocolAnthropic, "unknown"} {
				require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, protocol), adaptive.GetCNProtocolBaseURL(protocol), name+"/"+protocol)
			}
		}
	}
}

func TestProviderProfile_ProfilesMatchMultiProtocolPlatforms(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformComposite, "",
	} {
		want := IsCNProvider(platform) || platform == PlatformOpenCodeGo
		require.Equal(t, want, IsMultiProtocolAPIKeyProvider(platform), platform)
		require.Equal(t, want, LookupProviderProfile(platform) != nil, platform)
	}

	for platform, profile := range providerProfiles {
		require.Equal(t, platform, profile.Platform)
		_, ok := profile.Modes[profile.DefaultMode]
		require.True(t, ok, "%s default mode %q must be registered", platform, profile.DefaultMode)
	}
}

func TestProviderProfile_NativeResponsesMatchesLegacy(t *testing.T) {
	t.Parallel()

	legacy := map[string]bool{
		PlatformDeepseek:   true,
		PlatformKimi:       true,
		PlatformMiniMax:    true,
		PlatformOpenCodeGo: true,
	}
	modes := []string{"", AccountModePayG, AccountModeCoding, AccountModeZen, AccountModeGo, "unknown"}
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, mode := range modes {
			account := &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"account_mode": mode}}
			require.Equal(t, legacy[platform], account.SupportsNativeCNResponses(), platform+"/"+mode)
		}
	}
	require.False(t, (*Account)(nil).SupportsNativeCNResponses())
}

func TestProviderProfile_BaseURLKeysAreNativeProtocols(t *testing.T) {
	t.Parallel()

	for platform, profile := range providerProfiles {
		for mode, endpoints := range profile.Modes {
			require.NotEmpty(t, endpoints.BaseURLs[APIProtocolChatCompletions], "%s/%s must have a chat_completions base", platform, mode)
			for protocol, baseURL := range endpoints.BaseURLs {
				require.True(t, isNativeUpstreamProtocol(protocol), "%s/%s has unknown protocol key %q", platform, mode, protocol)
				require.NotEmpty(t, baseURL, "%s/%s/%s", platform, mode, protocol)
			}
		}
	}
}

func TestProviderProfile_ResponsesPathMatchesLegacy(t *testing.T) {
	t.Parallel()

	legacy := func(platform, base string) string {
		if platform == PlatformDeepseek {
			return buildOpenAIEndpointURL(base, "/responses")
		}
		return buildOpenAIResponsesURL(base)
	}
	bases := []string{
		"https://api.deepseek.com", "https://relay.example.com", "https://relay.example.com/v1",
		"https://opencode.ai/zen/go/v1", "https://open.bigmodel.cn/api/paas/v4", "https://api.moonshot.cn/v1",
	}
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, base := range bases {
			require.Equal(t, legacy(platform, base), buildOpenAIResponsesURLForPlatform(platform, base), platform+" "+base)
		}
	}
}

func TestProviderProfile_OpenCodeProtocolRulesByMode(t *testing.T) {
	t.Parallel()

	openCode := LookupProviderProfile(PlatformOpenCodeGo)
	require.Equal(t, DefaultOpenCodeGoProtocolRules(), openCode.Endpoints(AccountModeGo).ProtocolRules)
	require.Equal(t, DefaultOpenCodeGoProtocolRules(), openCode.Endpoints("").ProtocolRules)
	require.Equal(t, DefaultOpenCodeZenProtocolRules(), openCode.Endpoints(AccountModeZen).ProtocolRules)
	require.Empty(t, LookupProviderProfile(PlatformKimi).Endpoints(AccountModeCoding).ProtocolRules)
}

// 本地存量平台不是 profile 供应商：CodeBuddy 与 card 血统 opencode 的端点 / 协议
// 分流仍走各自专属分支（CodeBuddy 静态清单与专用链路；OpenCode 的
// case PlatformOpenCode, PlatformOpenCodeGo 与 IsOpenCodeGo 兼容），
// 通用 profile pipeline 不得覆盖它们。
func TestProviderProfile_LocalPlatformsHaveNoProfile(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{PlatformCodeBuddy, PlatformOpenCode, PlatformComposite, ""} {
		require.Nil(t, LookupProviderProfile(platform), platform)
		require.False(t, IsMultiProtocolAPIKeyProvider(platform), platform)
	}
}

// OpenCode Go 的 go / zen 端点与按模型分流规则保持登记：opencode 存量值与
// opencode_go 共用同一套端点差异（Go 为默认，未设置 account_mode 时按 Go 处理），
// 不得被通用 pipeline 抹平。
func TestProviderProfile_OpenCodeGoKeepsGoAndZenEndpoints(t *testing.T) {
	t.Parallel()

	profile := LookupProviderProfile(PlatformOpenCodeGo)
	require.NotNil(t, profile)
	require.Equal(t, AccountModeGo, profile.DefaultMode)
	require.Equal(t, ProviderRoutingByModel, profile.Routing)
	require.Equal(t, DefaultOpenCodeGoTestModel, profile.DefaultTestModel)
	require.Equal(t, DefaultOpenCodeGoBaseURL, profile.DefaultBaseURL(AccountModeGo, APIProtocolChatCompletions))
	require.Equal(t, DefaultOpenCodeGoBaseURL, profile.DefaultBaseURL(AccountModeGo, APIProtocolResponses))
	require.Equal(t, DefaultOpenCodeGoAnthropicBaseURL, profile.DefaultBaseURL(AccountModeGo, APIProtocolAnthropic))
	require.Equal(t, DefaultOpenCodeZenBaseURL, profile.DefaultBaseURL(AccountModeZen, APIProtocolChatCompletions))
	require.Equal(t, DefaultOpenCodeZenBaseURL, profile.DefaultBaseURL(AccountModeZen, APIProtocolResponses))
	require.Equal(t, DefaultOpenCodeZenAnthropicBaseURL, profile.DefaultBaseURL(AccountModeZen, APIProtocolAnthropic))
	require.Equal(t, DefaultOpenCodeGoProtocolRules(), profile.Endpoints(AccountModeGo).ProtocolRules)
	require.Equal(t, DefaultOpenCodeZenProtocolRules(), profile.Endpoints(AccountModeZen).ProtocolRules)
	// 未知 / 空 mode 回落 DefaultMode（go），与账号侧 GetOpenCodeAccountMode
	// "未设置按 Go 处理"一致。
	require.Equal(t, DefaultOpenCodeGoBaseURL, profile.DefaultBaseURL("", APIProtocolChatCompletions))
}
