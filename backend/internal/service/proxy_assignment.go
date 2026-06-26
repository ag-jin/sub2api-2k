package service

import (
	"context"
	"fmt"
)

// KiroProxyCap is the per-egress account cap enforced for the Kiro platform:
// each proxy — and the server's own direct egress (slot 0) — may carry at most
// this many Kiro accounts, to keep multiple Kiro accounts from sharing one
// egress IP and being correlated. Also used as the default per-proxy cap for
// proxy auto-assignment.
const KiroProxyCap = 5

// ProxyAssignmentPlanner picks proxies for new accounts, enforcing a per-proxy
// per-platform cap and balancing load across proxies. It is seeded with the
// current per-proxy account counts for a platform and updated as it hands out
// assignments within a batch.
type ProxyAssignmentPlanner struct {
	cap        int             // max accounts per proxy for this platform (e.g. 5)
	proxyIDs   []int64         // active proxy IDs, stable order
	counts     map[int64]int   // proxyID -> current account count (this platform)
	Exhausted  bool            // set true once no proxy has spare capacity
}

// NewProxyAssignmentPlanner builds a planner for one platform.
//   - activeProxyIDs: IDs of ACTIVE proxies, in a stable order
//   - existingCounts: proxyID -> how many accounts of this platform already use it
//   - perProxyCap: max accounts per proxy per platform (5)
func NewProxyAssignmentPlanner(activeProxyIDs []int64, existingCounts map[int64]int, perProxyCap int) *ProxyAssignmentPlanner {
	counts := make(map[int64]int, len(activeProxyIDs))
	for _, id := range activeProxyIDs {
		counts[id] = existingCounts[id] // defaults to 0
	}
	return &ProxyAssignmentPlanner{
		cap:      perProxyCap,
		proxyIDs: activeProxyIDs,
		counts:   counts,
	}
}

// Next returns the slot to assign the next account to, plus ok=false if every
// slot is at the per-platform cap (caller should leave the account disabled).
//
// A slot ID of 0 is the SERVER HOST IP (no proxy / direct egress) - it is a
// first-class slot that participates in even-fill exactly like a real proxy,
// also capped at p.cap. ok=true with id=0 means "assign with no proxy".
// It picks the least-loaded slot still under the cap; ties broken by stable
// order for deterministic, even fill.
func (p *ProxyAssignmentPlanner) Next() (int64, bool) {
	best := int64(0)
	bestCount := int(^uint(0) >> 1) // max int
	found := false
	for _, id := range p.proxyIDs {
		c := p.counts[id]
		if c >= p.cap {
			continue
		}
		if c < bestCount {
			bestCount = c
			best = id
			found = true
		}
	}
	if !found {
		p.Exhausted = true
		return 0, false
	}
	p.counts[best]++
	return best, true
}

// SpareCapacity returns how many more accounts can still be placed across all
// proxies under the cap.
func (p *ProxyAssignmentPlanner) SpareCapacity() int {
	spare := 0
	for _, id := range p.proxyIDs {
		if r := p.cap - p.counts[id]; r > 0 {
			spare += r
		}
	}
	return spare
}

// buildPlatformProxyCounts returns proxyID -> count of accounts on `platform`
// currently bound to that proxy (only counts non-deleted accounts with a proxy).
func (s *adminServiceImpl) buildPlatformProxyCounts(ctx context.Context, platform string) (map[int64]int, error) {
	accounts, err := s.accountRepo.ListByPlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	counts := map[int64]int{}
	for i := range accounts {
		if accounts[i].ProxyID != nil {
			counts[*accounts[i].ProxyID]++
		} else {
			// No proxy bound => this account occupies the server host-IP slot (0).
			counts[0]++
		}
	}
	return counts, nil
}

// NewProxyPlannerForPlatform builds a planner seeded from the live DB state for
// the given platform, using all ACTIVE proxies and the given per-proxy cap.
func (s *adminServiceImpl) NewProxyPlannerForPlatform(ctx context.Context, platform string, perProxyCap int) (*ProxyAssignmentPlanner, error) {
	proxies, err := s.proxyRepo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	// Slot 0 = the server's own IP (no proxy). It is a first-class slot that
	// can carry up to perProxyCap accounts and participates in even-fill with
	// the real proxies, so the host IP is used like any other egress.
	ids := make([]int64, 0, len(proxies)+1)
	ids = append(ids, 0)
	for i := range proxies {
		ids = append(ids, proxies[i].ID)
	}
	counts, err := s.buildPlatformProxyCounts(ctx, platform)
	if err != nil {
		return nil, err
	}
	return NewProxyAssignmentPlanner(ids, counts, perProxyCap), nil
}

// egressSlotOf maps an account's ProxyID to its egress slot key: a real proxy
// ID, or 0 for the server's direct (no-proxy) egress.
func egressSlotOf(proxyID *int64) int64 {
	if proxyID == nil {
		return 0
	}
	return *proxyID
}

// checkKiroEgressCapacity enforces the Kiro per-egress account cap (KiroProxyCap)
// when creating or editing a Kiro account. targetProxyID is the egress the
// account will bind to (nil = server direct egress, slot 0). excludeAccountID is
// the account being edited (0 when creating) so it is not counted against itself.
//
// Returns an error if the target egress already holds KiroProxyCap Kiro accounts.
// Only enforced for the Kiro platform; other platforms are unaffected.
func (s *adminServiceImpl) checkKiroEgressCapacity(ctx context.Context, platform string, targetProxyID *int64, excludeAccountID int64) error {
	if platform != PlatformKiro {
		return nil
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, platform)
	if err != nil {
		return err
	}
	target := egressSlotOf(targetProxyID)
	count := 0
	for i := range accounts {
		if accounts[i].ID == excludeAccountID {
			continue
		}
		if egressSlotOf(accounts[i].ProxyID) == target {
			count++
		}
	}
	if count >= KiroProxyCap {
		if target == 0 {
			return fmt.Errorf("server direct egress already holds %d Kiro accounts (cap %d); bind this account to a proxy with spare capacity", count, KiroProxyCap)
		}
		return fmt.Errorf("proxy already holds %d Kiro accounts (cap %d); choose another proxy or the server direct egress", count, KiroProxyCap)
	}
	return nil
}
