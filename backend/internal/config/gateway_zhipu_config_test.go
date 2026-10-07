//go:build unit

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadGatewayZhipuDefaults pins every gateway.zhipu key. A key without a
// registered default is unreachable from the environment (see
// TestConfigKeysAreEnvReachable) and silently changes behaviour, so each
// documented default is asserted explicitly here.
func TestLoadGatewayZhipuDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)

	zhipu := cfg.Gateway.Zhipu
	cases := []struct {
		key  string
		got  any
		want any
	}{
		{"sign_v4_enabled", zhipu.SignV4Enabled, true}, // 主会话裁决：默认 ON（issues/00 依赖图）
		{"sign_client_version", zhipu.SignClientVersion, "0.16.9"},
		{"sign_key_ttl_minutes", zhipu.SignKeyTTLMinutes, 1440},
		{"credential_check_interval_minutes", zhipu.CredentialCheckIntervalMinutes, 60},
		{"zcode_min_call_interval_seconds", zhipu.ZCodeMinCallIntervalSeconds, 30},
		{"reset_status_cache_minutes", zhipu.ResetStatusCacheMinutes, 10},
		{"sign_fail_policy", zhipu.SignFailPolicy, "open"},
		{"sign_alert_enabled", zhipu.SignAlertEnabled, true},
		{"sign_reconcile_interval_hours", zhipu.SignReconcileIntervalHours, 6},
		{"sign_reconcile_deviation_threshold", zhipu.SignReconcileDeviationThreshold, 0.70},
		{"sign_pow_bits", zhipu.SignPowBits, 8},
		{"sign_handshake_backoff_seconds", zhipu.SignHandshakeBackoffSeconds, 30},
		{"sign_account_circuit_break_threshold", zhipu.SignAccountCircuitBreakThreshold, 10},
		// 视觉桥（票 #35）：默认开启，桥模型与盲模型集默认互斥（glm-5.3-flash ∉ 盲集）。
		{"vision_bridge.enabled", zhipu.VisionBridge.Enabled, true},
		{"vision_bridge.model", zhipu.VisionBridge.Model, DefaultZhipuVisionBridgeModel},
		{"vision_bridge.blind_models", zhipu.VisionBridge.BlindModels, DefaultZhipuVisionBridgeBlindModels},
		{"vision_bridge.max_images", zhipu.VisionBridge.MaxImages, DefaultZhipuVisionBridgeMaxImages},
		{"vision_bridge.budget_seconds", zhipu.VisionBridge.BudgetSeconds, DefaultZhipuVisionBridgeBudgetSeconds},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.key, func(t *testing.T) {
			require.Equal(t, tc.want, tc.got)
		})
	}
}
