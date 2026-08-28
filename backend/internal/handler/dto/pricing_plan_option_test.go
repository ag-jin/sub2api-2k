package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestPricingPlanOptionFromService_StrictAllowlist 保证客户端点（KeysView
// 可选套餐）的序列化形状严格限定为 id/name/title/description：即便 service
// 层对象带有 status/is_public/sort_order/时间戳，也不得进入 JSON。
func TestPricingPlanOptionFromService_StrictAllowlist(t *testing.T) {
	opt := PricingPlanOptionFromService(&service.PricingPlan{
		ID:          7,
		Name:        "pro",
		Title:       "Pro Plan",
		Description: "All models",
		Status:      service.PricingPlanStatusActive,
		IsPublic:    true,
		SortOrder:   3,
	})
	require.NotNil(t, opt)
	require.Equal(t, int64(7), opt.ID)
	require.Equal(t, "pro", opt.Name)
	require.Equal(t, "Pro Plan", opt.Title)
	require.Equal(t, "All models", opt.Description)

	raw, err := json.Marshal(opt)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, map[string]any{
		"id":          float64(7),
		"name":        "pro",
		"title":       "Pro Plan",
		"description": "All models",
	}, m)

	// 禁止键深度断言：任何内部字段名都不得作为键出现。
	for _, forbidden := range []string{
		"status", "is_public", "sort_order", "created_at", "updated_at",
		"groups", "group_id", "routes", "plan_id", "models", "pricing",
		"cost", "upstream_model", "protocol", "direct", "enabled",
	} {
		_, present := m[forbidden]
		require.False(t, present, "customer plan payload must not contain %q", forbidden)
	}
}

func TestPricingPlanOptionFromService_NilPassthrough(t *testing.T) {
	require.Nil(t, PricingPlanOptionFromService(nil))
}
