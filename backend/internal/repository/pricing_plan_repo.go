package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/pricingplan"
	"github.com/Wei-Shaw/sub2api/ent/pricingplanmodel"
	"github.com/Wei-Shaw/sub2api/ent/pricingplanroute"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type pricingPlanRepository struct {
	client *dbent.Client
	// authCacheInvalidator 可选的认证缓存失效接入点（由 wire 在 APIKeyService
	// 构造后注入）：套餐内容/删除变更时按 planID 批量失效绑定 Key 的快照，
	// 覆盖 Redis（L2）与各实例进程内 L1。nil 表示未接入（测试/未注入），跳过。
	authCacheInvalidator service.APIKeyAuthCacheInvalidator
	planCacheInvalidator service.APIKeyPricingPlanAuthCacheInvalidator
}

func NewPricingPlanRepository(client *dbent.Client) service.PricingPlanRepository {
	return &pricingPlanRepository{client: client}
}

// SetAuthCacheInvalidator 注入认证缓存失效器。经 service/wire.go 在
// APIKeyService 构造完成后调用，避免仓储与 APIKeyService 的构造环。
func (r *pricingPlanRepository) SetAuthCacheInvalidator(invalidator service.APIKeyAuthCacheInvalidator) {
	r.authCacheInvalidator = invalidator
	if planInvalidator, ok := invalidator.(service.APIKeyPricingPlanAuthCacheInvalidator); ok {
		r.planCacheInvalidator = planInvalidator
	}
}

// withTx 在事务中执行 fn：若 context 已带事务则复用，否则开启新事务。
func (r *pricingPlanRepository) withTx(ctx context.Context, fn func(txCtx context.Context, txClient *dbent.Client) error) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin pricing plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	if err := fn(txCtx, tx.Client()); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pricing plan transaction: %w", err)
	}
	return nil
}

func (r *pricingPlanRepository) ListPlans(ctx context.Context, includeDisabled bool) ([]service.PricingPlan, error) {
	q := clientFromContext(ctx, r.client).PricingPlan.Query().
		Order(
			dbent.Asc(pricingplan.FieldSortOrder),
			dbent.Asc(pricingplan.FieldID),
		)
	if !includeDisabled {
		q = q.Where(pricingplan.StatusEQ(service.PricingPlanStatusActive))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.PricingPlan, 0, len(rows))
	for _, row := range rows {
		out = append(out, *pricingPlanEntityToService(row))
	}
	return out, nil
}

func (r *pricingPlanRepository) ListPublicProducts(ctx context.Context) ([]service.PricingPlanProduct, error) {
	plans, err := clientFromContext(ctx, r.client).PricingPlan.Query().
		Where(
			pricingplan.IsPublicEQ(true),
			pricingplan.StatusEQ(service.PricingPlanStatusActive),
		).
		Order(
			dbent.Asc(pricingplan.FieldSortOrder),
			dbent.Asc(pricingplan.FieldID),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.PricingPlanProduct, 0, len(plans))
	for _, plan := range plans {
		models, err := r.ListModelsByPlan(ctx, plan.ID, false)
		if err != nil {
			return nil, err
		}
		out = append(out, service.PricingPlanProduct{
			Plan:   *pricingPlanEntityToService(plan),
			Models: models,
		})
	}
	return out, nil
}

func (r *pricingPlanRepository) GetPlanByID(ctx context.Context, id int64) (*service.PricingPlan, error) {
	row, err := clientFromContext(ctx, r.client).PricingPlan.Get(ctx, id)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrPricingPlanNotFound, nil)
	}
	return pricingPlanEntityToService(row), nil
}

func (r *pricingPlanRepository) GetPlanByName(ctx context.Context, name string) (*service.PricingPlan, error) {
	row, err := clientFromContext(ctx, r.client).PricingPlan.Query().
		Where(pricingplan.NameEQ(name)).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrPricingPlanNotFound, nil)
	}
	return pricingPlanEntityToService(row), nil
}

