package service

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	opencodepkg "github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/stretchr/testify/require"
)

// OpenCode 额度保护（quota_fallback）纯策略测试。
// 期望值来源：.scratch/opencode-free-quota/contract.md §1/§2/§4 与用户冻结裁决，
// 不从实现反推。本文件不触碰网络/DB。

func openCodeQuotaFallbackNow() time.Time {
	return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
}

func openCodeQuotaFallbackReset(t *testing.T, after time.Duration) time.Time {
	t.Helper()
	return openCodeQuotaFallbackNow().Add(after)
}

func openCodeQuotaFallbackUsageWindow(percent float64, resetsAt time.Time) OpenCodeGoUsageWindow {
	return OpenCodeGoUsageWindow{Status: OpenCodeGoUsageStatusOK, Percent: percent, ResetsAt: resetsAt}
}

func openCodeQuotaFallbackUsageData(rolling, weekly, monthly float64, resetsAt time.Time) *OpenCodeGoUsageData {
	return &OpenCodeGoUsageData{
		Rolling: openCodeQuotaFallbackUsageWindow(rolling, resetsAt),
		Weekly:  openCodeQuotaFallbackUsageWindow(weekly, resetsAt),
		Monthly: openCodeQuotaFallbackUsageWindow(monthly, resetsAt),
	}
}

func openCodeQuotaFallbackSnapshot(status string, data *OpenCodeGoUsageData, fetchedAt time.Time) *OpenCodeGoUsageSnapshot {
	snapshot := &OpenCodeGoUsageSnapshot{
		Status:        status,
		Data:          data,
		LastAttemptAt: fetchedAt,
	}
	if !fetchedAt.IsZero() {
		fetched := fetchedAt
		snapshot.FetchedAt = &fetched
	}
	return snapshot
}

func openCodeQuotaFallbackEnabledConfig(mapping map[string]string) OpenCodeQuotaFallbackConfig {
	return OpenCodeQuotaFallbackConfig{
		Enabled:          true,
		ThresholdPercent: 98,
		ModelMapping:     mapping,
	}
}

