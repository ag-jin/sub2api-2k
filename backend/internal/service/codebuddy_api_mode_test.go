//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CodeBuddy「API 线路」账号（credentials.api_key 非空）：自定义 base_url 指向
// OpenAI 兼容中转（POST {base}/chat/completions、GET {base}/models），与腾讯
// auth JSON（access_token/refresh_token）账号共用 platform=codebuddy|type=apikey。

// apiModeAccount 构造一个 API 线路 codebuddy 账号。
func apiModeAccount(extraCreds map[string]any) *Account {
	creds := map[string]any{
		"api_key":  "sk-relay",
		"base_url": "http://relay.example/v1",
	}
	for k, v := range extraCreds {
		creds[k] = v
	}
	return &Account{ID: 801, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Credentials: creds}
}

func TestNormalizeCodeBuddyCredentials_KeepsAPIKey(t *testing.T) {
	out := NormalizeCodeBuddyCredentials(map[string]any{
		"api_key":  "sk-relay",
		"base_url": "http://relay.example/v1",
	})
	require.NotNil(t, out)
	assert.Equal(t, "sk-relay", out["api_key"], "api_key 是 API 线路账号的凭据，白名单必须保留")
	assert.Equal(t, "http://relay.example/v1", out["base_url"])
}

func TestIsCodeBuddyAPIMode(t *testing.T) {
	assert.True(t, apiModeAccount(nil).IsCodeBuddyAPIMode())

	blankKey := &Account{ID: 802, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at", "api_key": "   "}}
	assert.False(t, blankKey.IsCodeBuddyAPIMode(), "空白 api_key 不算 API 线路")

	oauth := &Account{ID: 803, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at", "refresh_token": "rt"}}
	assert.False(t, oauth.IsCodeBuddyAPIMode(), "仅腾讯 auth JSON 的账号不是 API 线路")

	other := &Account{ID: 804, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-openai"}}
	assert.False(t, other.IsCodeBuddyAPIMode(), "非 codebuddy 平台一律不是 API 线路")

	var nilAccount *Account
	assert.False(t, nilAccount.IsCodeBuddyAPIMode())
}

func TestGetOpenAIProtocolAPIKey_CodeBuddyAPIModeUsesAPIKey(t *testing.T) {
	acct := apiModeAccount(map[string]any{"access_token": "tencent-at"})
	assert.Equal(t, "sk-relay", acct.GetOpenAIProtocolAPIKey(),
		"API 线路账号出站 Bearer 必须是 api_key，不能被 access_token 抢占")

	oauth := &Account{ID: 802, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "tencent-at"}}
	assert.Equal(t, "tencent-at", oauth.GetOpenAIProtocolAPIKey(),
		"腾讯 auth JSON 账号回归：出站 Bearer 仍是 access_token")
}

// --- 出站 URL ---

func TestOpenAIChatCompletionsTargetURL_CodeBuddyAPIMode(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}

	got, err := svc.openAIChatCompletionsTargetURL(apiModeAccount(nil))
	require.NoError(t, err)
	assert.Equal(t, "http://relay.example/v1/chat/completions", got,
		"API 线路账号打中转的 OpenAI 路径，不是腾讯 /v2/chat/completions")

	oauth := &Account{ID: 802, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at", "base_url": "https://copilot.tencent.com"}}
	got, err = svc.openAIChatCompletionsTargetURL(oauth)
	require.NoError(t, err)
	assert.Equal(t, "https://copilot.tencent.com/v2/chat/completions", got,
		"腾讯 auth JSON 账号回归：端点仍是 base_url + /v2/chat/completions")
}

func TestOpenAIChatCompletionsTargetURL_CodeBuddyAPIModeRequiresBaseURL(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}

	acct := &Account{ID: 805, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-relay"}}
	_, err := svc.openAIChatCompletionsTargetURL(acct)
	require.Error(t, err, "API 线路缺少 base_url 时必须显式失败，不得回落腾讯默认域")
}