func (r *pricingPlanRepository) ListModelsByPlan(ctx context.Context, planID int64, includeDisabled bool) ([]service.PricingPlanModel, error) {
	q := clientFromContext(ctx, r.client).PricingPlanModel.Query().
		Where(pricingplanmodel.PlanIDEQ(planID)).
		Order(
			dbent.Asc(pricingplanmodel.FieldPriority),
			dbent.Asc(pricingplanmodel.FieldID),
		)
	if !includeDisabled {
		q = q.Where(pricingplanmodel.EnabledEQ(true))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.PricingPlanModel, 0, len(rows))
	for _, row := range rows {
		out = append(out, *pricingPlanModelEntityToService(row))
	}
	return out, nil
}

func (r *pricingPlanRepository) ListRoutesByPlan(ctx context.Context, planID int64, includeDisabled bool) ([]service.PricingPlanRoute, error) {
	q := clientFromContext(ctx, r.client).PricingPlanRoute.Query().
		Where(pricingplanroute.PlanIDEQ(planID)).
		Order(
			dbent.Asc(pricingplanroute.FieldPriority),
			dbent.Asc(pricingplanroute.FieldID),
		)
	if !includeDisabled {
		q = q.Where(pricingplanroute.EnabledEQ(true))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.PricingPlanRoute, 0, len(rows))
	for _, row := range rows {
		out = append(out, *pricingPlanRouteEntityToService(row))
	}
	return out, nil
}

func (r *pricingPlanRepository) CreatePlan(ctx context.Context, plan *service.PricingPlan, models []service.PricingPlanModel, routes []service.PricingPlanRoute) error {
	if plan == nil {
		return service.ErrPricingPlanNotFound
	}
	return r.withTx(ctx, func(txCtx context.Context, c *dbent.Client) error {
		created, err := c.PricingPlan.Create().
			SetName(plan.Name).
			SetTitle(plan.Title).
			SetDescription(plan.Description).
			SetStatus(plan.Status).
			SetIsPublic(plan.IsPublic).
			SetSortOrder(plan.SortOrder).
			Save(txCtx)
		if err != nil {
			return translatePersistenceError(err, nil, service.ErrPricingPlanExists)
		}
		*plan = *pricingPlanEntityToService(created)
		if err := bulkCreatePlanModels(txCtx, c, created.ID, models); err != nil {
			return err
		}
		return bulkCreatePlanRoutes(txCtx, c, created.ID, routes)
	})
}

func (r *pricingPlanRepository) UpdatePlan(ctx context.Context, plan *service.PricingPlan, models []service.PricingPlanModel, routes []service.PricingPlanRoute) error {
	if plan == nil {
		return service.ErrPricingPlanNotFound
	}
	err := r.withTx(ctx, func(txCtx context.Context, c *dbent.Client) error {
		updated, err := c.PricingPlan.UpdateOneID(plan.ID).
			SetName(plan.Name).
			SetTitle(plan.Title).
			SetDescription(plan.Description).
			SetStatus(plan.Status).
			SetIsPublic(plan.IsPublic).
			SetSortOrder(plan.SortOrder).
			Save(txCtx)
		if err != nil {
			return translatePersistenceError(err, service.ErrPricingPlanNotFound, service.ErrPricingPlanExists)
		}
		*plan = *pricingPlanEntityToService(updated)
		if err := replacePlanModels(txCtx, c, plan.ID, models); err != nil {
			return err
		}
		return replacePlanRoutes(txCtx, c, plan.ID, routes)
	})
	if err != nil {
		return err
	}
	// 套餐内容（含模型协议条目与路由层）已整体替换：绑定 Key 的认证快照
	// 携带旧内容，必须批量失效，否则网关继续按旧条目调度。
	r.invalidatePlanAuthCache(ctx, plan.ID)
	return nil
}

func (r *pricingPlanRepository) ReplaceModels(ctx context.Context, planID int64, models []service.PricingPlanModel) error {
	err := r.withTx(ctx, func(txCtx context.Context, c *dbent.Client) error {
		if _, err := c.PricingPlan.Get(txCtx, planID); err != nil {
			return translatePersistenceError(err, service.ErrPricingPlanNotFound, nil)
		}
		return replacePlanModels(txCtx, c, planID, models)
	})
	if err != nil {
		return err
	}
	r.invalidatePlanAuthCache(ctx, planID)
	return nil
}

