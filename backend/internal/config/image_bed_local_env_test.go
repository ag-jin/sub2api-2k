package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 本地磁盘兜底（无 S3 时站点自己当图床）的三个键都必须在 setDefaults 里注册默认值，
// 否则纯 env 部署读不到它们（见 env_reachability_test.go 的系统性护栏）；
// 这里再钉一次「默认值 = 无 S3 自动兜底」，以及 env 真的生效。
func TestImageBedLocalDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)

	// 默认开启：没有 S3 的部署上传即走本地磁盘，不需要先改配置。
	require.True(t, cfg.Gateway.ImageBed.LocalEnabled)
	// 留空 = 由 DATA_DIR 派生（<DATA_DIR>/image-bed），不写死绝对路径。
	require.Empty(t, cfg.Gateway.ImageBed.LocalDir)
	// 留空 = Save 返回站点内的相对直链，部署时再补公开域名前缀。
	require.Empty(t, cfg.Gateway.ImageBed.PublicBaseURL)
}

func TestLoadImageBedLocalFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_IMAGE_BED_LOCAL_ENABLED", "false")
	t.Setenv("GATEWAY_IMAGE_BED_LOCAL_DIR", "/var/lib/sub2api/image-bed")
	t.Setenv("GATEWAY_IMAGE_BED_PUBLIC_BASE_URL", "https://www.facaiai.top")

	cfg, err := Load()
	require.NoError(t, err)

	require.False(t, cfg.Gateway.ImageBed.LocalEnabled)
	require.Equal(t, "/var/lib/sub2api/image-bed", cfg.Gateway.ImageBed.LocalDir)
	require.Equal(t, "https://www.facaiai.top", cfg.Gateway.ImageBed.PublicBaseURL)
}