func TestParseOpenCodeQuotaFallback(t *testing.T) {
	tests := []struct {
		name    string
		raw     map[string]any
		want    OpenCodeQuotaFallbackConfig
		wantErr bool
	}{
		{
			name: "absent block is disabled with default threshold",
			raw:  nil,
			want: OpenCodeQuotaFallbackConfig{ThresholdPercent: 98},
		},
		{
			name: "empty object is disabled with default threshold",
			raw:  map[string]any{},
			want: OpenCodeQuotaFallbackConfig{ThresholdPercent: 98},
		},
		{
			name: "explicitly disabled keeps default threshold",
			raw:  map[string]any{"enabled": false},
			want: OpenCodeQuotaFallbackConfig{ThresholdPercent: 98},
		},
		{
			name: "disabled block tolerates malformed mapping content",
			raw: map[string]any{
				"enabled":       false,
				"model_mapping": map[string]any{"deepseek-v4-flash": ""},
			},
			want: OpenCodeQuotaFallbackConfig{ThresholdPercent: 98, ModelMapping: map[string]string{"deepseek-v4-flash": ""}},
		},
		{
			name: "enabled with one entry keeps default threshold",
			raw: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
			},
			want: OpenCodeQuotaFallbackConfig{
				Enabled:          true,
				ThresholdPercent: 98,
				ModelMapping:     map[string]string{"deepseek-v4-flash": "step-5-preview"},
			},
		},
		{
			name: "explicit integer threshold wins",
			raw: map[string]any{
				"enabled":           true,
				"threshold_percent": 90,
				"model_mapping":     map[string]any{"deepseek-v4-pro": "glm-5.3"},
			},
			want: OpenCodeQuotaFallbackConfig{
				Enabled:          true,
				ThresholdPercent: 90,
				ModelMapping:     map[string]string{"deepseek-v4-pro": "glm-5.3"},
			},
		},
		{
			name: "json float threshold is accepted when integral",
			raw: map[string]any{
				"enabled":           true,
				"threshold_percent": float64(100),
				"model_mapping":     map[string]any{"deepseek-v4-pro": "glm-5.3"},
			},
			want: OpenCodeQuotaFallbackConfig{
				Enabled:          true,
				ThresholdPercent: 100,
				ModelMapping:     map[string]string{"deepseek-v4-pro": "glm-5.3"},
			},
		},
		{
			name: "unknown subfields are ignored",
			raw: map[string]any{
				"enabled":           true,
				"threshold_percent": 98,
				"notes":             "manual free policy check",
				"model_mapping":     map[string]any{"deepseek-v4-pro": "glm-5.3"},
			},
			want: OpenCodeQuotaFallbackConfig{
				Enabled:          true,
				ThresholdPercent: 98,
				ModelMapping:     map[string]string{"deepseek-v4-pro": "glm-5.3"},
			},
		},
		{
			name:    "threshold below one is rejected",
			raw:     map[string]any{"enabled": false, "threshold_percent": 0},
			wantErr: true,
		},
		{
			name:    "threshold above one hundred is rejected",
			raw:     map[string]any{"enabled": false, "threshold_percent": 101},
			wantErr: true,
		},
		{
			name:    "fractional threshold is rejected",
			raw:     map[string]any{"enabled": true, "threshold_percent": 98.5, "model_mapping": map[string]any{"a": "b"}},
			wantErr: true,
		},
		{
			name:    "string threshold is rejected",
			raw:     map[string]any{"enabled": true, "threshold_percent": "98", "model_mapping": map[string]any{"a": "b"}},
			wantErr: true,
		},
		{
			name:    "non boolean enabled is rejected",
			raw:     map[string]any{"enabled": "true"},
			wantErr: true,
		},
		{
			name:    "non object model mapping is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": []any{"a"}},
			wantErr: true,
		},
		{
			name:    "non string mapping value is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{"a": 1}},
			wantErr: true,
		},
		{
			name:    "enabled without any mapping entry is rejected",
			raw:     map[string]any{"enabled": true},
			wantErr: true,
		},
		{
			name:    "enabled with empty mapping object is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{}},
			wantErr: true,
		},
		{
			name:    "empty source is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{"": "glm-5.3"}},
			wantErr: true,
		},
		{
			name:    "empty target is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{"deepseek-v4-pro": "   "}},
			wantErr: true,
		},
		{
			name:    "target equal to source is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{"deepseek-v4-pro": "deepseek-v4-pro"}},
			wantErr: true,
		},
		{
			name: "chained mapping target is rejected",
			raw: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-pro": "glm-5.3", "glm-5.3": "qwen3.8-max"},
			},
			wantErr: true,
		},
		{
			name:    "wildcard source is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{"deepseek-*": "glm-5.3"}},
			wantErr: true,
		},
		{
			name:    "wildcard target is rejected",
			raw:     map[string]any{"enabled": true, "model_mapping": map[string]any{"deepseek-v4-pro": "glm-*"}},
			wantErr: true,
		},
		{
			// Parse 无账号上下文，不做平台静态名单否决（gemini/jev 的通用端点
			// 限制由保存侧 ValidateOpenCodeQuotaFallbackCredentials 结合
			// base_url / protocol_rules 判定）。
			name: "unsupported targets are structurally accepted at parse",
			raw: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-pro": "gemini-2.5-pro", "deepseek-v4-flash": "jev-1"},
			},
			want: OpenCodeQuotaFallbackConfig{
				Enabled:          true,
				ThresholdPercent: 98,
				ModelMapping:     map[string]string{"deepseek-v4-pro": "gemini-2.5-pro", "deepseek-v4-flash": "jev-1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOpenCodeQuotaFallback(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEvaluateOpenCodeQuotaFallback(t *testing.T) {
	now := openCodeQuotaFallbackNow()
	resetAt := openCodeQuotaFallbackReset(t, 90*time.Minute)
	mapping := map[string]string{"deepseek-v4-flash": "step-5-preview"}

	tests := []struct {
		name             string
		cfg              OpenCodeQuotaFallbackConfig
		snap             *OpenCodeGoUsageSnapshot
		lastOk           *OpenCodeGoUsageData
		wantState        string
		wantReason       string
		wantWindows      []OpenCodeQuotaFallbackWindow
		wantSnapshotAt   bool
		wantEffectiveMap bool
	}{
		{
			name:       "disabled config keeps old behavior",
			cfg:        OpenCodeQuotaFallbackConfig{ThresholdPercent: 98},
			snap:       openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(100, 100, 100, resetAt), now),
			wantState:  OpenCodeQuotaFallbackStateDisabled,
			wantReason: "",
		},
		{
			name:           "fresh ok snapshot below threshold stays normal",
			cfg:            openCodeQuotaFallbackEnabledConfig(mapping),
			snap:           openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(97.9, 12, 5, resetAt), now.Add(-10*time.Minute)),
			wantState:      OpenCodeQuotaFallbackStateNormal,
			wantSnapshotAt: true,
		},
		{
			name:             "threshold equal to 98 triggers fallback",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(98, 12, 5, resetAt), now),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonThresholdReached,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 98, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:             "fully used monthly window triggers fallback",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(10, 20, 100, resetAt), now),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonThresholdReached,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "monthly", UsedPercent: 100, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:       "multiple windows report every triggering window in order",
			cfg:        openCodeQuotaFallbackEnabledConfig(mapping),
			snap:       openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(98, 99, 100, resetAt), now),
			wantState:  OpenCodeQuotaFallbackStateFallbackActive,
			wantReason: OpenCodeQuotaFallbackReasonThresholdReached,
			wantWindows: []OpenCodeQuotaFallbackWindow{
				{Window: "5h", UsedPercent: 98, ResetsAt: &resetAt},
				{Window: "weekly", UsedPercent: 99, ResetsAt: &resetAt},
				{Window: "monthly", UsedPercent: 100, ResetsAt: &resetAt},
			},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name: "explicit threshold is honored",
			cfg: OpenCodeQuotaFallbackConfig{
				Enabled:          true,
				ThresholdPercent: 97,
				ModelMapping:     mapping,
			},
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(97, 10, 10, resetAt), now),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonThresholdReached,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 97, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:           "zero threshold falls back to default 98",
			cfg:            openCodeQuotaFallbackEnabledConfig(mapping),
			snap:           openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(97.9, 10, 10, resetAt), now),
			wantState:      OpenCodeQuotaFallbackStateNormal,
			wantSnapshotAt: true,
		},
		{
			name:           "stale ok snapshot below threshold is unknown",
			cfg:            openCodeQuotaFallbackEnabledConfig(mapping),
			snap:           openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(30, 10, 5, resetAt), now.Add(-3*time.Hour)),
			wantState:      OpenCodeQuotaFallbackStateUnknown,
			wantReason:     OpenCodeQuotaFallbackReasonSnapshotStale,
			wantSnapshotAt: true,
		},
		{
			name:             "stale ok snapshot with triggering evidence keeps fallback active",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(99, 10, 5, resetAt), now.Add(-3*time.Hour)),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonLastKnownTriggered,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 99, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:             "failed snapshot keeps last known triggering evidence",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusFailed, openCodeQuotaFallbackUsageData(100, 10, 5, resetAt), now),
			lastOk:           openCodeQuotaFallbackUsageData(100, 10, 5, resetAt),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonLastKnownTriggered,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 100, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:           "failed snapshot with below threshold evidence is unknown",
			cfg:            openCodeQuotaFallbackEnabledConfig(mapping),
			snap:           openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusFailed, openCodeQuotaFallbackUsageData(10, 10, 5, resetAt), now),
			lastOk:         openCodeQuotaFallbackUsageData(10, 10, 5, resetAt),
			wantState:      OpenCodeQuotaFallbackStateUnknown,
			wantReason:     OpenCodeQuotaFallbackReasonRefreshFailed,
			wantSnapshotAt: true,
		},
		{
			name:           "unauthorized snapshot is unknown",
			cfg:            openCodeQuotaFallbackEnabledConfig(mapping),
			snap:           openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusUnauthorized, nil, now),
			wantState:      OpenCodeQuotaFallbackStateUnknown,
			wantReason:     OpenCodeQuotaFallbackReasonUnauthorized,
			wantSnapshotAt: true,
		},
		{
			name:       "missing snapshot without evidence is unknown",
			cfg:        openCodeQuotaFallbackEnabledConfig(mapping),
			wantState:  OpenCodeQuotaFallbackStateUnknown,
			wantReason: OpenCodeQuotaFallbackReasonNoSnapshot,
		},
		{
			name:             "missing snapshot with triggering evidence keeps fallback active",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			lastOk:           openCodeQuotaFallbackUsageData(99, 10, 5, resetAt),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonLastKnownTriggered,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 99, ResetsAt: &resetAt}},
			wantEffectiveMap: true,
		},
		{
			name:             "passed reset time on triggering evidence does not recover",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(98, 10, 5, now.Add(-30*time.Minute)), now.Add(-3*time.Hour)),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonLastKnownTriggered,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 98, ResetsAt: pointerToTime(now.Add(-30 * time.Minute))}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:             "one recovered window does not recover while another still triggers",
			cfg:              openCodeQuotaFallbackEnabledConfig(mapping),
			snap:             openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(10, 98.5, 20, resetAt), now),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonThresholdReached,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "weekly", UsedPercent: 98.5, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
		{
			name:           "new ok snapshot all below recovers from last known trigger",
			cfg:            openCodeQuotaFallbackEnabledConfig(mapping),
			snap:           openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(5, 5, 5, resetAt), now),
			lastOk:         openCodeQuotaFallbackUsageData(99, 10, 5, resetAt),
			wantState:      OpenCodeQuotaFallbackStateNormal,
			wantSnapshotAt: true,
		},
		{
			name: "fresh snapshot with a missing window status is unknown",
			cfg:  openCodeQuotaFallbackEnabledConfig(mapping),
			snap: openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, &OpenCodeGoUsageData{
				Rolling: OpenCodeGoUsageWindow{Status: "", Percent: 5, ResetsAt: resetAt},
				Weekly:  openCodeQuotaFallbackUsageWindow(5, resetAt),
				Monthly: openCodeQuotaFallbackUsageWindow(5, resetAt),
			}, now),
			wantState:      OpenCodeQuotaFallbackStateUnknown,
			wantReason:     OpenCodeQuotaFallbackReasonSnapshotInvalid,
			wantSnapshotAt: true,
		},
		{
			name: "fresh snapshot with a NaN percent is never treated as below threshold",
			cfg:  openCodeQuotaFallbackEnabledConfig(mapping),
			snap: openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, &OpenCodeGoUsageData{
				Rolling: openCodeQuotaFallbackUsageWindow(math.NaN(), resetAt),
				Weekly:  openCodeQuotaFallbackUsageWindow(5, resetAt),
				Monthly: openCodeQuotaFallbackUsageWindow(5, resetAt),
			}, now),
			wantState:      OpenCodeQuotaFallbackStateUnknown,
			wantReason:     OpenCodeQuotaFallbackReasonSnapshotInvalid,
			wantSnapshotAt: true,
		},
		{
			name: "rate limited window counts as fully used even when percent lags",
			cfg:  openCodeQuotaFallbackEnabledConfig(mapping),
			snap: openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusOK, &OpenCodeGoUsageData{
				Rolling: OpenCodeGoUsageWindow{Status: string(opencodepkg.UsageStatusRateLimited), Percent: 40, ResetsAt: resetAt},
				Weekly:  openCodeQuotaFallbackUsageWindow(5, resetAt),
				Monthly: openCodeQuotaFallbackUsageWindow(5, resetAt),
			}, now),
			wantState:        OpenCodeQuotaFallbackStateFallbackActive,
			wantReason:       OpenCodeQuotaFallbackReasonThresholdReached,
			wantWindows:      []OpenCodeQuotaFallbackWindow{{Window: "5h", UsedPercent: 100, ResetsAt: &resetAt}},
			wantSnapshotAt:   true,
			wantEffectiveMap: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateOpenCodeQuotaFallback(tt.cfg, tt.snap, tt.lastOk, now)
			require.Equal(t, tt.wantState, got.State)
			require.Equal(t, tt.wantReason, got.Reason)
			if tt.wantWindows == nil {
				require.Empty(t, got.TriggeringWindows)
			} else {
				require.Equal(t, tt.wantWindows, got.TriggeringWindows)
			}
			if tt.wantSnapshotAt {
				require.NotNil(t, got.SnapshotAt)
			}
			if tt.wantEffectiveMap {
				require.Equal(t, tt.cfg.ModelMapping, got.EffectiveMapping)
			} else {
				require.Empty(t, got.EffectiveMapping)
			}
		})
	}
}

