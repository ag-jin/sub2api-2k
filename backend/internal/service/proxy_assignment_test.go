package service

import "testing"

// With the host-IP slot (0) as a first-class slot, NewProxyAssignmentPlanner is
// still tested directly by passing the slot list explicitly (0 must be included
// by callers; the constructor does not inject it).

func TestProxyPlanner_HostIPSlotEvenFill(t *testing.T) {
	// slots: host-IP(0) + 1 real proxy, cap 5 => 10 total.
	p := NewProxyAssignmentPlanner([]int64{0, 1}, map[int64]int{}, 5)
	hostCount, proxyCount := 0, 0
	for i := 0; i < 10; i++ {
		id, ok := p.Next()
		if !ok {
			t.Fatalf("unexpected exhaustion at %d", i)
		}
		if id == 0 {
			hostCount++
		} else {
			proxyCount++
		}
	}
	if hostCount != 5 || proxyCount != 5 {
		t.Errorf("even fill expected 5/5, got host=%d proxy=%d", hostCount, proxyCount)
	}
	// 11th must be refused (all slots full)
	if _, ok := p.Next(); ok {
		t.Error("11th assignment should be refused")
	}
	if !p.Exhausted {
		t.Error("planner should be exhausted")
	}
}

func TestProxyPlanner_HostIPOnly(t *testing.T) {
	// No real proxies: only host-IP slot (0), cap 5 => first 5 use host IP, 6th disabled.
	p := NewProxyAssignmentPlanner([]int64{0}, map[int64]int{}, 5)
	for i := 0; i < 5; i++ {
		id, ok := p.Next()
		if !ok || id != 0 {
			t.Fatalf("assignment %d: expected host-IP slot (0,true), got (%d,%v)", i, id, ok)
		}
	}
	if _, ok := p.Next(); ok {
		t.Error("6th account should be refused (host IP full)")
	}
}

func TestProxyPlanner_HostIPSeedsFromExisting(t *testing.T) {
	// host IP already has 4 (proxy_id NULL accounts), real proxy empty, cap 5.
	// Next 2 should prefer the empty real proxy first (least-loaded).
	p := NewProxyAssignmentPlanner([]int64{0, 1}, map[int64]int{0: 4, 1: 0}, 5)
	id1, _ := p.Next()
	id2, _ := p.Next()
	if id1 != 1 || id2 != 1 {
		t.Errorf("least-loaded real proxy should fill first, got %d,%d", id1, id2)
	}
}
