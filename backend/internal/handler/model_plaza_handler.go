package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ModelPlazaHandler 处理「模型广场」查询（公开产品目录）。
//
// 数据源为定价套餐的公开产品（PricingPlanRepository.ListPublicProducts）：
// 只下发公开售价与展示字段，任何内部数据（分组、group_id、路由层、账号、
// 上游成本、倍率、健康状态、平台/基址）都不进入响应。广场路由挂
// OptionalJWT 中间件：匿名可访问（除非 require_auth 开启），带 token 仅用于
// 识别用户；原分组/专属可见性语义已整体移除。
type ModelPlazaHandler struct {
	pricingPlanRepo service.PricingPlanRepository
	settingService  *service.SettingService
}

// NewModelPlazaHandler 创建模型广场 handler。
func NewModelPlazaHandler(
	pricingPlanRepo service.PricingPlanRepository,
	settingService *service.SettingService,
) *ModelPlazaHandler {
	return &ModelPlazaHandler{
		pricingPlanRepo: pricingPlanRepo,
		settingService:  settingService,
	}
}

// modelPlazaPricingInterval 目录定价区间白名单（去掉内部 ID、倍率、SortOrder 等）。
type modelPlazaPricingInterval struct {
	MinTokens       int      `json:"min_tokens"`
	MaxTokens       *int     `json:"max_tokens"`
	TierLabel       string   `json:"tier_label,omitempty"`
	InputPrice      *float64 `json:"input_price"`
	OutputPrice     *float64 `json:"output_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	PerRequestPrice *float64 `json:"per_request_price"`
}

// modelPlazaPricing 目录定价白名单（USD；token 计费为每 token 单价，
// 按次/按图为每单位单价），等价于公开售价，不携带渠道/内部字段。
type modelPlazaPricing struct {
	InputPrice       *float64                    `json:"input_price"`
	OutputPrice      *float64                    `json:"output_price"`
	CacheWritePrice  *float64                    `json:"cache_write_price"`
	CacheReadPrice   *float64                    `json:"cache_read_price"`
	ImageInputPrice  *float64                    `json:"image_input_price"`
	ImageOutputPrice *float64                    `json:"image_output_price"`
	PerRequestPrice  *float64                    `json:"per_request_price"`
	Intervals        []modelPlazaPricingInterval `json:"intervals"`
}

// modelPlazaProtocol 单个协议计价行：协议 + 是否直连 + 计费模式 + 公开定价。
type modelPlazaProtocol struct {
	Protocol    string             `json:"protocol"`
	Direct      bool               `json:"direct"`
	BillingMode string             `json:"billing_mode"`
	Pricing     *modelPlazaPricing `json:"pricing"`
}

// modelPlazaModel 目录模型条目：模型标识 + 展示名 + 各协议计价行。
type modelPlazaModel struct {
	ID          string               `json:"id"`
	DisplayName string               `json:"display_name"`
	Protocols   []modelPlazaProtocol `json:"protocols"`
}

// modelPlazaPlan 目录套餐（白名单字段）。
type modelPlazaPlan struct {
	Code        string            `json:"code"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Models      []modelPlazaModel `json:"models"`
}

// modelPlazaResponse 广场页响应。
type modelPlazaResponse struct {
	Description string           `json:"description"`
	Plans       []modelPlazaPlan `json:"plans"`
}

// Get 返回模型广场数据（公开产品目录）。
// GET /api/v1/model-plaza
func (h *ModelPlazaHandler) Get(c *gin.Context) {
	if h.settingService == nil {
		response.NotFound(c, "Model plaza is not enabled")
		return
	}
	rt := h.settingService.GetModelPlazaRuntime(c.Request.Context())
	if !rt.Enabled {
		response.NotFound(c, "Model plaza is not enabled")
		return
	}

	_, authed := middleware.GetAuthSubjectFromContext(c)
	if rt.RequireAuth && !authed {
		response.Unauthorized(c, "Authentication required")
		return
	}

	if h.pricingPlanRepo == nil {
		// fail-closed：仓储缺失视为功能未启用，不返回半成品目录。
		response.NotFound(c, "Model plaza is not enabled")
		return
	}
	products, err := h.pricingPlanRepo.ListPublicProducts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	plans := make([]modelPlazaPlan, 0, len(products))
	for _, p := range products {
		plans = append(plans, toModelPlazaPlan(p))
	}
	response.Success(c, modelPlazaResponse{
		Description: rt.Description,
		Plans:       plans,
	})
}

