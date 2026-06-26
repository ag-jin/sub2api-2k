package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// sha256Hex returns the lowercase hex sha256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// normalizeMachineID normalizes a configured machineId to a 64-char hex string,
// mirroring kiro.rs machine_id::normalize_machine_id.
//   - 64-char hex: used as-is
//   - UUID (32 hex after removing dashes): duplicated to 64 chars
//   - otherwise: not recognized (returns "", false)
func normalizeMachineID(machineID string) (string, bool) {
	trimmed := strings.TrimSpace(machineID)
	if len(trimmed) == 64 && isHex(trimmed) {
		return trimmed, true
	}
	withoutDashes := strings.ReplaceAll(trimmed, "-", "")
	if len(withoutDashes) == 32 && isHex(withoutDashes) {
		return withoutDashes + withoutDashes, true
	}
	return "", false
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

// EffectiveMachineID derives the per-credential device fingerprint, mirroring
// kiro.rs machine_id::generate_from_credentials. Each account gets a distinct,
// stable fingerprint so multiple accounts are not seen as the same device.
//
// Priority:
//  1. credential.MachineID (normalized)
//  2. derive from refreshToken: sha256("KotlinNativeAPI/" + refreshToken)
//  3. derive from accessToken as a last resort (process-stable-ish)
func (c *KiroCredential) EffectiveMachineID() string {
	if c.MachineID != "" {
		if norm, ok := normalizeMachineID(c.MachineID); ok {
			return norm
		}
	}
	if c.RefreshToken != "" {
		return sha256Hex("KotlinNativeAPI/" + c.RefreshToken)
	}
	if c.AccessToken != "" {
		return sha256Hex("KiroFallback/" + c.AccessToken)
	}
	// Absolute fallback: a constant is undesirable but only reached when a
	// credential has neither machineId nor any token (should not happen).
	return sha256Hex("KiroFallback/empty")
}