func pointerToTime(value time.Time) *time.Time {
	return &value
}

// TestOpenCodeQuotaFallbackRoutingModelForLegacyPlatform 覆盖 P1 修订：legacy
// opencode 没有 provider profile（routesByModel=false），该分支必须仅在额度保护
// 命中替代时返回实际目标，否则返回 ""（feature-off/未命中与原行为逐字一致）。
func TestOpenCodeQuotaFallbackRoutingModelForLegacyPlatform(t *testing.T) {
	now := openCodeQuotaFallbackNow()
	resetAt := openCodeQuotaFallbackReset(t, time.Hour)
	configuration := map[string]any{
		"enabled":       true,
		"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
	}
	newAccount := func(extra map[string]any) *Account {
		return &Account{
			Platform:    PlatformOpenCode,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: configuration, "api_key": "sk-test"},
			Extra:       extra,
		}
	}
	triggering := map[string]any{
		OpenCodeGoUsageSnapshotExtraKey: openCodeQuotaFallbackSnapshot(
			OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(100, 10, 5, resetAt), now),
	}

	require.Equal(t, "step-5-preview",
		openCodeQuotaFallbackRoutingModelForBody(newAccount(triggering), []byte(`{"model":"deepseek-v4-flash"}`), ""))
	require.Equal(t, "",
		openCodeQuotaFallbackRoutingModelForBody(newAccount(triggering), []byte(`{"model":"deepseek-v4-pro"}`), ""))
	require.Equal(t, "",
		openCodeQuotaFallbackRoutingModelForBody(newAccount(nil), []byte(`{"model":"deepseek-v4-flash"}`), ""))

	disabled := &Account{
		Platform:    PlatformOpenCode,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: map[string]any{"enabled": false}, "api_key": "sk-test"},
		Extra:       triggering,
	}
	require.Equal(t, "",
		openCodeQuotaFallbackRoutingModelForBody(disabled, []byte(`{"model":"deepseek-v4-flash"}`), ""))
}

