//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// codeBuddyCreateRepoStub 记录 Create 调用。
type codeBuddyCreateRepoStub struct {
	accountRepoStub
	created *Account
}

func (s *codeBuddyCreateRepoStub) Create(_ context.Context, account *Account) error {
	s.created = account
	return nil
}

// TestAccountService_Create_CodeBuddyTypeLocked 校验 platform=codebuddy
// 创建账号时 type 必须为 apikey（oauth 型会被网关 GetAccessToken 走
// openAITokenProvider 分派而失败， design v3 补丁 D2）。
func TestAccountService_Create_CodeBuddyTypeLocked(t *testing.T) {
	repo := &codeBuddyCreateRepoStub{}
	svc := &AccountService{accountRepo: repo}

	t.Run("oauth rejected", func(t *testing.T) {
		_, err := svc.Create(context.Background(), CreateAccountRequest{
			Name:        "cb",
			Platform:    PlatformCodeBuddy,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"auth": map[string]any{"accessToken": "at"}},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "type=apikey")
		require.Nil(t, repo.created, "非法 type 不应落库")
	})

	t.Run("apikey allowed", func(t *testing.T) {
		repo.created = nil
		acc, err := svc.Create(context.Background(), CreateAccountRequest{
			Name:        "cb",
			Platform:    PlatformCodeBuddy,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{"auth": map[string]any{"accessToken": "at"}},
		})
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, AccountTypeAPIKey, acc.Type)
		require.Equal(t, PlatformCodeBuddy, acc.Platform)
	})
}
