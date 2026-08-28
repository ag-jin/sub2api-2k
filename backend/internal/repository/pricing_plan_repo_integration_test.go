//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestPricingPlanRepositoryIntegration 验证定价套餐在真实 PostgreSQL 上的
// 完整读写：按优先级排序、公开产品过滤，以及 api_keys.pricing_plan_id
// 的 ON DELETE SET NULL 外键行为。
func TestPricingPlanRepositoryIntegration(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewPricingPlanRepository(client)
	suffix := time.Now().UnixNano()
	firstGroup, err := client.Group.Create().
		SetName(fmt.Sprintf("plan-route-first-%d", suffix)).
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1).
		Save(ctx)
	require.NoError(t, err)
	backupGroup, err := client.Group.Create().
		SetName(fmt.Sprintf("plan-route-backup-%d", suffix)).
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2)", firstGroup.ID, backupGroup.ID)
	})

	plan := &service.PricingPlan{
		Name:      fmt.Sprintf("plan-%d", suffix),
		Title:     "Integration Plan",
		Status:    service.PricingPlanStatusActive,
		IsPublic:  true,
		SortOrder: 10,
	}
	models := []service.PricingPlanModel{
		{PublicModel: "claude-sonnet-4-6", Protocol: service.PricingPlanProtocolMessages, Priority: 20, Enabled: true},
		{PublicModel: "gpt-5", Protocol: service.PricingPlanProtocolChatCompletions, Priority: 10, Enabled: true},
	}
	routes := []service.PricingPlanRoute{
		{GroupID: backupGroup.ID, Priority: 30, Enabled: true},
		{GroupID: firstGroup.ID, Priority: 10, Enabled: true},
	}
	require.NoError(t, repo.CreatePlan(ctx, plan, models, routes))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM pricing_plans WHERE id = $1", plan.ID)
	})

	// 路由按 priority 升序（低值优先）
	gotRoutes, err := repo.ListRoutesByPlan(ctx, plan.ID, false)
	require.NoError(t, err)
	require.Len(t, gotRoutes, 2)
	require.Equal(t, firstGroup.ID, gotRoutes[0].GroupID)
	require.Equal(t, backupGroup.ID, gotRoutes[1].GroupID)

	// 模型协议条目按 priority 升序
	gotModels, err := repo.ListModelsByPlan(ctx, plan.ID, false)
	require.NoError(t, err)
	require.Len(t, gotModels, 2)
	require.Equal(t, "gpt-5", gotModels[0].PublicModel)
	require.Equal(t, service.PricingPlanProtocolMessages, gotModels[1].Protocol)

	// 公开产品列表
	products, err := repo.ListPublicProducts(ctx)
	require.NoError(t, err)
	var found *service.PricingPlanProduct
	for i := range products {
		if products[i].Plan.ID == plan.ID {
			found = &products[i]
			break
		}
	}
	require.NotNil(t, found)
	require.Len(t, found.Models, 2)

	// api_keys.pricing_plan_id 外键：物理删除套餐后置 NULL（group_id 不受影响）
	user, err := client.User.Create().
		SetEmail(fmt.Sprintf("plan-key-%d@test.com", suffix)).
		SetPasswordHash("test-password-hash").
		SetRole(service.RoleUser).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	key, err := client.APIKey.Create().
		SetUserID(user.ID).
		SetKey(fmt.Sprintf("sk-plan-%d", suffix)).
		SetName("plan-key").
		SetStatus(service.StatusActive).
		SetPricingPlanID(plan.ID).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM api_keys WHERE id = $1", key.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})

	var planID *int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT pricing_plan_id FROM api_keys WHERE id = $1", key.ID).Scan(&planID))
	require.NotNil(t, planID)
	require.Equal(t, plan.ID, *planID)

	_, err = integrationDB.ExecContext(ctx, "DELETE FROM pricing_plans WHERE id = $1", plan.ID)
	require.NoError(t, err)

	var afterDelete *int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT pricing_plan_id FROM api_keys WHERE id = $1", key.ID).Scan(&afterDelete))
	require.Nil(t, afterDelete, "pricing_plan_id 应因 ON DELETE SET NULL 置空")
}
