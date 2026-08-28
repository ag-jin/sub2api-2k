package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

func newPricingPlanRepoSQLite(t *testing.T) (service.PricingPlanRepository, *dbent.Client) {
	t.Helper()

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })

	return NewPricingPlanRepository(client), client
}

func mustCreatePricingPlanRepoPlan(t *testing.T, ctx context.Context, repo service.PricingPlanRepository, name string) *service.PricingPlan {
	t.Helper()
	plan := &service.PricingPlan{
		Name:      name,
		Title:     "Plan " + name,
		Status:    service.PricingPlanStatusActive,
		IsPublic:  true,
		SortOrder: 10,
	}
	require.NoError(t, repo.CreatePlan(ctx, plan, nil, nil))
	return plan
}

func mustCreatePricingPlanRepoGroup(t *testing.T, ctx context.Context, client *dbent.Client, name string) int64 {
	t.Helper()
	group, err := client.Group.Create().
		SetName(name).
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1).
		Save(ctx)
	require.NoError(t, err)
	return group.ID
}

func TestPricingPlanRepositoryCreatePlanPersistsModelsAndRoutes(t *testing.T) {
	repo, client := newPricingPlanRepoSQLite(t)
	ctx := context.Background()
	firstGroupID := mustCreatePricingPlanRepoGroup(t, ctx, client, "pricing-plan-first")
	backupGroupID := mustCreatePricingPlanRepoGroup(t, ctx, client, "pricing-plan-backup")

	plan := &service.PricingPlan{Name: "pro", Title: "Pro", Status: service.PricingPlanStatusActive, IsPublic: true, SortOrder: 5}
	models := []service.PricingPlanModel{
		{PublicModel: "gpt-5", Protocol: service.PricingPlanProtocolChatCompletions, Priority: 10, Enabled: true},
		{PublicModel: "claude-sonnet-4-6", Protocol: service.PricingPlanProtocolMessages, UpstreamModel: "claude-sonnet-4-6", Priority: 20, Enabled: true},
	}
	routes := []service.PricingPlanRoute{
		{GroupID: backupGroupID, Priority: 30, Enabled: true},
		{GroupID: firstGroupID, Priority: 10, Enabled: true},
	}
	require.NoError(t, repo.CreatePlan(ctx, plan, models, routes))
	require.NotZero(t, plan.ID)

	gotModels, err := repo.ListModelsByPlan(ctx, plan.ID, false)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5", "claude-sonnet-4-6"}, []string{gotModels[0].PublicModel, gotModels[1].PublicModel})

	gotRoutes, err := repo.ListRoutesByPlan(ctx, plan.ID, false)
	require.NoError(t, err)
	require.Equal(t, []int64{firstGroupID, backupGroupID}, []int64{gotRoutes[0].GroupID, gotRoutes[1].GroupID})
	require.Equal(t, []int{10, 30}, []int{gotRoutes[0].Priority, gotRoutes[1].Priority})
}

func TestPricingPlanRepositoryListPublicProducts(t *testing.T) {
	repo, _ := newPricingPlanRepoSQLite(t)
	ctx := context.Background()

	public := mustCreatePricingPlanRepoPlan(t, ctx, repo, "public-plan")
	require.NoError(t, repo.ReplaceModels(ctx, public.ID, []service.PricingPlanModel{
		{PublicModel: "gpt-5", Protocol: service.PricingPlanProtocolChatCompletions, Priority: 10, Enabled: true},
		{PublicModel: "gpt-5-preview", Protocol: service.PricingPlanProtocolChatCompletions, Priority: 20, Enabled: false},
	}))
	disabled := &service.PricingPlan{Name: "retired-plan", Title: "Retired", Status: service.PricingPlanStatusDisabled, IsPublic: true}
	require.NoError(t, repo.CreatePlan(ctx, disabled, nil, nil))

	products, err := repo.ListPublicProducts(ctx)
	require.NoError(t, err)
	require.Len(t, products, 1)
	require.Equal(t, "public-plan", products[0].Plan.Name)
	require.Len(t, products[0].Models, 1)
}