func (r *pricingPlanRepository) ReplaceRoutes(ctx context.Context, planID int64, routes []service.PricingPlanRoute) error {
	err := r.withTx(ctx, func(txCtx context.Context, c *dbent.Client) error {
		if _, err := c.PricingPlan.Get(txCtx, planID); err != nil {
			return translatePersistenceError(err, service.ErrPricingPlanNotFound, nil)
		}
		return replacePlanRoutes(txCtx, c, planID, routes)
	})
	if err != nil {
		return err
	}
	r.invalidatePlanAuthCache(ctx, planID)
	return nil
}

func (r *pricingPlanRepository) DeletePlan(ctx context.Context, id int64) error {
	// 删除事务会先把绑定 Key 的 pricing_plan_id 置空（ON DELETE SET NULL 语义），
	// 提交后无法再按 planID 反查，因此先取 Key 快照；随后按 planID 做一次预失效
	// （与 GroupService.Delete 的失效顺序一致），提交后再按快照逐个 Key 失效，
	// 覆盖删除事务期间重建的认证条目。
	keys, err := r.apiKeysByPlan(ctx, id)
	if err != nil {
		return err
	}
	r.invalidatePlanAuthCache(ctx, id)
	if err := r.withTx(ctx, func(txCtx context.Context, c *dbent.Client) error {
		// 软删除套餐前先解除 API key 绑定：等价于迁移中物理删除的
		// ON DELETE SET NULL 语义（group_id 不受影响）。
		if _, err := c.APIKey.Update().
			Where(apikey.PricingPlanIDEQ(id)).
			ClearPricingPlanID().
			Save(txCtx); err != nil {
			return err
		}
		// 子条目一并软删除，保持查询（自动过滤 deleted_at）的一致语义
		if _, err := c.PricingPlanModel.Delete().
			Where(pricingplanmodel.PlanIDEQ(id)).
			Exec(txCtx); err != nil {
			return err
		}
		if _, err := c.PricingPlanRoute.Delete().
			Where(pricingplanroute.PlanIDEQ(id)).
			Exec(txCtx); err != nil {
			return err
		}
		err := c.PricingPlan.DeleteOneID(id).Exec(txCtx)
		return translatePersistenceError(err, service.ErrPricingPlanNotFound, nil)
	}); err != nil {
		return err
	}
	r.invalidateKeysAuthCache(ctx, keys)
	return nil
}

// invalidatePlanAuthCache 按 planID 批量失效绑定该套餐的 API Key 认证缓存。
// 未注入失效器（测试/直接构造）时为空操作。
func (r *pricingPlanRepository) invalidatePlanAuthCache(ctx context.Context, planID int64) {
	if r.planCacheInvalidator == nil {
		return
	}
	r.planCacheInvalidator.InvalidateAuthCacheByPricingPlanID(ctx, planID)
}

// invalidateKeysAuthCache 逐个失效指定 API Key credential 的认证缓存。
func (r *pricingPlanRepository) invalidateKeysAuthCache(ctx context.Context, keys []string) {
	if r.authCacheInvalidator == nil {
		return
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		r.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, key)
	}
}

// apiKeysByPlan 返回绑定指定套餐的 API Key credential（含软删除过滤）。
func (r *pricingPlanRepository) apiKeysByPlan(ctx context.Context, planID int64) ([]string, error) {
	return clientFromContext(ctx, r.client).APIKey.Query().
		Where(apikey.PricingPlanIDEQ(planID), apikey.DeletedAtIsNil()).
		Select(apikey.FieldKey).
		Strings(ctx)
}

// replacePlanModels 软删除 planID 的既有模型条目并批量写入新条目（整体替换）。
func replacePlanModels(ctx context.Context, c *dbent.Client, planID int64, models []service.PricingPlanModel) error {
	if _, err := c.PricingPlanModel.Delete().
		Where(pricingplanmodel.PlanIDEQ(planID)).
		Exec(ctx); err != nil {
		return err
	}
	return bulkCreatePlanModels(ctx, c, planID, models)
}

