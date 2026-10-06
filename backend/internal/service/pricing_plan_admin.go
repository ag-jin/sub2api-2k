package service

import (
	"context"
	"math"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ErrPricingPlanInvalidInput 套餐管理端输入校验失败（缺名称、缺模型标识、
// 路由层未指定分组等）。对外不暴露字段细节，避免内幕信息泄漏。
var ErrPricingPlanInvalidInput = infraerrors.BadRequest("PRICING_PLAN_INVALID_INPUT", "invalid pricing plan input")

// PricingPlanService 是定价套餐的管理端服务：套餐 CRUD（含模型协议条目与
// 内部路由层）。输入先规范化（复用 pricing_plan.go 的 normalize 函数），
// 再委托 PricingPlanRepository 原子持久化；读路径组装管理端全量视图。
type PricingPlanService struct {
	repo PricingPlanRepository
}

// NewPricingPlanService 创建定价套餐管理服务。
func NewPricingPlanService(repo PricingPlanRepository) *PricingPlanService {
	return &PricingPlanService{repo: repo}
}

// PricingPlanDetail 是管理端套餐全量视图：套餐 + 模型协议条目（含销售定价）
// + 内部路由层（含内部 group_id）。仅供管理端读取，绝不下发公开/用户端点。
type PricingPlanDetail struct {
	Plan   PricingPlan        `json:"plan"`
	Models []PricingPlanModel `json:"models"`
	Routes []PricingPlanRoute `json:"routes"`
}

// List 返回全部套餐；includeDisabled=false 时只返回 active 套餐。
func (s *PricingPlanService) List(ctx context.Context, includeDisabled bool) ([]PricingPlan, error) {
	if s.repo == nil {
		return nil, ErrPricingPlanUnavailable
	}
	return s.repo.ListPlans(ctx, includeDisabled)
}

// GetWithContent 返回套餐及其模型协议条目、路由层（管理端详情）。
func (s *PricingPlanService) GetWithContent(ctx context.Context, id int64) (*PricingPlanDetail, error) {
	if s.repo == nil {
		return nil, ErrPricingPlanUnavailable
	}
	plan, err := s.repo.GetPlanByID(ctx, id)
	if err != nil {
		return nil, err
	}
	models, err := s.repo.ListModelsByPlan(ctx, id, true)
	if err != nil {
		return nil, err
	}
	routes, err := s.repo.ListRoutesByPlan(ctx, id, true)
	if err != nil {
		return nil, err
	}
	return &PricingPlanDetail{
		Plan:   *plan,
		Models: models,
		Routes: routes,
	}, nil
}

// Create 规范化输入并原子创建套餐及其模型协议条目、路由层。
func (s *PricingPlanService) Create(
	ctx context.Context,
	input PricingPlanInput,
	modelInputs []PricingPlanModelInput,
	routeInputs []PricingPlanRouteInput,
) (*PricingPlanDetail, error) {
	if s.repo == nil {
		return nil, ErrPricingPlanUnavailable
	}
	plan := normalizePricingPlanInput(input)
	if plan.Name == "" {
		return nil, ErrPricingPlanInvalidInput
	}
	models, err := normalizePricingPlanModels(modelInputs)
	if err != nil {
		return nil, err
	}
	routes, err := normalizePricingPlanRoutes(routeInputs)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreatePlan(ctx, &plan, models, routes); err != nil {
		return nil, err
	}
	return s.GetWithContent(ctx, plan.ID)
}

// Update 规范化输入并整体替换套餐内容（含模型协议条目与路由层）。
func (s *PricingPlanService) Update(
	ctx context.Context,
	id int64,
	input PricingPlanInput,
	modelInputs []PricingPlanModelInput,
	routeInputs []PricingPlanRouteInput,
) (*PricingPlanDetail, error) {
	if s.repo == nil {
		return nil, ErrPricingPlanUnavailable
	}
	plan := normalizePricingPlanInput(input)
	if plan.Name == "" {
		return nil, ErrPricingPlanInvalidInput
	}
	plan.ID = id
	models, err := normalizePricingPlanModels(modelInputs)
	if err != nil {
		return nil, err
	}
	routes, err := normalizePricingPlanRoutes(routeInputs)
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpdatePlan(ctx, &plan, models, routes); err != nil {
		return nil, err
	}
	return s.GetWithContent(ctx, id)
}

// Delete 软删除套餐及其模型协议条目、路由层。
func (s *PricingPlanService) Delete(ctx context.Context, id int64) error {
	if s.repo == nil {
		return ErrPricingPlanUnavailable
	}
	return s.repo.DeletePlan(ctx, id)
}

func normalizePricingPlanModels(inputs []PricingPlanModelInput) ([]PricingPlanModel, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(inputs))
	out := make([]PricingPlanModel, 0, len(inputs))
	for _, in := range inputs {
		m := normalizePricingPlanModelInput(in)
		if m.PublicModel == "" || !isPricingPlanProtocol(m.Protocol) {
			return nil, ErrPricingPlanInvalidInput
		}
		if m.AllowCompatibilityFallback &&
			(m.Direct || m.Protocol != PricingPlanProtocolChatCompletions) {
			return nil, ErrPricingPlanInvalidInput
		}
		if err := validatePricingPlanPricing(m.Pricing); err != nil {
			return nil, err
		}
		identity := strings.ToLower(m.PublicModel) + "\x00" + m.Protocol
		if _, exists := seen[identity]; exists {
			return nil, ErrPricingPlanInvalidInput
		}
		seen[identity] = struct{}{}
		out = append(out, m)
	}
	return out, nil
}

