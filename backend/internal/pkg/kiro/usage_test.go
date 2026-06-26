package kiro

import "testing"

// TestEffectiveTotalLimit verifies the two-tier total quota: base + overage cap
// when overage is enabled, base alone otherwise. Mirrors real Kiro getUsageLimits
// (account 322: base 1000, cap 10000, used 8235 → total 11000 / ~75%, not 823%).
func TestEffectiveTotalLimit(t *testing.T) {
	mk := func(base, cap int64, overageStatus string) *UsageLimitsResponse {
		r := &UsageLimitsResponse{
			UsageBreakdownList: []UsageBreakdown{{UsageLimit: base, OverageCap: cap}},
		}
		if overageStatus != "" {
			r.OverageConfiguration = &OverageConfiguration{OverageStatus: overageStatus}
		}
		return r
	}

	cases := []struct {
		name string
		r    *UsageLimitsResponse
		want float64
	}{
		{"overage enabled → base+cap", mk(1000, 10000, "ENABLED"), 11000},
		{"overage disabled → base only", mk(1000, 10000, "DISABLED"), 1000},
		{"no overage config → base only", mk(1000, 10000, ""), 1000},
		{"enabled but zero cap → base", mk(1000, 0, "ENABLED"), 1000},
		{"empty breakdown → 0", &UsageLimitsResponse{}, 0},
	}
	for _, c := range cases {
		if got := c.r.EffectiveTotalLimit(); got != c.want {
			t.Errorf("%s: EffectiveTotalLimit()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestEffectiveTotalLimit_Precision verifies the *WithPrecision base is preferred.
func TestEffectiveTotalLimit_Precision(t *testing.T) {
	r := &UsageLimitsResponse{
		OverageConfiguration: &OverageConfiguration{OverageStatus: "ENABLED"},
		UsageBreakdownList: []UsageBreakdown{{
			UsageLimit:              1000,
			UsageLimitWithPrecision: 1000.5,
			OverageCap:              10000,
		}},
	}
	if got := r.EffectiveTotalLimit(); got != 11000.5 {
		t.Errorf("EffectiveTotalLimit with precision = %v want 11000.5", got)
	}
}
