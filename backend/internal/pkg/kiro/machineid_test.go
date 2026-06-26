package kiro

import "testing"

func TestEffectiveMachineID_ConfiguredUUID(t *testing.T) {
	// UUID format should be normalized to 64-char hex (dashes removed, duplicated)
	c := &KiroCredential{MachineID: "4ae860d0-1f2e-4569-9b5e-432d141252af"}
	mid := c.EffectiveMachineID()
	if len(mid) != 64 {
		t.Errorf("expected 64-char hex, got %d: %s", len(mid), mid)
	}
	if !isHex(mid) {
		t.Errorf("not hex: %s", mid)
	}
	// deterministic
	if c.EffectiveMachineID() != mid {
		t.Error("not deterministic")
	}
}

func TestEffectiveMachineID_Configured64Hex(t *testing.T) {
	h := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	c := &KiroCredential{MachineID: h}
	if c.EffectiveMachineID() != h {
		t.Errorf("64-hex should be used as-is, got %s", c.EffectiveMachineID())
	}
}

func TestEffectiveMachineID_DerivedFromRefreshToken(t *testing.T) {
	// No machineId -> derive from refreshToken, NOT "unknown"
	c := &KiroCredential{RefreshToken: "some-refresh-token-value-1234567890"}
	mid := c.EffectiveMachineID()
	if mid == "unknown" || mid == "" {
		t.Fatalf("should derive a fingerprint, got %q", mid)
	}
	if len(mid) != 64 {
		t.Errorf("derived id should be 64-char sha256 hex, got %d", len(mid))
	}
	// Must match kiro.rs formula sha256("KotlinNativeAPI/" + rt)
	want := sha256Hex("KotlinNativeAPI/some-refresh-token-value-1234567890")
	if mid != want {
		t.Errorf("derivation mismatch:\n got=%s\nwant=%s", mid, want)
	}
}

func TestEffectiveMachineID_DistinctPerAccount(t *testing.T) {
	// Two accounts without machineId must get DIFFERENT fingerprints
	a := &KiroCredential{RefreshToken: "token-account-A"}
	b := &KiroCredential{RefreshToken: "token-account-B"}
	if a.EffectiveMachineID() == b.EffectiveMachineID() {
		t.Error("different accounts must have distinct machine IDs (isolation!)")
	}
}
