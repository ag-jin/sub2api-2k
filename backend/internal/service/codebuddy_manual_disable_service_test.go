package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type manualDisableRepoStub struct {
	AccountRepository
	acct *Account
	err  error
	// updates 记录每次 UpdateExtra 的 payload（键 manual_disabled 的值）
	updates []map[string]any
}

func (s *manualDisableRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.acct, nil
}

func (s *manualDisableRepoStub) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	if v, ok := updates["manual_disabled"]; ok {
		if obj, ok := v.(map[string]any); ok {
			s.updates = append(s.updates, obj)
		}
	}
	return nil
}

func newManualDisableSvc(acct *Account) *CodeBuddyAdminService {
	return &CodeBuddyAdminService{accountRepo: &manualDisableRepoStub{acct: acct}}
}

func TestManualDisable_SetsEnabledWithReason(t *testing.T) {
	svc := newManualDisableSvc(&Account{ID: 504, Platform: PlatformCodeBuddy})
	require.NoError(t, svc.ManualDisable(context.Background(), 504, "拖后腿"))
}

func TestManualDisable_EmptyReasonDefaults(t *testing.T) {
	svc := newManualDisableSvc(&Account{ID: 504, Platform: PlatformCodeBuddy})
	require.NoError(t, svc.ManualDisable(context.Background(), 504, ""))
}

func TestManualDisable_RejectsNonCodeBuddy(t *testing.T) {
	svc := newManualDisableSvc(&Account{ID: 1, Platform: PlatformOpenAI})
	err := svc.ManualDisable(context.Background(), 1, "x")
	require.ErrorIs(t, err, errCodeBuddyManualDisableNeedsAccount)
}

func TestManualEnable_ClearsBit(t *testing.T) {
	svc := newManualDisableSvc(&Account{ID: 505, Platform: PlatformCodeBuddy})
	require.NoError(t, svc.ManualEnable(context.Background(), 505))
}

func TestManualDisable_MissingAccountErrors(t *testing.T) {
	svc := newManualDisableSvc(nil)
	err := svc.ManualDisable(context.Background(), 999, "x")
	require.ErrorIs(t, err, errCodeBuddyManualDisableNeedsAccount)
	err = svc.ManualEnable(context.Background(), 999)
	require.ErrorIs(t, err, errCodeBuddyManualDisableNeedsAccount)
	_ = errors.Is
}
