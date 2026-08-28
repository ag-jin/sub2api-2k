package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// PricingPlanHandler 处理定价套餐管理端 CRUD：套餐本体（名称/标题/描述/
// 状态/公开标记/排序）连同模型协议条目（含销售定价）与内部路由层一起维护。
// 详情响应只走管理端路由；公开/用户端点另有白名单 DTO，不经过本 handler。
type PricingPlanHandler struct {
	pricingPlanService *service.PricingPlanService
}

// NewPricingPlanHandler 创建定价套餐管理 handler。
func NewPricingPlanHandler(pricingPlanService *service.PricingPlanService) *PricingPlanHandler {
	return &PricingPlanHandler{
		pricingPlanService: pricingPlanService,
	}
}

// PricingPlanModelRequest 模型协议条目的创建/更新请求。Pricing 直接绑定
// domain.PlanModelPricing（持久化 JSONB 的定价文档形状，无渠道/实体标识）。
type PricingPlanModelRequest struct {
	PublicModel                string                   `json:"public_model" binding:"required"`
	Protocol                   string                   `json:"protocol"`
	UpstreamModel              string                   `json:"upstream_model"`
	Direct                     bool                     `json:"direct"`
	AllowCompatibilityFallback bool                     `json:"allow_compatibility_fallback"`
	Priority                   int                      `json:"priority"`
	Enabled                    bool                     `json:"enabled"`
	Notes                      string                   `json:"notes"`
	Pricing                    *domain.PlanModelPricing `json:"pricing"`
}

// PricingPlanRouteRequest 内部路由层条目的创建/更新请求。
type PricingPlanRouteRequest struct {
	GroupID  int64 `json:"group_id" binding:"required,gt=0"`
	Priority int   `json:"priority"`
	Enabled  bool  `json:"enabled"`
}

// UpsertPricingPlanRequest 套餐创建/更新请求（整体替换语义：models/routes
// 全量下发，空数组即清空）。
type UpsertPricingPlanRequest struct {
	Name        string                    `json:"name" binding:"required"`
	Title       string                    `json:"title"`
	Description string                    `json:"description"`
	Status      string                    `json:"status" binding:"omitempty,oneof=active disabled"`
	IsPublic    bool                      `json:"is_public"`
	SortOrder   int                       `json:"sort_order"`
	Models      []PricingPlanModelRequest `json:"models"`
	Routes      []PricingPlanRouteRequest `json:"routes"`
}

// adminPricingPlanModel 模型协议条目的管理端响应（含销售定价配置）。
type adminPricingPlanModel struct {
	ID                         int64                    `json:"id"`
	PlanID                     int64                    `json:"plan_id"`
	PublicModel                string                   `json:"public_model"`
	Protocol                   string                   `json:"protocol"`
	UpstreamModel              string                   `json:"upstream_model"`
	Direct                     bool                     `json:"direct"`
	AllowCompatibilityFallback bool                     `json:"allow_compatibility_fallback"`
	Priority                   int                      `json:"priority"`
	Enabled                    bool                     `json:"enabled"`
	Notes                      string                   `json:"notes"`
	Pricing                    *domain.PlanModelPricing `json:"pricing,omitempty"`
	CreatedAt                  time.Time                `json:"created_at"`
	UpdatedAt                  time.Time                `json:"updated_at"`
}

// adminPricingPlanRoute 内部路由层的管理端响应（含内部 group_id）。
type adminPricingPlanRoute struct {
	ID        int64     `json:"id"`
	PlanID    int64     `json:"plan_id"`
	GroupID   int64     `json:"group_id"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// adminPricingPlan 套餐的管理端响应（列表/详情共用明细结构）。
type adminPricingPlan struct {
	ID          int64                   `json:"id"`
	Name        string                  `json:"name"`
	Title       string                  `json:"title"`
	Description string                  `json:"description"`
	Status      string                  `json:"status"`
	IsPublic    bool                    `json:"is_public"`
	SortOrder   int                     `json:"sort_order"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
	Models      []adminPricingPlanModel `json:"models"`
	Routes      []adminPricingPlanRoute `json:"routes"`
}

// List 列出套餐；include_disabled=true 时包含停用套餐。
// GET /api/v1/admin/pricing-plans
func (h *PricingPlanHandler) List(c *gin.Context) {
	includeDisabled := strings.EqualFold(c.DefaultQuery("include_disabled", "false"), "true")
	plans, err := h.pricingPlanService.List(c.Request.Context(), includeDisabled)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]adminPricingPlan, 0, len(plans))
	for i := range plans {
		out = append(out, adminPricingPlanFromService(&plans[i], nil, nil))
	}
	response.Success(c, out)
}

// GetByID 返回套餐及其模型协议条目、路由层（管理端全量视图）。
// GET /api/v1/admin/pricing-plans/:id
func (h *PricingPlanHandler) GetByID(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || planID <= 0 {
		response.BadRequest(c, "Invalid pricing plan ID")
		return
	}
	detail, err := h.pricingPlanService.GetWithContent(c.Request.Context(), planID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, adminPricingPlanFromService(&detail.Plan, detail.Models, detail.Routes))
}