func normalizePricingPlanRoutes(inputs []PricingPlanRouteInput) ([]PricingPlanRoute, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	seenGroups := make(map[int64]struct{}, len(inputs))
	seenPriorities := make(map[int]struct{}, len(inputs))
	out := make([]PricingPlanRoute, 0, len(inputs))
	for _, in := range inputs {
		r := normalizePricingPlanRouteInput(in)
		if r.GroupID <= 0 {
			return nil, ErrPricingPlanInvalidInput
		}
		if _, exists := seenGroups[r.GroupID]; exists {
			return nil, ErrPricingPlanInvalidInput
		}
		if _, exists := seenPriorities[r.Priority]; exists {
			return nil, ErrPricingPlanInvalidInput
		}
		seenGroups[r.GroupID] = struct{}{}
		seenPriorities[r.Priority] = struct{}{}
		out = append(out, r)
	}
	return out, nil
}

func validatePricingPlanPricing(pricing *ChannelModelPricing) error {
	if pricing == nil {
		return nil
	}
	if pricing.BillingMode == BillingModePerRequest ||
		pricing.BillingMode == BillingModeImage ||
		pricing.BillingMode == BillingModeVideo {
		if pricing.PerRequestPrice == nil && len(pricing.Intervals) == 0 {
			return ErrPricingPlanInvalidInput
		}
	}
	for _, value := range []*float64{
		pricing.InputPrice,
		pricing.OutputPrice,
		pricing.CacheWritePrice,
		pricing.CacheReadPrice,
		pricing.ImageInputPrice,
		pricing.ImageOutputPrice,
		pricing.PerRequestPrice,
		pricing.FastMultiplier,
		pricing.FlexMultiplier,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
			return ErrPricingPlanInvalidInput
		}
	}
	for _, interval := range pricing.Intervals {
		if interval.MinTokens < 0 ||
			(interval.MaxTokens != nil && *interval.MaxTokens <= interval.MinTokens) {
			return ErrPricingPlanInvalidInput
		}
		for _, value := range []*float64{
			interval.InputPrice,
			interval.OutputPrice,
			interval.CacheWritePrice,
			interval.CacheReadPrice,
			interval.PerRequestPrice,
			interval.InputMultiplier,
			interval.OutputMultiplier,
			interval.CacheWriteMultiplier,
			interval.CacheReadMultiplier,
		} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
				return ErrPricingPlanInvalidInput
			}
		}
	}
	if err := ValidateIntervals(pricing.Intervals, pricing.BillingMode); err != nil {
		return ErrPricingPlanInvalidInput
	}
	if pricing.TimePricing != nil {
		if pricing.BillingMode != BillingModeToken || validateChannelTimePricing(pricing.TimePricing) != nil {
			return ErrPricingPlanInvalidInput
		}
	}
	return nil
}