func TestPricingPlanRepositoryUpdatePlanReplacesContentAtomically(t *testing.T) {
	repo, client := newPricingPlanRepoSQLite(t)
	ctx := context.Background()
	oldGroupID := mustCreatePricingPlanRepoGroup(t, ctx, client, "pricing-plan-old")
	newGroupID := mustCreatePricingPlanRepoGroup(t, ctx, client, "pricing-plan-new")
	plan := mustCreatePricingPlanRepoPlan(t, ctx, repo, "atomic-plan")
	require.NoError(t, repo.ReplaceModels(ctx, plan.ID, []service.PricingPlanModel{{PublicModel: "old-model", Protocol: service.PricingPlanProtocolChatCompletions, Enabled: true}}))
	require.NoError(t, repo.ReplaceRoutes(ctx, plan.ID, []service.PricingPlanRoute{{GroupID: oldGroupID, Priority: 10, Enabled: true}}))

	plan.Title = "Atomic Plan v2"
	require.NoError(t, repo.UpdatePlan(ctx, plan,
		[]service.PricingPlanModel{{PublicModel: "new-model", Protocol: service.PricingPlanProtocolResponses, Enabled: true}},
		[]service.PricingPlanRoute{{GroupID: newGroupID, Priority: 5, Enabled: true}},
	))
	gotRoutes, err := repo.ListRoutesByPlan(ctx, plan.ID, false)
	require.NoError(t, err)
	require.Equal(t, newGroupID, gotRoutes[0].GroupID)

	allModels, err := client.PricingPlanModel.Query().All(mixins.SkipSoftDelete(ctx))
	require.NoError(t, err)
	require.Len(t, allModels, 2)
}

func TestPricingPlanRepositoryReplaceRoutesRollsBackOnForeignKeyFailure(t *testing.T) {
	repo, client := newPricingPlanRepoSQLite(t)
	ctx := context.Background()
	groupID := mustCreatePricingPlanRepoGroup(t, ctx, client, "pricing-plan-rollback")
	plan := mustCreatePricingPlanRepoPlan(t, ctx, repo, "rollback-routes")
	require.NoError(t, repo.ReplaceRoutes(ctx, plan.ID, []service.PricingPlanRoute{{GroupID: groupID, Priority: 10, Enabled: true}}))

	err := repo.ReplaceRoutes(ctx, plan.ID, []service.PricingPlanRoute{
		{GroupID: groupID, Priority: 10, Enabled: true},
		{GroupID: 999999, Priority: 20, Enabled: true},
	})
	require.Error(t, err)
	gotRoutes, err := repo.ListRoutesByPlan(ctx, plan.ID, true)
	require.NoError(t, err)
	require.Len(t, gotRoutes, 1)
	require.Equal(t, groupID, gotRoutes[0].GroupID)
}

func TestPricingPlanRepositoryDeletePlanSoftDeletesChildren(t *testing.T) {
	repo, client := newPricingPlanRepoSQLite(t)
	ctx := context.Background()
	groupID := mustCreatePricingPlanRepoGroup(t, ctx, client, "pricing-plan-delete")
	plan := mustCreatePricingPlanRepoPlan(t, ctx, repo, "delete-plan")
	require.NoError(t, repo.ReplaceModels(ctx, plan.ID, []service.PricingPlanModel{{PublicModel: "gpt-5", Protocol: service.PricingPlanProtocolChatCompletions, Enabled: true}}))
	require.NoError(t, repo.ReplaceRoutes(ctx, plan.ID, []service.PricingPlanRoute{{GroupID: groupID, Priority: 10, Enabled: true}}))

	require.NoError(t, repo.DeletePlan(ctx, plan.ID))
	_, err := repo.GetPlanByID(ctx, plan.ID)
	require.ErrorIs(t, err, service.ErrPricingPlanNotFound)
	count, err := client.PricingPlan.Query().Count(mixins.SkipSoftDelete(ctx))
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPricingPlanRepositoryDeletePlanClearsAPIKeyBinding(t *testing.T) {
	repo, client := newPricingPlanRepoSQLite(t)
	ctx := context.Background()
	plan := mustCreatePricingPlanRepoPlan(t, ctx, repo, "unbind-plan")
	user, err := client.User.Create().
		SetEmail("unbind-plan@example.com").
		SetPasswordHash("hash").
		SetRole(service.RoleUser).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	key, err := client.APIKey.Create().
		SetUserID(user.ID).
		SetKey("sk-unbind-plan").
		SetName("unbind-plan").
		SetStatus(service.StatusActive).
		SetPricingPlanID(plan.ID).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, repo.DeletePlan(ctx, plan.ID))
	got, err := client.APIKey.Get(ctx, key.ID)
	require.NoError(t, err)
	require.Nil(t, got.PricingPlanID)
}

func TestPricingPlanRepositoryNotFoundPaths(t *testing.T) {
	repo, _ := newPricingPlanRepoSQLite(t)
	ctx := context.Background()
	_, err := repo.GetPlanByID(ctx, 123456)
	require.ErrorIs(t, err, service.ErrPricingPlanNotFound)
	require.ErrorIs(t, repo.ReplaceModels(ctx, 123456, nil), service.ErrPricingPlanNotFound)
	require.ErrorIs(t, repo.ReplaceRoutes(ctx, 123456, nil), service.ErrPricingPlanNotFound)
	require.ErrorIs(t, repo.DeletePlan(ctx, 123456), service.ErrPricingPlanNotFound)
}