// Create 创建套餐（含模型协议条目与路由层）。
// POST /api/v1/admin/pricing-plans
func (h *PricingPlanHandler) Create(c *gin.Context) {
	var req UpsertPricingPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	detail, err := h.pricingPlanService.Create(
		c.Request.Context(),
		pricingPlanInputFromRequest(req),
		pricingPlanModelInputsFromRequest(req.Models),
		pricingPlanRouteInputsFromRequest(req.Routes),
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, adminPricingPlanFromService(&detail.Plan, detail.Models, detail.Routes))
}

// Update 整体替换套餐内容（含模型协议条目与路由层）。
// PUT /api/v1/admin/pricing-plans/:id
func (h *PricingPlanHandler) Update(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || planID <= 0 {
		response.BadRequest(c, "Invalid pricing plan ID")
		return
	}
	var req UpsertPricingPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	detail, err := h.pricingPlanService.Update(
		c.Request.Context(),
		planID,
		pricingPlanInputFromRequest(req),
		pricingPlanModelInputsFromRequest(req.Models),
		pricingPlanRouteInputsFromRequest(req.Routes),
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, adminPricingPlanFromService(&detail.Plan, detail.Models, detail.Routes))
}

// Delete 删除套餐（软删除，含模型协议条目与路由层）。
// DELETE /api/v1/admin/pricing-plans/:id
func (h *PricingPlanHandler) Delete(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || planID <= 0 {
		response.BadRequest(c, "Invalid pricing plan ID")
		return
	}
	if err := h.pricingPlanService.Delete(c.Request.Context(), planID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Pricing plan deleted successfully"})
}

func pricingPlanInputFromRequest(req UpsertPricingPlanRequest) service.PricingPlanInput {
	return service.PricingPlanInput{
		Name:        req.Name,
		Title:       req.Title,
		Description: req.Description,
		Status:      req.Status,
		IsPublic:    req.IsPublic,
		SortOrder:   req.SortOrder,
	}
}

func pricingPlanModelInputsFromRequest(reqs []PricingPlanModelRequest) []service.PricingPlanModelInput {
	if len(reqs) == 0 {
		return nil
	}
	out := make([]service.PricingPlanModelInput, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, service.PricingPlanModelInput{
			PublicModel:                r.PublicModel,
			Protocol:                   r.Protocol,
			UpstreamModel:              r.UpstreamModel,
			Direct:                     r.Direct,
			AllowCompatibilityFallback: r.AllowCompatibilityFallback,
			Priority:                   r.Priority,
			Enabled:                    r.Enabled,
			Notes:                      r.Notes,
			Pricing:                    service.PlanModelPricingToService(r.Pricing),
		})
	}
	return out
}

func pricingPlanRouteInputsFromRequest(reqs []PricingPlanRouteRequest) []service.PricingPlanRouteInput {
	if len(reqs) == 0 {
		return nil
	}
	out := make([]service.PricingPlanRouteInput, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, service.PricingPlanRouteInput{
			GroupID:  r.GroupID,
			Priority: r.Priority,
			Enabled:  r.Enabled,
		})
	}
	return out
}

// adminPricingPlanFromService 组装管理端套餐响应；models/routes 为 nil 时
// 输出空数组，保证 JSON 形状稳定。
func adminPricingPlanFromService(plan *service.PricingPlan, models []service.PricingPlanModel, routes []service.PricingPlanRoute) adminPricingPlan {
	out := adminPricingPlan{
		ID:          plan.ID,
		Name:        plan.Name,
		Title:       plan.Title,
		Description: plan.Description,
		Status:      plan.Status,
		IsPublic:    plan.IsPublic,
		SortOrder:   plan.SortOrder,
		CreatedAt:   plan.CreatedAt,
		UpdatedAt:   plan.UpdatedAt,
		Models:      make([]adminPricingPlanModel, 0, len(models)),
		Routes:      make([]adminPricingPlanRoute, 0, len(routes)),
	}
	for i := range models {
		m := &models[i]
		out.Models = append(out.Models, adminPricingPlanModel{
			ID:                         m.ID,
			PlanID:                     m.PlanID,
			PublicModel:                m.PublicModel,
			Protocol:                   m.Protocol,
			UpstreamModel:              m.UpstreamModel,
			Direct:                     m.Direct,
			AllowCompatibilityFallback: m.AllowCompatibilityFallback,
			Priority:                   m.Priority,
			Enabled:                    m.Enabled,
			Notes:                      m.Notes,
			Pricing:                    service.PlanModelPricingToDomain(m.Pricing),
			CreatedAt:                  m.CreatedAt,
			UpdatedAt:                  m.UpdatedAt,
		})
	}
	for i := range routes {
		r := &routes[i]
		out.Routes = append(out.Routes, adminPricingPlanRoute{
			ID:        r.ID,
			PlanID:    r.PlanID,
			GroupID:   r.GroupID,
			Priority:  r.Priority,
			Enabled:   r.Enabled,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return out
}