// PlanModelPricingToDomain 把 service.ChannelModelPricing 映射为持久化的
// 定价文档（domain.PlanModelPricing）：JSONB 只存定价字段，不含渠道/实体标识。
// 管理端读写与仓储共用此映射，保证销售定价配置与公开售价同一套语义。
func PlanModelPricingToDomain(p *ChannelModelPricing) *domain.PlanModelPricing {
	if p == nil {
		return nil
	}
	out := &domain.PlanModelPricing{
		BillingMode:      string(p.BillingMode),
		InputPrice:       p.InputPrice,
		OutputPrice:      p.OutputPrice,
		CacheWritePrice:  p.CacheWritePrice,
		CacheReadPrice:   p.CacheReadPrice,
		FastMultiplier:   p.FastMultiplier,
		FlexMultiplier:   p.FlexMultiplier,
		ImageInputPrice:  p.ImageInputPrice,
		ImageOutputPrice: p.ImageOutputPrice,
		PerRequestPrice:  p.PerRequestPrice,
		TimePricing:      planTimePricingToDomain(p.TimePricing),
	}
	for i := range p.Intervals {
		in := &p.Intervals[i]
		out.Intervals = append(out.Intervals, domain.PlanModelPricingInterval{
			MinTokens:            in.MinTokens,
			MaxTokens:            in.MaxTokens,
			TierLabel:            in.TierLabel,
			InputPrice:           in.InputPrice,
			OutputPrice:          in.OutputPrice,
			CacheWritePrice:      in.CacheWritePrice,
			CacheReadPrice:       in.CacheReadPrice,
			InputMultiplier:      in.InputMultiplier,
			OutputMultiplier:     in.OutputMultiplier,
			CacheWriteMultiplier: in.CacheWriteMultiplier,
			CacheReadMultiplier:  in.CacheReadMultiplier,
			PerRequestPrice:      in.PerRequestPrice,
			SortOrder:            in.SortOrder,
		})
	}
	return out
}

// PlanModelPricingToService 把持久化的定价文档映射回 service.ChannelModelPricing，
// 供读路径按既有定价语义解析（billing mode、区间、分时倍率等）。
func PlanModelPricingToService(p *domain.PlanModelPricing) *ChannelModelPricing {
	if p == nil {
		return nil
	}
	out := &ChannelModelPricing{
		BillingMode:      BillingMode(p.BillingMode),
		InputPrice:       p.InputPrice,
		OutputPrice:      p.OutputPrice,
		CacheWritePrice:  p.CacheWritePrice,
		CacheReadPrice:   p.CacheReadPrice,
		FastMultiplier:   p.FastMultiplier,
		FlexMultiplier:   p.FlexMultiplier,
		ImageInputPrice:  p.ImageInputPrice,
		ImageOutputPrice: p.ImageOutputPrice,
		PerRequestPrice:  p.PerRequestPrice,
		TimePricing:      planTimePricingToService(p.TimePricing),
	}
	for i := range p.Intervals {
		in := &p.Intervals[i]
		out.Intervals = append(out.Intervals, PricingInterval{
			MinTokens:            in.MinTokens,
			MaxTokens:            in.MaxTokens,
			TierLabel:            in.TierLabel,
			InputPrice:           in.InputPrice,
			OutputPrice:          in.OutputPrice,
			CacheWritePrice:      in.CacheWritePrice,
			CacheReadPrice:       in.CacheReadPrice,
			InputMultiplier:      in.InputMultiplier,
			OutputMultiplier:     in.OutputMultiplier,
			CacheWriteMultiplier: in.CacheWriteMultiplier,
			CacheReadMultiplier:  in.CacheReadMultiplier,
			PerRequestPrice:      in.PerRequestPrice,
			SortOrder:            in.SortOrder,
		})
	}
	return out
}

func planTimePricingToDomain(p *ChannelTimePricing) *domain.PlanModelTimePricing {
	if p == nil {
		return nil
	}
	out := &domain.PlanModelTimePricing{Timezone: p.Timezone}
	for i := range p.Periods {
		in := &p.Periods[i]
		out.Periods = append(out.Periods, domain.PlanModelTimePricingPeriod{
			StartTime:  in.StartTime,
			EndTime:    in.EndTime,
			Multiplier: in.Multiplier,
		})
	}
	return out
}

func planTimePricingToService(p *domain.PlanModelTimePricing) *ChannelTimePricing {
	if p == nil {
		return nil
	}
	out := &ChannelTimePricing{Timezone: p.Timezone}
	for i := range p.Periods {
		in := &p.Periods[i]
		out.Periods = append(out.Periods, ChannelTimePricingPeriod{
			StartTime:  in.StartTime,
			EndTime:    in.EndTime,
			Multiplier: in.Multiplier,
		})
	}
	return out
}
