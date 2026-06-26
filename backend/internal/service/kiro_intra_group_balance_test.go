//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSelectAccount_IntraGroupBalance_SkipsStickyHit verifies that for a group
// flagged IntraGroupBalance, a sticky session binding is NOT honored — the
// request re-selects within the group by load/priority instead of returning the
// previously bound account. (Kiro 企业号组：会话粘组、组内分发，不绑单个成员。)
func TestSelectAccount_IntraGroupBalance_SkipsStickyHit(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1)
	sessionHash := "sess-A"

	// Two Kiro accounts in the group. Account 2 has higher priority (lower number).
	// Sticky is bound to account 1. With IntraGroupBalance, selection must bypass
	// sticky and pick the priority winner (account 2).
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformKiro, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			{ID: 2, Platform: PlatformKiro, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{sessionHash: 1}, // bound to account 1
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{
			groupID: {
				ID:                groupID,
				Platform:          PlatformKiro,
				Status:            StatusActive,
				Hydrated:          true,
				IntraGroupBalance: true, // <-- enterprise group
			},
		},
	}

	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false // legacy path

	svc := &GatewayService{
		accountRepo:        repo,
		groupRepo:          groupRepo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: nil,
	}

	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "", nil, "", int64(0))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, int64(2), result.Account.ID,
		"IntraGroupBalance: sticky binding to acct 1 must be bypassed; priority winner acct 2 selected")
}

// TestSelectAccount_NormalGroup_HonorsStickyHit is the control: the SAME setup
// without IntraGroupBalance must honor the sticky binding (return account 1).
func TestSelectAccount_NormalGroup_HonorsStickyHit(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1)
	sessionHash := "sess-A"

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformKiro, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			{ID: 2, Platform: PlatformKiro, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{sessionHash: 1},
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{
			groupID: {
				ID:                groupID,
				Platform:          PlatformKiro,
				Status:            StatusActive,
				Hydrated:          true,
				IntraGroupBalance: false, // normal group
			},
		},
	}

	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false

	svc := &GatewayService{
		accountRepo:        repo,
		groupRepo:          groupRepo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: nil,
	}

	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "", nil, "", int64(0))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, int64(1), result.Account.ID,
		"normal group: sticky binding to acct 1 must be honored")
}