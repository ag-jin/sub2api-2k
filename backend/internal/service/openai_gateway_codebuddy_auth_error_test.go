//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 401/403 的账号级错误文案必须按线路模式分流：
//   - 腾讯 auth JSON 账号：仍是「re-import the auth JSON」（原文不变）；
//   - API 线路账号（credentials.api_key 非空）：没有 auth JSON 可重录，
//     提示应为替换 credentials.api_key（否则运维照着提示做无效动作）。

func TestHandleCodeBuddyAccountUpstreamError_HintMatchesAPIMode(t *testing.T) {
	repo := &codebuddySetErrorRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	acct := apiModeAccount(nil)

	svc.handleCodeBuddyAccountUpstreamError(context.Background(), acct, http.StatusUnauthorized, "invalid api key")

	require.Equal(t, acct.ID, repo.errorAccountID, "401 应登记账号级错误态")
	assert.Contains(t, repo.errorMessage, "replace credentials.api_key on the account",
		"API 线路账号应提示替换 credentials.api_key")
	assert.NotContains(t, repo.errorMessage, "re-import the auth JSON",
		"API 线路账号没有 auth JSON 可重录，不得给出误导提示")
	assert.Contains(t, repo.errorMessage, "HTTP 401")
}

func TestHandleCodeBuddyAccountUpstreamError_OAuthHintUnchanged(t *testing.T) {
	repo := &codebuddySetErrorRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	oauth := &Account{ID: 802, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at", "refresh_token": "rt"}}

	svc.handleCodeBuddyAccountUpstreamError(context.Background(), oauth, http.StatusForbidden, "forbidden")

	require.Equal(t, oauth.ID, repo.errorAccountID, "403 应登记账号级错误态")
	assert.Contains(t, repo.errorMessage, "re-import the auth JSON on the account",
		"腾讯 auth JSON 账号文案保持原文不变")
	assert.Contains(t, repo.errorMessage, "HTTP 403")
}
