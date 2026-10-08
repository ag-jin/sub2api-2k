package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 图床的 4 个键（gateway.image_bed.*）都必须在 setDefaults 里注册默认值，
// 否则纯 env 部署读不到它们（viper 只解码 AllKeys() 里的键——见
// env_reachability_test.go 的系统性护栏）；这里再钉一次「env 真的生效」。
func TestLoadImageBedFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_IMAGE_BED_ENABLED", "false")
	t.Setenv("GATEWAY_IMAGE_BED_MAX_BYTES", "2097152")
	t.Setenv("GATEWAY_IMAGE_BED_HOURLY_LIMIT_PER_KEY", "5")
	t.Setenv("GATEWAY_IMAGE_BED_TTL_HOURS", "6")

	cfg, err := Load()
	require.NoError(t, err)

	require.False(t, cfg.Gateway.ImageBed.Enabled)
	require.EqualValues(t, 2097152, cfg.Gateway.ImageBed.MaxBytes)
	require.Equal(t, 5, cfg.Gateway.ImageBed.HourlyLimitPerKey)
	require.Equal(t, 6, cfg.Gateway.ImageBed.TTLHours)
}

// 票面默认值：开关默认开启、单文件 10MiB、每 key 每小时 60 张、24h TTL。
func TestImageBedDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)

	require.True(t, cfg.Gateway.ImageBed.Enabled)
	require.EqualValues(t, 10<<20, cfg.Gateway.ImageBed.MaxBytes)
	require.Equal(t, 60, cfg.Gateway.ImageBed.HourlyLimitPerKey)
	require.Equal(t, 24, cfg.Gateway.ImageBed.TTLHours)
}
