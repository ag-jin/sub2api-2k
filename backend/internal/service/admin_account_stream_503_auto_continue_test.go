//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestValidateOpenAIStream503AutoContinueExtra(t *testing.T) {
	tests := []struct {
		name           string
		platform       string
		extra          map[string]any
		wantBadRequest bool
	}{
		{
			name:     "non OpenAI ignores provider owned value",
			platform: PlatformAnthropic,
			extra:    map[string]any{openAIStream503AutoContinueKey: "provider-owned"},
		},
		{
			name:     "OpenAI missing key is accepted",
			platform: PlatformOpenAI,
			extra:    map[string]any{"other": true},
		},
		{
			name:     "OpenAI nil extra is accepted",
			platform: PlatformOpenAI,
		},
		{
			name:     "OpenAI true is accepted",
			platform: PlatformOpenAI,
			extra:    map[string]any{openAIStream503AutoContinueKey: true},
		},
		{
			name:     "OpenAI false is accepted",
			platform: PlatformOpenAI,
			extra:    map[string]any{openAIStream503AutoContinueKey: false},
		},
		{
			name:           "OpenAI string is rejected",
			platform:       PlatformOpenAI,
			extra:          map[string]any{openAIStream503AutoContinueKey: "true"},
			wantBadRequest: true,
		},
		{
			name:           "OpenAI number is rejected",
			platform:       PlatformOpenAI,
			extra:          map[string]any{openAIStream503AutoContinueKey: 1},
			wantBadRequest: true,
		},
		{
			name:           "OpenAI null is rejected",
			platform:       PlatformOpenAI,
			extra:          map[string]any{openAIStream503AutoContinueKey: nil},
			wantBadRequest: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOpenAIStream503AutoContinueExtra(tt.platform, tt.extra)

			if !tt.wantBadRequest {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", infraerrors.Reason(err))
		})
	}
}

func TestNormalizeOpenAIStream503AutoContinueExtra(t *testing.T) {
	t.Run("OpenAI missing key persists disabled default", func(t *testing.T) {
		extra, err := normalizeOpenAIStream503AutoContinueExtra(PlatformOpenAI, nil)

		require.NoError(t, err)
		require.Equal(t, false, extra[openAIStream503AutoContinueKey])
	})

	t.Run("OpenAI explicit true is preserved", func(t *testing.T) {
		extra, err := normalizeOpenAIStream503AutoContinueExtra(PlatformOpenAI, map[string]any{openAIStream503AutoContinueKey: true})

		require.NoError(t, err)
		require.Equal(t, true, extra[openAIStream503AutoContinueKey])
	})

	t.Run("OpenAI explicit false is preserved", func(t *testing.T) {
		extra, err := normalizeOpenAIStream503AutoContinueExtra(PlatformOpenAI, map[string]any{openAIStream503AutoContinueKey: false})

		require.NoError(t, err)
		require.Equal(t, false, extra[openAIStream503AutoContinueKey])
	})

	t.Run("OpenAI malformed value is rejected", func(t *testing.T) {
		extra, err := normalizeOpenAIStream503AutoContinueExtra(PlatformOpenAI, map[string]any{openAIStream503AutoContinueKey: "false"})

		require.Nil(t, extra)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", infraerrors.Reason(err))
	})

	t.Run("non OpenAI nil extra is unchanged", func(t *testing.T) {
		extra, err := normalizeOpenAIStream503AutoContinueExtra(PlatformGrok, nil)

		require.NoError(t, err)
		require.Nil(t, extra)
	})

	t.Run("non OpenAI malformed value is ignored", func(t *testing.T) {
		original := map[string]any{openAIStream503AutoContinueKey: "provider-owned"}
		extra, err := normalizeOpenAIStream503AutoContinueExtra(PlatformAnthropic, original)

		require.NoError(t, err)
		require.Equal(t, original, extra)
	})
}

func TestNormalizeOpenAIStream503AutoContinueUpdateExtra(t *testing.T) {
	t.Run("omitted key preserves previous enabled value", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIStream503AutoContinueKey: true}}

		extra, err := normalizeOpenAIStream503AutoContinueUpdateExtra(account, &UpdateAccountInput{Extra: map[string]any{"other": "value"}}, map[string]any{"other": "value"})

		require.NoError(t, err)
		require.Equal(t, true, extra[openAIStream503AutoContinueKey])
	})

	t.Run("omitted key preserves previous disabled value", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIStream503AutoContinueKey: false}}

		extra, err := normalizeOpenAIStream503AutoContinueUpdateExtra(account, &UpdateAccountInput{Extra: map[string]any{}}, map[string]any{})

		require.NoError(t, err)
		require.Equal(t, false, extra[openAIStream503AutoContinueKey])
	})

	t.Run("explicit true overrides previous disabled value", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIStream503AutoContinueKey: false}}

		extra, err := normalizeOpenAIStream503AutoContinueUpdateExtra(account, &UpdateAccountInput{Extra: map[string]any{openAIStream503AutoContinueKey: true}}, map[string]any{openAIStream503AutoContinueKey: true})

		require.NoError(t, err)
		require.Equal(t, true, extra[openAIStream503AutoContinueKey])
	})

	t.Run("explicit false overrides previous enabled value", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIStream503AutoContinueKey: true}}

		extra, err := normalizeOpenAIStream503AutoContinueUpdateExtra(account, &UpdateAccountInput{Extra: map[string]any{openAIStream503AutoContinueKey: false}}, map[string]any{openAIStream503AutoContinueKey: false})

		require.NoError(t, err)
		require.Equal(t, false, extra[openAIStream503AutoContinueKey])
	})

	t.Run("malformed value is rejected", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIStream503AutoContinueKey: true}}

		extra, err := normalizeOpenAIStream503AutoContinueUpdateExtra(account, &UpdateAccountInput{Extra: map[string]any{openAIStream503AutoContinueKey: "true"}}, map[string]any{})

		require.Nil(t, extra)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", infraerrors.Reason(err))
	})

	t.Run("non OpenAI extra is unchanged", func(t *testing.T) {
		account := &Account{Platform: PlatformAnthropic}
		original := map[string]any{openAIStream503AutoContinueKey: "provider-owned"}

		extra, err := normalizeOpenAIStream503AutoContinueUpdateExtra(account, &UpdateAccountInput{Extra: original}, original)

		require.NoError(t, err)
		require.Equal(t, original, extra)
	})
}