// TestOpenCodeQuotaFallbackAllowsCapability 覆盖 legacy inbound gate 的条件接通：
// 只有启用保护且替代目标的协议集合覆盖该端点能力时才放行；feature-off、无规则声明、
// 或目标协议不符一律保持 CC-only 原行为。
func TestOpenCodeQuotaFallbackAllowsCapability(t *testing.T) {
	configuration := map[string]any{
		"enabled":       true,
		"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
	}
	accountWithRules := func(rules any) *Account {
		credentials := map[string]any{OpenCodeQuotaFallbackCredentialKey: configuration, "api_key": "sk-test"}
		if rules != nil {
			credentials["protocol_rules"] = rules
		}
		return &Account{Platform: PlatformOpenCode, Type: AccountTypeAPIKey, Credentials: credentials}
	}

	require.True(t, openCodeQuotaFallbackAllowsCapability(accountWithRules([]any{
		map[string]any{"pattern": "step-5-preview", "protocol": "responses"},
	}), OpenAIEndpointCapabilityResponses))
	require.False(t, openCodeQuotaFallbackAllowsCapability(accountWithRules(nil), OpenAIEndpointCapabilityResponses))
	require.False(t, openCodeQuotaFallbackAllowsCapability(accountWithRules([]any{
		map[string]any{"pattern": "step-5-preview", "protocol": "anthropic"},
	}), OpenAIEndpointCapabilityResponses))
	require.False(t, openCodeQuotaFallbackAllowsCapability(&Account{
		Platform:    PlatformOpenCode,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: map[string]any{"enabled": false}, "api_key": "sk-test"},
	}, OpenAIEndpointCapabilityResponses))
}

func TestOpenCodeQuotaFallbackDecisionResolveRequest(t *testing.T) {
	mapping := map[string]string{"deepseek-v4-flash": "step-5-preview"}

	tests := []struct {
		name            string
		decision        OpenCodeQuotaFallbackDecision
		normalUpstream  string
		wantModel       string
		wantAllowed     bool
		wantMappingUsed bool
	}{
		{
			name:           "disabled passes the normal model through",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateDisabled},
			normalUpstream: "deepseek-v4-flash",
			wantModel:      "deepseek-v4-flash",
			wantAllowed:    true,
		},
		{
			name:           "normal passes the normal model through",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateNormal},
			normalUpstream: "deepseek-v4-flash",
			wantModel:      "deepseek-v4-flash",
			wantAllowed:    true,
		},
		{
			name:            "fallback active rewrites a configured source to its target",
			decision:        OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateFallbackActive, EffectiveMapping: mapping},
			normalUpstream:  "deepseek-v4-flash",
			wantModel:       "step-5-preview",
			wantAllowed:     true,
			wantMappingUsed: true,
		},
		{
			name:           "fallback active blocks a paid model without a configured target",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateFallbackActive, EffectiveMapping: mapping},
			normalUpstream: "deepseek-v4-pro",
			wantModel:      "deepseek-v4-pro",
			wantAllowed:    false,
		},
		{
			name:           "fallback active allows a direct call to the configured target",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateFallbackActive, EffectiveMapping: mapping},
			normalUpstream: "step-5-preview",
			wantModel:      "step-5-preview",
			wantAllowed:    true,
		},
		{
			// 「配置的 target」必须由 decision 携带的映射表界定：unknown 只放行
			// 管理员手选目标（EffectiveMapping 的 value），不放行付费 source。
			name:           "unknown allows a direct call to the configured target",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateUnknown, EffectiveMapping: mapping},
			normalUpstream: "step-5-preview",
			wantModel:      "step-5-preview",
			wantAllowed:    true,
		},
		{
			name:            "unknown never auto selects the target for a mapped source",
			decision:        OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateUnknown, EffectiveMapping: mapping},
			normalUpstream:  "deepseek-v4-flash",
			wantModel:       "deepseek-v4-flash",
			wantAllowed:     false,
			wantMappingUsed: false,
		},
		{
			name:           "unknown blocks other paid models",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateUnknown},
			normalUpstream: "deepseek-v4-pro",
			wantModel:      "deepseek-v4-pro",
			wantAllowed:    false,
		},
		{
			name:           "fallback active does not rewrite the target twice",
			decision:       OpenCodeQuotaFallbackDecision{State: OpenCodeQuotaFallbackStateFallbackActive, EffectiveMapping: map[string]string{"a": "b"}},
			normalUpstream: "b",
			wantModel:      "b",
			wantAllowed:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.decision.ResolveRequest(tt.normalUpstream)
			require.Equal(t, tt.wantModel, got.EffectiveUpstreamModel)
			require.Equal(t, tt.wantAllowed, got.DispatchAllowed)
			require.Equal(t, tt.wantMappingUsed, got.MappingApplied)
		})
	}
}

