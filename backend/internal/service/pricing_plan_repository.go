package service

import "context"

// PricingPlanRepository 是定价套餐的持久化接口，同时服务管理端
// （套餐 CRUD）与认证/产品浏览端（公开产品、模型协议条目、按优先级路由）。
type PricingPlanRepository interface {
	// ListPlans 返回全部套餐；includeDisabled=false 时只返回 active 套餐。
	ListPlans(ctx context.Context, includeDisabled bool) ([]PricingPlan, error)
	// ListPublicProducts 返回公开可见（is_public 且 active）的套餐及其
	// 启用的模型协议条目，供产品列表/购买页读取。
	ListPublicProducts(ctx context.Context) ([]PricingPlanProduct, error)
	// GetPlanByID 返回单个套餐；不存在时返回 ErrPricingPlanNotFound。
	GetPlanByID(ctx context.Context, id int64) (*PricingPlan, error)
	// GetPlanByName 按稳定 key 返回套餐；不存在时返回 ErrPricingPlanNotFound。
	GetPlanByName(ctx context.Context, name string) (*PricingPlan, error)
	// ListModelsByPlan 返回套餐的模型协议条目，按 priority 升序。
	ListModelsByPlan(ctx context.Context, planID int64, includeDisabled bool) ([]PricingPlanModel, error)
	// ListRoutesByPlan 返回套餐的路由条目，按 priority 升序（同优先级按 id）。
	ListRoutesByPlan(ctx context.Context, planID int64, includeDisabled bool) ([]PricingPlanRoute, error)
	// CreatePlan 原子创建套餐及其模型协议条目、路由。
	CreatePlan(ctx context.Context, plan *PricingPlan, models []PricingPlanModel, routes []PricingPlanRoute) error
	// UpdatePlan 原子更新套餐及其模型协议条目、路由（先整体替换子条目）。
	UpdatePlan(ctx context.Context, plan *PricingPlan, models []PricingPlanModel, routes []PricingPlanRoute) error
	// ReplaceModels 在事务内整体替换套餐的模型协议条目。
	ReplaceModels(ctx context.Context, planID int64, models []PricingPlanModel) error
	// ReplaceRoutes 在事务内整体替换套餐的路由条目。
	ReplaceRoutes(ctx context.Context, planID int64, routes []PricingPlanRoute) error
	// DeletePlan 软删除套餐及其模型协议条目、路由。
	DeletePlan(ctx context.Context, id int64) error
}
