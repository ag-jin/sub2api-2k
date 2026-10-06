//go:build unit

package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// MonitorQuotaSnapshot 是 channel_monitor_histories.quota（JSONB）的序列化形态，
// 字段名即前端契约（design M4）。本组用例把设计里的 JSON 形状固化成回归：
//   - 老历史行（无 M4 字段）必须能解码且新字段为零值；
//   - 新字段按设计键名输出，且为空/零时不出现在 JSON 里（omitempty）。

// 老快照：M4 之前的实际写入形状（source/success/tiers/balance_low/credential_invalid/
// fetched_at），不得因为追加字段而解码失败。
const monitorQuotaLegacySnapshotJSONForM4Test = `{
  "source": "cn_quota",
  "success": true,
  "tiers": [{"window": "5h", "used_percent": 42.5}, {"window": "weekly", "label": "zhipu", "used_percent": 10}],
  "balance_low": false,
  "credential_invalid": false,
  "fetched_at": "2026-08-18T06:00:00Z"
}`

func TestMonitorQuotaSnapshot_LegacyJSONBDecodesWithZeroNewFields(t *testing.T) {
	var snapshot MonitorQuotaSnapshot
	require.NoError(t, json.Unmarshal([]byte(monitorQuotaLegacySnapshotJSONForM4Test), &snapshot))

	require.Equal(t, "cn_quota", snapshot.Source)
	require.True(t, snapshot.Success)
	require.Len(t, snapshot.Tiers, 2)

	// M4 新字段：老行缺字段 → 零值，不报错。
	require.Nil(t, snapshot.ModelCredits)
	require.Nil(t, snapshot.ResetCards)
	require.False(t, snapshot.NeedsRelogin)
}

func TestMonitorQuotaSnapshot_M4FieldsMarshalWithDesignKeys(t *testing.T) {
	snapshot := MonitorQuotaSnapshot{
		Source:  "cn_quota",
		Success: true,
		Tiers:   []MonitorQuotaTier{{Window: "5h", UsedPercent: 42.5}},
		ModelCredits: []MonitorQuotaModelCredit{
			{Model: "glm-5.3", Date: "2026-10-05", InputTokens: 1200, CachedTokens: 340, OutputTokens: 560, Credits: 1.25},
			{Model: "glm-4.6-flash", Date: "2026-10-06", InputTokens: 20, CachedTokens: 0, OutputTokens: 5, Credits: 0.01},
		},
		ResetCards:   []MonitorResetCard{{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00+08:00"}, {Type: "week", ExpireAt: "2026-10-13T00:00:00+08:00"}},
		NeedsRelogin: true,
	}

	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)

	// 期望值来自 design M4 的字段契约（独立于实现书写）。
	require.JSONEq(t, `{
	  "source": "cn_quota",
	  "success": true,
	  "tiers": [{"window": "5h", "used_percent": 42.5}],
	  "fetched_at": "0001-01-01T00:00:00Z",
	  "model_credits": [
	    {"model": "glm-5.3", "date": "2026-10-05", "input_tokens": 1200, "cached_tokens": 340, "output_tokens": 560, "credits": 1.25},
	    {"model": "glm-4.6-flash", "date": "2026-10-06", "input_tokens": 20, "cached_tokens": 0, "output_tokens": 5, "credits": 0.01}
	  ],
	  "reset_cards": [
	    {"type": "five_hour", "expire_at": "2026-10-06T12:00:00+08:00"},
	    {"type": "week", "expire_at": "2026-10-13T00:00:00+08:00"}
	  ],
	  "needs_relogin": true
	}`, string(raw))
}

func TestMonitorQuotaSnapshot_M4FieldsOmittedWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(MonitorQuotaSnapshot{Source: "cn_quota", Success: true})
	require.NoError(t, err)

	// omitempty 契约：无 M4 数据时的 JSON 形状与改动前逐字节等价
	// （前端 16/30 必须能对缺字段静默降级）。
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.NotContains(t, fields, "model_credits")
	require.NotContains(t, fields, "reset_cards")
	require.NotContains(t, fields, "needs_relogin")
}

func TestMonitorQuotaModelCredit_RoundTripPreservesFieldSemantics(t *testing.T) {
	// 逐日逐模型一行：credits 取当日总量，InputTokens 为未缓存输入。
	source := MonitorQuotaModelCredit{
		Model:        "glm-5.3",
		Date:         "2026-10-05",
		InputTokens:  1500.5,
		CachedTokens: 200,
		OutputTokens: 42.25,
		Credits:      0.75,
	}
	raw, err := json.Marshal(source)
	require.NoError(t, err)

	var decoded MonitorQuotaModelCredit
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, source, decoded)
}