func TestOpenCodeQuotaFallbackLastKnownOKData(t *testing.T) {
	now := openCodeQuotaFallbackNow()
	resetAt := openCodeQuotaFallbackReset(t, time.Hour)
	data := openCodeQuotaFallbackUsageData(99, 10, 5, resetAt)

	require.Nil(t, OpenCodeQuotaFallbackLastKnownOKData(nil))
	require.Nil(t, OpenCodeQuotaFallbackLastKnownOKData(&OpenCodeGoUsageSnapshot{Status: OpenCodeGoUsageStatusOK, Data: data}))
	require.Same(t, data, OpenCodeQuotaFallbackLastKnownOKData(
		openCodeQuotaFallbackSnapshot(OpenCodeGoUsageStatusFailed, data, now),
	))
}

func TestAccountGetOpenCodeQuotaFallback(t *testing.T) {
	enabledCredentials := func(extra map[string]any) map[string]any {
		credentials := map[string]any{
			OpenCodeQuotaFallbackCredentialKey: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
			},
		}
		for key, value := range extra {
			credentials[key] = value
		}
		return credentials
	}

	tests := []struct {
		name        string
		account     *Account
		wantEnabled bool
	}{
		{
			name:        "opencode_go api key go mode enables the config",
			account:     &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Credentials: enabledCredentials(nil)},
			wantEnabled: true,
		},
		{
			name: "legacy opencode api key go mode enables the config",
			account: &Account{Platform: PlatformOpenCode, Type: AccountTypeAPIKey,
				Credentials: enabledCredentials(map[string]any{"account_mode": AccountModeGo})},
			wantEnabled: true,
		},
		{
			name: "zen mode keeps the feature off",
			account: &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey,
				Credentials: enabledCredentials(map[string]any{"account_mode": AccountModeZen})},
			wantEnabled: false,
		},
		{
			name: "non opencode platform keeps the feature off",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: enabledCredentials(map[string]any{"account_mode": AccountModeGo})},
			wantEnabled: false,
		},
		{
			name: "non api key account keeps the feature off",
			account: &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeOAuth,
				Credentials: enabledCredentials(nil)},
			wantEnabled: false,
		},
		{
			name:        "dirty config disables the feature instead of panicking",
			account:     &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: "not-an-object"}},
			wantEnabled: false,
		},
		{
			name:        "missing config disables the feature",
			account:     &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"}},
			wantEnabled: false,
		},
		{
			name:        "nil account keeps the feature off",
			account:     nil,
			wantEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.account.GetOpenCodeQuotaFallback()
			require.Equal(t, tt.wantEnabled, got.Enabled)
		})
	}
}

func TestValidateOpenCodeQuotaFallbackCredentials(t *testing.T) {
	enabledBlock := map[string]any{
		"enabled":       true,
		"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
	}
	disabledBlock := map[string]any{"enabled": false}

	tests := []struct {
		name        string
		platform    string
		accountType string
		credentials map[string]any
		wantReason  string
	}{
		{
			name:        "missing block is a no-op",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{"api_key": "sk-test"},
		},
		{
			name:        "nil credentials are a no-op",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: nil,
		},
		{
			name:        "enabled block on opencode_go go mode is accepted",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: enabledBlock},
		},
		{
			name:        "enabled block on legacy opencode go mode is accepted",
			platform:    PlatformOpenCode,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: enabledBlock, "account_mode": AccountModeGo},
		},
		{
			name:        "disabled block is tolerated on other platforms",
			platform:    PlatformOpenAI,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: disabledBlock},
		},
		{
			name:        "enabled block on another platform is rejected",
			platform:    PlatformOpenAI,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: enabledBlock},
			wantReason:  "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			name:        "enabled block in zen mode is rejected",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: enabledBlock, "account_mode": AccountModeZen},
			wantReason:  "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			name:        "enabled block on a non api key account is rejected",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeOAuth,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: enabledBlock},
			wantReason:  "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			name:        "enabled block with an empty mapping is rejected",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: map[string]any{"enabled": true, "model_mapping": map[string]any{}}},
			wantReason:  "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			name:        "threshold out of range is rejected",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: map[string]any{"enabled": true, "threshold_percent": 0}},
			wantReason:  "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			name:        "non object block is rejected",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: "enabled"},
			wantReason:  "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			// 官方端点（无自定义 base_url / protocol_rules）上 gemini 目标不可用。
			name:        "unsupported target on the official endpoint is rejected",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-pro": "gemini-2.5-pro"},
			}},
			wantReason: "INVALID_OPENCODE_QUOTA_FALLBACK",
		},
		{
			// 自定义 relay 不能被平台静态名单误拒。
			name:        "unsupported target behind a custom relay is accepted",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{
				OpenCodeQuotaFallbackCredentialKey: map[string]any{
					"enabled":       true,
					"model_mapping": map[string]any{"deepseek-v4-pro": "gemini-2.5-pro"},
				},
				"base_url": "https://relay.example.com/v1",
			},
		},
		{
			// 账号 protocol_rules 显式声明该模型协议时同样放行。
			name:        "unsupported target with explicit protocol rules is accepted",
			platform:    PlatformOpenCodeGo,
			accountType: AccountTypeAPIKey,
			credentials: map[string]any{
				OpenCodeQuotaFallbackCredentialKey: map[string]any{
					"enabled":       true,
					"model_mapping": map[string]any{"deepseek-v4-pro": "gemini-2.5-pro"},
				},
				"protocol_rules": []any{map[string]any{"pattern": "gemini-*", "protocol": "chat_completions"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOpenCodeQuotaFallbackCredentials(tt.platform, tt.accountType, tt.credentials)
			if tt.wantReason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
		})
	}
}

