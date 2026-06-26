package service

import (
	"context"
	"testing"
)

// kiroCapRepoStub implements just enough of AccountRepository for the egress
// cap check (only ListByPlatform is exercised).
type kiroCapRepoStub struct {
	AccountRepository // embed nil interface; only ListByPlatform is called
	accounts          []Account
}

func (r *kiroCapRepoStub) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	out := make([]Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		if a.Platform == platform {
			out = append(out, a)
		}
	}
	return out, nil
}

func kiroCapPtr(v int64) *int64 { return &v }

func mkKiroAcct(id int64, proxyID *int64) Account {
	return Account{ID: id, Platform: PlatformKiro, ProxyID: proxyID}
}

func TestCheckKiroEgressCapacity(t *testing.T) {
	ctx := context.Background()

	t.Run("proxy under cap allows", func(t *testing.T) {
		repo := &kiroCapRepoStub{accounts: []Account{
			mkKiroAcct(1, kiroCapPtr(7)),
			mkKiroAcct(2, kiroCapPtr(7)),
		}}
		svc := &adminServiceImpl{accountRepo: repo}
		if err := svc.checkKiroEgressCapacity(ctx, PlatformKiro, kiroCapPtr(7), 0); err != nil {
			t.Errorf("expected allow (2<5), got %v", err)
		}
	})

	t.Run("proxy at cap rejects", func(t *testing.T) {
		accts := make([]Account, 0, 5)
		for i := int64(1); i <= 5; i++ {
			accts = append(accts, mkKiroAcct(i, kiroCapPtr(7)))
		}
		repo := &kiroCapRepoStub{accounts: accts}
		svc := &adminServiceImpl{accountRepo: repo}
		if err := svc.checkKiroEgressCapacity(ctx, PlatformKiro, kiroCapPtr(7), 0); err == nil {
			t.Error("expected reject at cap (5>=5)")
		}
	})

	t.Run("direct egress (nil) capped at slot 0", func(t *testing.T) {
		accts := make([]Account, 0, 5)
		for i := int64(1); i <= 5; i++ {
			accts = append(accts, mkKiroAcct(i, nil)) // all direct egress
		}
		repo := &kiroCapRepoStub{accounts: accts}
		svc := &adminServiceImpl{accountRepo: repo}
		if err := svc.checkKiroEgressCapacity(ctx, PlatformKiro, nil, 0); err == nil {
			t.Error("expected reject: direct egress slot full")
		}
	})

	t.Run("exclude self on update", func(t *testing.T) {
		accts := make([]Account, 0, 5)
		for i := int64(1); i <= 5; i++ {
			accts = append(accts, mkKiroAcct(i, kiroCapPtr(7)))
		}
		repo := &kiroCapRepoStub{accounts: accts}
		svc := &adminServiceImpl{accountRepo: repo}
		// editing account 3 (already on proxy 7) -> excluded, count=4 < 5 -> allow
		if err := svc.checkKiroEgressCapacity(ctx, PlatformKiro, kiroCapPtr(7), 3); err != nil {
			t.Errorf("expected allow when excluding self, got %v", err)
		}
	})

	t.Run("non-kiro platform skipped", func(t *testing.T) {
		accts := make([]Account, 0, 10)
		for i := int64(1); i <= 10; i++ {
			a := mkKiroAcct(i, kiroCapPtr(7))
			a.Platform = "openai"
			accts = append(accts, a)
		}
		repo := &kiroCapRepoStub{accounts: accts}
		svc := &adminServiceImpl{accountRepo: repo}
		if err := svc.checkKiroEgressCapacity(ctx, "openai", kiroCapPtr(7), 0); err != nil {
			t.Errorf("non-kiro must be skipped, got %v", err)
		}
	})
}
