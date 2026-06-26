package kiro

import "testing"

func TestEffectiveSystemVersion_StablePerAccount(t *testing.T) {
	c := &KiroCredential{MachineID: "4ae860d0-1f2e-4569-9b5e-432d141252af"}
	cfg := DefaultKiroConfig()
	v1 := c.EffectiveSystemVersion(cfg)
	v2 := c.EffectiveSystemVersion(cfg)
	if v1 != v2 {
		t.Errorf("same account must get stable OS: %s vs %s", v1, v2)
	}
	// must be a real Kiro OS string
	ok := false
	for _, s := range systemVersions {
		if s == v1 {
			ok = true
		}
	}
	if !ok {
		t.Errorf("OS %q not in known systemVersions", v1)
	}
}

func TestEffectiveSystemVersion_VariesAcrossAccounts(t *testing.T) {
	cfg := DefaultKiroConfig()
	// Generate many distinct accounts; expect BOTH darwin and win32 to appear,
	// proving accounts are not all pinned to one OS.
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		c := &KiroCredential{RefreshToken: "refresh-token-account-" + string(rune('A'+i%26)) + string(rune('0'+i/26))}
		seen[c.EffectiveSystemVersion(cfg)]++
	}
	if len(seen) < 2 {
		t.Errorf("expected a mix of OS across accounts, got only: %v", seen)
	}
	t.Logf("OS distribution across accounts: %v", seen)
}

func TestEffectiveSystemVersion_ExplicitConfigWins(t *testing.T) {
	c := &KiroCredential{MachineID: "abc"}
	cfg := KiroConfig{SystemVersion: "darwin#24.6.0"}
	if c.EffectiveSystemVersion(cfg) != "darwin#24.6.0" {
		t.Error("explicit config SystemVersion should win")
	}
}

func TestDefaultConfig_NoGlobalOSPin(t *testing.T) {
	// The default config must NOT pin a single OS for all accounts.
	cfg := DefaultKiroConfig()
	if cfg.SystemVersion != "" {
		t.Errorf("DefaultKiroConfig should leave SystemVersion empty (per-account), got %q", cfg.SystemVersion)
	}
}