// openCodeQuotaFallbackPolicyAccount 构造一个「启用额度保护」的 OpenCode Go 账号，
// 复用 opencode_go_usage_test.go 的组身份约定（api_key + 官方 base_url）。
func openCodeQuotaFallbackPolicyAccount(id int64, apiKey string) *Account {
	return &Account{
		ID:       id,
		Name:     fmt.Sprintf("opencode-policy-%d", id),
		Platform: PlatformOpenCodeGo,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go/v1",
			"api_key":  apiKey,
			OpenCodeQuotaFallbackCredentialKey: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
			},
		},
		Extra:       map[string]any{},
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
}

// TestOpenCodeGoUsageRunDueRefreshesPolicyAccountsWhenGlobalDisabled 覆盖 gap 3：
// 全局用量开关关闭（默认）时，启用额度保护的账号仍必须刷新——否则首启 unknown
// 永远不会离开；未启用策略且未开自动刷新的账号仍被跳过（设置原样）。
func TestOpenCodeGoUsageRunDueRefreshesPolicyAccountsWhenGlobalDisabled(t *testing.T) {
	policyAccount := openCodeQuotaFallbackPolicyAccount(11, "key-11")
	plainAccount := openCodeGoUsageAccount(12) // 无 auto_refresh、无策略
	repo := &openCodeGoUsageTestRepo{accounts: map[int64]*Account{11: policyAccount, 12: plainAccount}}
	stub := &openCodeGoUsageHTTPStub{body: []byte(openCodeGoUsageFixture)}
	svc := newOpenCodeGoUsageTestService(t, repo, stub, &upstreamBillingProbeSettingRepo{})

	require.NoError(t, svc.RunDue(context.Background()))
	require.Equal(t, int64(1), stub.calls.Load())
	require.Equal(t, OpenCodeGoUsageStatusOK, decodeOpenCodeGoUsageSnapshot(policyAccount.Extra).Status)
	require.Nil(t, decodeOpenCodeGoUsageSnapshot(plainAccount.Extra))
}

// TestOpenCodeGoUsageRunDueSharesPolicySnapshotWithGroupSibling 覆盖 gap 4：
// 同一 api_key/base_url 身份的 siblings 共享同一份快照（既有组作用域写入方），
// 任一 sibling 不会因没有自己的快照而永久处于 unknown。
func TestOpenCodeGoUsageRunDueSharesPolicySnapshotWithGroupSibling(t *testing.T) {
	policyAccount := openCodeQuotaFallbackPolicyAccount(21, "key-shared")
	sibling := &Account{
		ID:       22,
		Name:     "opencode-sibling-22",
		Platform: PlatformOpenCodeGo,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go/v1",
			"api_key":  "key-shared",
		},
		Extra:       map[string]any{},
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	repo := &openCodeGoUsageTestRepo{accounts: map[int64]*Account{21: policyAccount, 22: sibling}}
	stub := &openCodeGoUsageHTTPStub{body: []byte(openCodeGoUsageFixture)}
	svc := newOpenCodeGoUsageTestService(t, repo, stub, &upstreamBillingProbeSettingRepo{})

	require.NoError(t, svc.RunDue(context.Background()))
	require.Equal(t, int64(1), stub.calls.Load())
	require.Equal(t, OpenCodeGoUsageStatusOK, decodeOpenCodeGoUsageSnapshot(policyAccount.Extra).Status)
	require.Equal(t, OpenCodeGoUsageStatusOK, decodeOpenCodeGoUsageSnapshot(sibling.Extra).Status)
}

// openCodeQuotaFallbackWriteRecorder 只为满足接口；本用例只驱动两个写入 helper。
// 同时嵌入两个接口：AccountRepository 供付费钱包写入，openCodeGoUsageRepository 供
// 组作用域快照写入（helper 内部对该接口做类型断言，缺一即退化为 no-op）。
type openCodeQuotaFallbackWriteRecorder struct {
	AccountRepository
	openCodeGoUsageRepository
	snapshot        *OpenCodeGoUsageSnapshot
	modelScope      string
	modelResetAt    time.Time
	modelReason     string
	setModelCalled  int
	updateSnapCalls int
}

