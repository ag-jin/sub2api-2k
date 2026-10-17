package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsSubscriptionPlanType(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"plan_type":     "plus",
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
	})

	require.Equal(t, "plus", filtered["plan_type"])
	require.NotContains(t, filtered, "access_token")
	require.NotContains(t, filtered, "refresh_token")
}

func TestFilterSchedulerCredentialsKeepsGeminiExplicitCacheKeys(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"api_key":              "k",
		"base_url":             "https://gw.example.com",
		"auth_mode":            "apikey",
		"service_account_json": "secret",
		service.GeminiExplicitCacheUpstreamCredentialKey:       true,
		service.GeminiExplicitCacheUpstreamMaxTTLCredentialKey: 3600,
	})

	require.Equal(t, "https://gw.example.com", filtered["base_url"])
	require.Equal(t, "apikey", filtered["auth_mode"])
	require.Equal(t, true, filtered[service.GeminiExplicitCacheUpstreamCredentialKey])
	require.Equal(t, 3600, filtered[service.GeminiExplicitCacheUpstreamMaxTTLCredentialKey])
	require.NotContains(t, filtered, "service_account_json")
}

func TestSchedulerMetadataAccountKeepsOpenAISubscriptionIdentity(t *testing.T) {
	account := service.Account{
		ID:       24,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type":    "plus",
			"access_token": "secret-access-token",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.True(t, metadata.IsOpenAIChatGPTSubscription())
	require.Empty(t, metadata.GetCredential("access_token"))
}

func TestSchedulerMetadataAccountProjectsUpstreamBillingProbe(t *testing.T) {
	lastError := strings.Repeat("upstream diagnostic ", 512)
	probe := map[string]any{
		"status": "ok",
		"data": map[string]any{
			"billing_scope":             "token",
			"resolved_rate_multiplier":  0.03,
			"peak_rate_enabled":         true,
			"peak_start":                "09:00",
			"peak_end":                  "18:00",
			"peak_rate_multiplier":      2.0,
			"timezone":                  "Asia/Shanghai",
			"effective_rate_multiplier": 0.03,
			"remote_diagnostic":         lastError,
		},
		"received_at":   "2026-07-13T10:00:00Z",
		"fresh_until":   "2026-07-13T11:00:00Z",
		"next_probe_at": "2026-07-13T10:30:00Z",
		"http_status":   502,
		"last_error":    lastError,
	}
	account := service.Account{
		ID: 42,
		Extra: map[string]any{
			"upstream_billing_probe": probe,
			"unused_large_field":     "drop-me",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	filtered, ok := metadata.Extra["upstream_billing_probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", filtered["status"])
	require.Equal(t, "2026-07-13T10:00:00Z", filtered["received_at"])
	require.Equal(t, "2026-07-13T11:00:00Z", filtered["fresh_until"])
	require.Equal(t, "2026-07-13T10:30:00Z", filtered["next_probe_at"])
	require.NotContains(t, filtered, "http_status")
	require.NotContains(t, filtered, "last_error")
	filteredData, ok := filtered["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", filteredData["billing_scope"])
	require.Equal(t, 0.03, filteredData["resolved_rate_multiplier"])
	require.Equal(t, true, filteredData["peak_rate_enabled"])
	require.Equal(t, "09:00", filteredData["peak_start"])
	require.Equal(t, "18:00", filteredData["peak_end"])
	require.Equal(t, 2.0, filteredData["peak_rate_multiplier"])
	require.Equal(t, "Asia/Shanghai", filteredData["timezone"])
	require.NotContains(t, filteredData, "effective_rate_multiplier")
	require.NotContains(t, filteredData, "remote_diagnostic")
	require.NotContains(t, metadata.Extra, "unused_large_field")
	require.Contains(t, string(fullPayload), lastError)
	require.NotContains(t, string(metaPayload), "last_error")
	require.Less(t, len(metaPayload)*4, len(fullPayload))
}

func TestSchedulerMetadataAccountDropsInvalidUpstreamBillingProbe(t *testing.T) {
	for _, probe := range []any{
		"invalid",
		map[string]any{},
		map[string]any{"status": ""},
	} {
		metadata := buildSchedulerMetadataAccount(service.Account{
			Extra: map[string]any{service.UpstreamBillingProbeExtraKey: probe},
		})

		require.NotContains(t, metadata.Extra, service.UpstreamBillingProbeExtraKey)
	}
}

// TestFilterSchedulerProjectionKeepsOpenCodeQuotaFallbackGateInputs 钉死额度保护的
// 「实际 meta 路径」：缓存命中时候选过滤直接读本投影（SchedulerSnapshotService.
// ListSchedulableAccounts 命中即返回投影账号，不回库 hydrate），因此
// credentials.quota_fallback 与 extra.opencode_go_usage_snapshot 必须在投影里，
// 否则启用保护的账号在选号阶段退化为无配置/unknown，未配替代的付费模型会被漏发。
// model_rate_limits（付费钱包 scope 与目标自身冷却键）同样必须保留。
func TestFilterSchedulerProjectionKeepsOpenCodeQuotaFallbackGateInputs(t *testing.T) {
	quotaFallback := map[string]any{
		"enabled":           true,
		"threshold_percent": 98,
		"model_mapping":     map[string]any{"deepseek-v4-flash": "step-5-preview"},
	}
	snapshot := map[string]any{
		"status": "ok",
		"data": map[string]any{
			"rolling": map[string]any{"status": "ok", "percent": 100.0},
		},
	}
	filteredCreds := filterSchedulerCredentials(map[string]any{
		"api_key": "k",
		service.OpenCodeQuotaFallbackCredentialKey: quotaFallback,
		"access_token": "secret-access-token",
	})
	filteredExtra := filterSchedulerExtra(map[string]any{
		service.OpenCodeGoUsageSnapshotExtraKey: snapshot,
		"model_rate_limits": map[string]any{
			service.OpenCodeQuotaFallbackWalletScope: map[string]any{
				"rate_limit_reset_at": "2026-10-17T00:00:00Z",
			},
		},
	})

	require.Equal(t, quotaFallback, filteredCreds[service.OpenCodeQuotaFallbackCredentialKey])
	require.NotContains(t, filteredCreds, "access_token")
	require.Equal(t, snapshot, filteredExtra[service.OpenCodeGoUsageSnapshotExtraKey])
	require.Contains(t, filteredExtra, "model_rate_limits")

	// 投影后的账号仍能读出启用态（判定入口读的就是这两个键）。
	account := service.Account{
		Platform:    service.PlatformOpenCode,
		Type:        service.AccountTypeAPIKey,
		Credentials: filteredCreds,
		Extra:       filteredExtra,
	}
	require.True(t, account.GetOpenCodeQuotaFallback().Enabled)
}
