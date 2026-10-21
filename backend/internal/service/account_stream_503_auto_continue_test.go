//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIStream503AutoContinueExtraKeyConstant(t *testing.T) {
	require.Equal(t, "openai_stream_503_auto_continue", OpenAIStream503AutoContinueExtraKey)
}

func TestIsOpenAIStream503AutoContinueEnabled(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{
			name:    "nil account",
			account: nil,
			want:    false,
		},
		{
			name:    "non-openai platform with key true",
			account: &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: true}},
			want:    false,
		},
		{
			name:    "nil extra",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: nil},
			want:    false,
		},
		{
			name:    "key missing",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_passthrough": true}},
			want:    false,
		},
		{
			name:    "string true is not a bool",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: "true"}},
			want:    false,
		},
		{
			name:    "int 1 is not a bool",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: int(1)}},
			want:    false,
		},
		{
			name:    "float64 1 is not a bool",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: float64(1)}},
			want:    false,
		},
		{
			name:    "explicit false",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: false}},
			want:    false,
		},
		{
			name:    "openai oauth with key true",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: true}},
			want:    true,
		},
		{
			name:    "openai setup-token with key true",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: true}},
			want:    true,
		},
		{
			name:    "openai apikey with key true is allowed by accessor; type gate lives in continuation state",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIStream503AutoContinueExtraKey: true}},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.IsOpenAIStream503AutoContinueEnabled())
		})
	}
}