func (r *openCodeQuotaFallbackWriteRecorder) UpdateOpenCodeGoUsageSnapshot(_ context.Context, _ *Account, snapshot *OpenCodeGoUsageSnapshot) error {
	r.updateSnapCalls++
	r.snapshot = snapshot
	return nil
}

func (r *openCodeQuotaFallbackWriteRecorder) SetModelRateLimit(_ context.Context, _ int64, scope string, resetAt time.Time, reason ...string) error {
	r.setModelCalled++
	r.modelScope = scope
	r.modelResetAt = resetAt
	if len(reason) > 0 {
		r.modelReason = reason[0]
	}
	return nil
}

// TestOpenCodeQuotaFallbackProbeSnapshotAndWalletWriter 覆盖 gap 2：429 主动探测结果
// 必须并入同一份 opencode_go_usage_snapshot 形态（组作用域写入），并另行写模型作用域
// 付费钱包冷却；探测为空/写库不可用时不得伪造快照。
func TestOpenCodeQuotaFallbackProbeSnapshotAndWalletWriter(t *testing.T) {
	account := openCodeQuotaFallbackPolicyAccount(31, "key-31")
	resetAt := time.Now().Add(2 * time.Hour).UTC()
	rateLimitedReset := time.Now().Add(time.Hour).UTC()
	probe := &opencodepkg.UsageSnapshot{
		Rolling: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusRateLimited, Percent: 40, ResetsAt: &rateLimitedReset},
		Weekly:  opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusOK, Percent: 12},
		Monthly: opencodepkg.UsageWindow{Status: opencodepkg.UsageStatusOK, Percent: 3},
	}
	repo := &openCodeQuotaFallbackWriteRecorder{}

	require.True(t, persistOpenCodeQuotaFallbackProbeSnapshot(context.Background(), repo, account, probe))
	require.Equal(t, 1, repo.updateSnapCalls)
	require.NotNil(t, repo.snapshot)
	require.Equal(t, OpenCodeGoUsageStatusOK, repo.snapshot.Status)
	require.NotNil(t, repo.snapshot.Data)
	// rate-limited 窗口按既有约定归一为已用满，其余窗口保留原始百分比。
	require.InDelta(t, 100, repo.snapshot.Data.Rolling.Percent, 0.001)
	require.InDelta(t, 12, repo.snapshot.Data.Weekly.Percent, 0.001)
	require.NotNil(t, repo.snapshot.FetchedAt)
	require.True(t, repo.snapshot.NextRefreshAt.After(*repo.snapshot.FetchedAt))

	require.True(t, persistOpenCodeQuotaFallbackWalletLimit(context.Background(), repo, account, resetAt, "opencode_usage"))
	require.Equal(t, 1, repo.setModelCalled)
	require.Equal(t, OpenCodeQuotaFallbackWalletScope, repo.modelScope)
	require.Equal(t, resetAt.UTC(), repo.modelResetAt.UTC())
	require.Contains(t, repo.modelReason, OpenCodeQuotaFallbackWalletReasonPrefix)

	require.False(t, persistOpenCodeQuotaFallbackProbeSnapshot(context.Background(), repo, account, nil))
	require.False(t, persistOpenCodeQuotaFallbackWalletLimit(context.Background(), repo, account, time.Now().Add(-time.Minute), "opencode_usage"))
}

// TestOpenCodeQuotaFallbackBlocksAndRoutedShareNormalResolution 覆盖 blocks/routed 同源：
// 别名经账号 model_mapping 解析一次得到正常上游模型后命中替代；目标直连放行且不再二次映射。
func TestOpenCodeQuotaFallbackBlocksAndRoutedShareNormalResolution(t *testing.T) {
	now := openCodeQuotaFallbackNow()
	resetAt := openCodeQuotaFallbackReset(t, time.Hour)
	account := &Account{
		ID:       41,
		Platform: PlatformOpenCode,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
			OpenCodeQuotaFallbackCredentialKey: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
			},
			"model_mapping": map[string]any{"alias-flash": "deepseek-v4-flash"},
		},
		Extra: map[string]any{
			OpenCodeGoUsageSnapshotExtraKey: openCodeQuotaFallbackSnapshot(
				OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(100, 10, 5, resetAt), now),
		},
	}

	// 别名 → 正常上游（deepseek-v4-flash）→ 命中替代：不拒选、出站为目标。
	require.False(t, openCodeQuotaFallbackBlocksModel(account, "alias-flash"))
	target, ok := openCodeQuotaFallbackRoutedTarget(account, "alias-flash")
	require.True(t, ok)
	require.Equal(t, "step-5-preview", target)
	// 未配置替代的付费模型：拒选。
	require.True(t, openCodeQuotaFallbackBlocksModel(account, "deepseek-v4-pro"))
	if unmapped, ok := openCodeQuotaFallbackRoutedTarget(account, "deepseek-v4-pro"); ok {
		require.Equal(t, "", unmapped)
	}
	// 目标直连：放行且不做二次映射（目标不是任何 source）。
	require.False(t, openCodeQuotaFallbackBlocksModel(account, "step-5-preview"))
	if direct, ok := openCodeQuotaFallbackRoutedTarget(account, "step-5-preview"); ok {
		require.Equal(t, "step-5-preview", direct)
	}
}

// openCodeQuotaFallbackWalletExtra 构造付费钱包冷却的 extra 形态（与 repo 写入一致）。
func openCodeQuotaFallbackWalletExtra(until time.Time) map[string]any {
	return map[string]any{
		modelRateLimitsKey: map[string]any{
			OpenCodeQuotaFallbackWalletScope: map[string]any{
				"rate_limited_at":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				"rate_limit_reset_at": until.UTC().Format(time.RFC3339),
				"reason":              OpenCodeQuotaFallbackWalletReasonPrefix + ": opencode_usage",
			},
		},
	}
}