type stream503AutoContinueRepoStub struct {
	accountRepoStub
	account          *Account
	createdAccount   *Account
	updateExtraCalls int
}

func (r *stream503AutoContinueRepoStub) Create(_ context.Context, account *Account) error {
	account.ID = 1
	r.createdAccount = account
	return nil
}

func (r *stream503AutoContinueRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

func (r *stream503AutoContinueRepoStub) GetByIDs(_ context.Context, _ []int64) ([]*Account, error) {
	if r.account == nil {
		return nil, nil
	}
	return []*Account{r.account}, nil
}

func (r *stream503AutoContinueRepoStub) Update(_ context.Context, account *Account) error {
	r.account = account
	return nil
}

func (r *stream503AutoContinueRepoStub) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	r.updateExtraCalls++
	return nil
}

func TestAdminServiceCreateAccountDefaultsOpenAIStream503AutoContinueDisabled(t *testing.T) {
	repo := &stream503AutoContinueRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo}

	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "openai-account",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "test"},
		SkipDefaultGroupBind: true,
	})

	require.NoError(t, err)
	require.Same(t, account, repo.createdAccount)
	require.Equal(t, false, account.Extra[openAIStream503AutoContinueKey])
}

func TestAdminServiceCreateAccountRejectsMalformedOpenAIStream503AutoContinueValue(t *testing.T) {
	repo := &stream503AutoContinueRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo}

	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{openAIStream503AutoContinueKey: "false"},
	})

	require.Nil(t, account)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", infraerrors.Reason(err))
	require.Nil(t, repo.createdAccount)
}

func TestAdminServiceUpdateAccountPreservesOpenAIStream503AutoContinueWhenOmitted(t *testing.T) {
	for _, previous := range []bool{true, false} {
		t.Run("previous "+map[bool]string{true: "enabled", false: "disabled"}[previous], func(t *testing.T) {
			repo := &stream503AutoContinueRepoStub{account: &Account{
				ID:       1,
				Platform: PlatformOpenAI,
				Type:     AccountTypeAPIKey,
				Extra:    map[string]any{openAIStream503AutoContinueKey: previous},
			}}
			svc := &adminServiceImpl{accountRepo: repo}

			account, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: map[string]any{}})

			require.NoError(t, err)
			require.Equal(t, previous, account.Extra[openAIStream503AutoContinueKey])
		})
	}
}

func TestAdminServiceUpdateAccountRejectsMalformedOpenAIStream503AutoContinueValue(t *testing.T) {
	repo := &stream503AutoContinueRepoStub{account: &Account{ID: 1, Platform: PlatformOpenAI}}
	svc := &adminServiceImpl{accountRepo: repo}

	account, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: map[string]any{
		openAIStream503AutoContinueKey: 1,
	}})

	require.Nil(t, account)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", infraerrors.Reason(err))
}

func TestAdminServiceUpdateAccountExtraRejectsMalformedOpenAIStream503AutoContinueValue(t *testing.T) {
	repo := &stream503AutoContinueRepoStub{account: &Account{ID: 1, Platform: PlatformOpenAI}}
	svc := &adminServiceImpl{accountRepo: repo}

	err := svc.UpdateAccountExtra(context.Background(), 1, map[string]any{
		openAIStream503AutoContinueKey: "true",
	})

	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", infraerrors.Reason(err))
	require.Zero(t, repo.updateExtraCalls)
}

func TestAdminServiceUpdateAccountExtraAllowsProviderOwnedStream503ValueForNonOpenAIAccount(t *testing.T) {
	repo := &stream503AutoContinueRepoStub{account: &Account{ID: 1, Platform: PlatformAnthropic}}
	svc := &adminServiceImpl{accountRepo: repo}

	err := svc.UpdateAccountExtra(context.Background(), 1, map[string]any{
		openAIStream503AutoContinueKey: "provider-owned",
	})

	require.NoError(t, err)
	require.Equal(t, 1, repo.updateExtraCalls)
}

func TestDuplicateAccountDefaultsOpenAIStream503AutoContinueDisabled(t *testing.T) {
	ctx := context.Background()
	repo := newDuplicateAccountRepoStub()
	svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
	source := &Account{
		Name:        "source",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "secret"},
	}
	require.NoError(t, repo.Create(ctx, source))

	duplicate, err := svc.DuplicateAccount(ctx, source.ID, "admin:1", "")

	require.NoError(t, err)
	require.Equal(t, false, duplicate.Extra[openAIStream503AutoContinueKey])
}