// replacePlanRoutes 软删除 planID 的既有路由并批量写入新条目（整体替换）。
func replacePlanRoutes(ctx context.Context, c *dbent.Client, planID int64, routes []service.PricingPlanRoute) error {
	if _, err := c.PricingPlanRoute.Delete().
		Where(pricingplanroute.PlanIDEQ(planID)).
		Exec(ctx); err != nil {
		return err
	}
	return bulkCreatePlanRoutes(ctx, c, planID, routes)
}

func bulkCreatePlanModels(ctx context.Context, c *dbent.Client, planID int64, models []service.PricingPlanModel) error {
	builders := make([]*dbent.PricingPlanModelCreate, 0, len(models))
	for _, model := range models {
		builder := c.PricingPlanModel.Create().
			SetPlanID(planID).
			SetPublicModel(model.PublicModel).
			SetProtocol(model.Protocol).
			SetUpstreamModel(model.UpstreamModel).
			SetDirect(model.Direct).
			SetAllowCompatibilityFallback(model.AllowCompatibilityFallback).
			SetPriority(model.Priority).
			SetEnabled(model.Enabled).
			SetNotes(model.Notes)
		if model.Pricing != nil {
			builder = builder.SetPricing(service.PlanModelPricingToDomain(model.Pricing))
		}
		builders = append(builders, builder)
	}
	created, err := c.PricingPlanModel.CreateBulk(builders...).Save(ctx)
	if err != nil {
		return translatePersistenceError(err, nil, service.ErrPricingPlanExists)
	}
	for i := range created {
		if i < len(models) {
			models[i] = *pricingPlanModelEntityToService(created[i])
		}
	}
	return nil
}

func bulkCreatePlanRoutes(ctx context.Context, c *dbent.Client, planID int64, routes []service.PricingPlanRoute) error {
	builders := make([]*dbent.PricingPlanRouteCreate, 0, len(routes))
	for _, route := range routes {
		builders = append(builders, c.PricingPlanRoute.Create().
			SetPlanID(planID).
			SetGroupID(route.GroupID).
			SetPriority(route.Priority).
			SetEnabled(route.Enabled))
	}
	created, err := c.PricingPlanRoute.CreateBulk(builders...).Save(ctx)
	if err != nil {
		if isForeignKeyViolation(err) {
			return service.ErrPricingPlanInvalidInput.WithCause(err)
		}
		return translatePersistenceError(err, nil, service.ErrPricingPlanExists)
	}
	for i := range created {
		if i < len(routes) {
			routes[i] = *pricingPlanRouteEntityToService(created[i])
		}
	}
	return nil
}

func pricingPlanEntityToService(row *dbent.PricingPlan) *service.PricingPlan {
	if row == nil {
		return nil
	}
	return &service.PricingPlan{
		ID:          row.ID,
		Name:        row.Name,
		Title:       row.Title,
		Description: derefString(row.Description),
		Status:      row.Status,
		IsPublic:    row.IsPublic,
		SortOrder:   row.SortOrder,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

func pricingPlanModelEntityToService(row *dbent.PricingPlanModel) *service.PricingPlanModel {
	if row == nil {
		return nil
	}
	return &service.PricingPlanModel{
		ID:                         row.ID,
		PlanID:                     row.PlanID,
		PublicModel:                row.PublicModel,
		Protocol:                   row.Protocol,
		UpstreamModel:              row.UpstreamModel,
		Direct:                     row.Direct,
		AllowCompatibilityFallback: row.AllowCompatibilityFallback,
		Priority:                   row.Priority,
		Enabled:                    row.Enabled,
		Notes:                      derefString(row.Notes),
		Pricing:                    service.PlanModelPricingToService(row.Pricing),
		CreatedAt:                  row.CreatedAt,
		UpdatedAt:                  row.UpdatedAt,
	}
}

func pricingPlanRouteEntityToService(row *dbent.PricingPlanRoute) *service.PricingPlanRoute {
	if row == nil {
		return nil
	}
	return &service.PricingPlanRoute{
		ID:        row.ID,
		PlanID:    row.PlanID,
		GroupID:   row.GroupID,
		Priority:  row.Priority,
		Enabled:   row.Enabled,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
