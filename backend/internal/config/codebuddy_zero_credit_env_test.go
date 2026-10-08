package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 票 #37（CodeBuddy 0 积分主动门）：两个键都必须在 setDefaults 里注册默认值，
// 否则纯 env 部署读不到它们（见 env_reachability_test.go 的系统性护栏）；
// 这里再钉一次「默认 = 开启 + 30 分钟」，以及 env 真的生效。
func TestCodeBuddyZeroCreditGateDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)

	// 默认开启：0 积分停调度是用户明确要求的行为，装好即生效。
	require.True(t, cfg.Gateway.CodeBuddy.ZeroCreditGateEnabled)
	require.Equal(t, 30, cfg.Gateway.CodeBuddy.ZeroCreditCheckIntervalMinutes)
}

func TestLoadCodeBuddyZeroCreditGateFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_CODEBUDDY_ZERO_CREDIT_GATE_ENABLED", "false")
	t.Setenv("GATEWAY_CODEBUDDY_ZERO_CREDIT_CHECK_INTERVAL_MINUTES", "45")

	cfg, err := Load()
	require.NoError(t, err)

	require.False(t, cfg.Gateway.CodeBuddy.ZeroCreditGateEnabled)
	require.Equal(t, 45, cfg.Gateway.CodeBuddy.ZeroCreditCheckIntervalMinutes)
}