// toModelPlazaPlan 将公开产品映射为目录套餐：code=套餐稳定代号（Name），
// name=展示名（Title），description=套餐描述；模型按公开模型分组。
func toModelPlazaPlan(p service.PricingPlanProduct) modelPlazaPlan {
	return modelPlazaPlan{
		Code:        p.Plan.Name,
		Name:        p.Plan.Title,
		Description: p.Plan.Description,
		Models:      groupModelPlazaProtocols(p.Models),
	}
}

// groupModelPlazaProtocols 把套餐的「模型 -> 协议」条目按公开模型分组为目录
// 模型条目：同模型的多协议行合并到 protocols（保持仓库返回顺序，即
// priority 升序）；display_name 暂以 public_model 兜底展示。
func groupModelPlazaProtocols(models []service.PricingPlanModel) []modelPlazaModel {
	out := make([]modelPlazaModel, 0)
	idx := make(map[string]int, len(models))
	for i := range models {
		m := &models[i]
		if !m.Enabled {
			continue
		}
		at, seen := idx[m.PublicModel]
		if !seen {
			at = len(out)
			idx[m.PublicModel] = at
			out = append(out, modelPlazaModel{
				ID:          m.PublicModel,
				DisplayName: m.PublicModel,
			})
		}
		out[at].Protocols = append(out[at].Protocols, toModelPlazaProtocol(m))
	}
	return out
}

// toModelPlazaProtocol 将套餐「模型 -> 协议」条目映射为协议计价行；
// 计费模式取定价文档的 BillingMode，未配置时按 token 计费展示。
func toModelPlazaProtocol(m *service.PricingPlanModel) modelPlazaProtocol {
	billingMode := string(service.BillingModeToken)
	if m.Pricing != nil && m.Pricing.BillingMode != "" {
		billingMode = string(m.Pricing.BillingMode)
	}
	return modelPlazaProtocol{
		Protocol:    m.Protocol,
		Direct:      m.Direct,
		BillingMode: billingMode,
		Pricing:     toModelPlazaPricing(m.Pricing),
	}
}

// toModelPlazaPricing 将定价文档映射为公开售价白名单；nil 透传（前端显示空价）。
func toModelPlazaPricing(p *service.ChannelModelPricing) *modelPlazaPricing {
	if p == nil {
		return nil
	}
	intervals := make([]modelPlazaPricingInterval, 0, len(p.Intervals))
	for _, iv := range p.Intervals {
		intervals = append(intervals, modelPlazaPricingInterval{
			MinTokens:       iv.MinTokens,
			MaxTokens:       iv.MaxTokens,
			TierLabel:       iv.TierLabel,
			InputPrice:      iv.InputPrice,
			OutputPrice:     iv.OutputPrice,
			CacheWritePrice: iv.CacheWritePrice,
			CacheReadPrice:  iv.CacheReadPrice,
			PerRequestPrice: iv.PerRequestPrice,
		})
	}
	return &modelPlazaPricing{
		InputPrice:       p.InputPrice,
		OutputPrice:      p.OutputPrice,
		CacheWritePrice:  p.CacheWritePrice,
		CacheReadPrice:   p.CacheReadPrice,
		ImageInputPrice:  p.ImageInputPrice,
		ImageOutputPrice: p.ImageOutputPrice,
		PerRequestPrice:  p.PerRequestPrice,
		Intervals:        intervals,
	}
}