// TestOpenCodeQuotaFallbackWalletKeyDoesNotBlockTargetOrRecoveredPaid 覆盖 review 收尾：
// paid-wallet key 只在额度保护判定该模型不可用时加入——替代目标、目标直连、
// 以及已从可信快照恢复 normal 的请求都不得被误封（目标自身冷却键仍保留）。
func TestOpenCodeQuotaFallbackWalletKeyDoesNotBlockTargetOrRecoveredPaid(t *testing.T) {
	// 该用例驱动的是生产 helper（内部使用真实时钟），夹具时间必须相对 time.Now()。
	now := time.Now()
	resetAt := now.Add(time.Hour)
	walletUntil := time.Now().Add(4 * time.Hour)
	configuration := map[string]any{
		"enabled":       true,
		"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
	}
	accountWith := func(snapshotPercent float64, fetchedAt time.Time) *Account {
		extra := openCodeQuotaFallbackWalletExtra(walletUntil)
		extra[OpenCodeGoUsageSnapshotExtraKey] = openCodeQuotaFallbackSnapshot(
			OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(snapshotPercent, 10, 5, resetAt), fetchedAt)
		return &Account{
			ID:          51,
			Platform:    PlatformOpenCode,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{OpenCodeQuotaFallbackCredentialKey: configuration, "api_key": "sk-test"},
			Extra:       extra,
		}
	}
	keysFor := func(account *Account, requestedModel string) []string {
		return account.modelRateLimitKeysForRequest(context.Background(), requestedModel)
	}

	// 触发态：替代目标直连不被钱包键封（无替代命中、也不拒选）。
	// 夹具时间来自函数开头的 time.Now()：生产 helper 使用真实时钟，「新鲜」快照
	// 必须相对真实时钟构造（固定过去时间会越界 30 分钟 stale 界，按契约降级 unknown
	// 并停付费源）。
	triggered := accountWith(100, now)
	require.False(t, openCodeQuotaFallbackBlocksModel(triggered, "step-5-preview"))
	require.NotContains(t, keysFor(triggered, "step-5-preview"), OpenCodeQuotaFallbackWalletScope)
	// 触发态：未配置替代的付费模型被拒选，并因钱包冷却获得额外门禁键。
	require.True(t, openCodeQuotaFallbackBlocksModel(triggered, "deepseek-v4-pro"))
	require.Contains(t, keysFor(triggered, "deepseek-v4-pro"), OpenCodeQuotaFallbackWalletScope)
	// 触发态：命中替代的源模型出站为目标，不套钱包键。
	require.NotContains(t, keysFor(triggered, "deepseek-v4-flash"), OpenCodeQuotaFallbackWalletScope)
	require.Contains(t, keysFor(triggered, "deepseek-v4-flash"), "step-5-preview")

	// fresh 快照全窗低于阈值（从旧触发恢复 normal）：钱包残留冷却不得误封付费模型。
	recovered := accountWith(5, now)
	require.False(t, openCodeQuotaFallbackBlocksModel(recovered, "deepseek-v4-pro"))
	require.NotContains(t, keysFor(recovered, "deepseek-v4-pro"), OpenCodeQuotaFallbackWalletScope)
}

// TestOpenCodeQuotaFallbackRoutingModelSharesBodyChain 覆盖 review 收尾：协议分流的
// routing model 必须来自与 blocks/routed 相同的 normal 解析链（body 版本的单一来源），
// 不在本函数里自算一遍 map+normalize。
func TestOpenCodeQuotaFallbackRoutingModelSharesBodyChain(t *testing.T) {
	now := openCodeQuotaFallbackNow()
	resetAt := openCodeQuotaFallbackReset(t, time.Hour)
	body := []byte(`{"model":"alias-flash"}`)
	account := &Account{
		ID:       61,
		Platform: PlatformOpenCode,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
			OpenCodeQuotaFallbackCredentialKey: map[string]any{
				"enabled":       true,
				"model_mapping": map[string]any{"deepseek-v4-flash": "step-5-preview"},
			},
			"model_mapping": map[string]any{"alias-flash": "deepseek-v4-flash"},
		},
		Extra: map[string]any{
			OpenCodeGoUsageSnapshotExtraKey: openCodeQuotaFallbackSnapshot(
				OpenCodeGoUsageStatusOK, openCodeQuotaFallbackUsageData(100, 10, 5, resetAt), now),
		},
	}

	// 共享 body 链（含替代）与 legacy 分流入口给出同一 effective 目标。
	shared := resolveMappedUpstreamModel(account, body, "")
	require.Equal(t, "step-5-preview", shared)
	require.Equal(t, shared, openCodeQuotaFallbackRoutingModelForBody(account, body, ""))

	// feature-off：分流入口保持基线 ""（不影响既有 unsupported-model 判定），
	// 共享链仍是普通映射结果。
	disabled := &Account{
		ID:          62,
		Platform:    PlatformOpenCode,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", OpenCodeQuotaFallbackCredentialKey: map[string]any{"enabled": false}, "model_mapping": map[string]any{"alias-flash": "deepseek-v4-flash"}},
		Extra:       account.Extra,
	}
	require.Equal(t, "", openCodeQuotaFallbackRoutingModelForBody(disabled, body, ""))
	require.Equal(t, "deepseek-v4-flash", resolveMappedUpstreamModel(disabled, body, ""))
}
