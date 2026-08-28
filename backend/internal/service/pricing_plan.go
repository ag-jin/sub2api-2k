package service

import (
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Pricing plan status constants（与 domain.StatusActive/StatusDisabled 同值）。
const (
	PricingPlanStatusActive   = "active"
	PricingPlanStatusDisabled = "disabled"
)

// Pricing plan model protocol constants：套餐内每个公开模型走的上游 API 协议。
const (
	PricingPlanProtocolChatCompletions = "chat_completions"
	PricingPlanProtocolMessages        = "messages"
	PricingPlanProtocolResponses       = "responses"
)

var (
	ErrPricingPlanNotFound = infraerrors.NotFound("PRICING_PLAN_NOT_FOUND", "pricing plan not found")
	ErrPricingPlanExists   = infraerrors.Conflict("PRICING_PLAN_EXISTS", "pricing plan already exists")
	// ErrPricingPlanUnavailable 套餐缺失/停用/仓库未注入时的通用授权错误。
	// 对外文案与 legacy group 不可用保持一致（"group or pricing plan unavailable"），
	// 避免暴露套餐内部结构或分组数据。
	ErrPricingPlanUnavailable = infraerrors.Forbidden("GROUP_OR_PLAN_UNAVAILABLE", "the group or pricing plan is currently unavailable")
)

// PricingPlan 是定价套餐的领域模型：管理端维护套餐数据与公开产品标记，
// 认证/产品浏览端读取（见 PricingPlanProduct）。
type PricingPlan struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"`
	IsPublic    bool      `json:"is_public"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PricingPlanModel 是套餐内的「模型 -> 协议」条目：声明公开模型在该套餐下
// 走哪种上游 API 协议、是否直连/允许兼容回退，以及定价文档，供认证热路径
// 按模型解析协议。一个公开模型可对应多个协议条目（唯一键含 protocol）。
type PricingPlanModel struct {
	ID                         int64                `json:"id"`
	PlanID                     int64                `json:"plan_id"`
	PublicModel                string               `json:"public_model"`
	Protocol                   string               `json:"protocol"`
	UpstreamModel              string               `json:"upstream_model"`
	Direct                     bool                 `json:"direct"`
	AllowCompatibilityFallback bool                 `json:"allow_compatibility_fallback"`
	Priority                   int                  `json:"priority"`
	Enabled                    bool                 `json:"enabled"`
	Notes                      string               `json:"notes"`
	Pricing                    *ChannelModelPricing `json:"pricing,omitempty"`
	CreatedAt                  time.Time            `json:"created_at"`
	UpdatedAt                  time.Time            `json:"updated_at"`
}

// PricingPlanRoute 是套餐内的内部池层：每层绑定一个分组（group_id）作为出站
// 池，按 priority 升序裁决（低值优先）。同一套餐内 (plan_id, group_id) 与
// (plan_id, priority) 在未删除行上各自唯一（部分唯一索引见迁移）。
type PricingPlanRoute struct {
	ID        int64     `json:"id"`
	PlanID    int64     `json:"plan_id"`
	GroupID   int64     `json:"group_id"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PricingPlanProduct 是公开产品：套餐及其启用的模型协议条目，
// 供产品列表/购买页（auth 读取）使用。
type PricingPlanProduct struct {
	Plan   PricingPlan        `json:"plan"`
	Models []PricingPlanModel `json:"models"`
}

// PricingPlanInput 是管理端创建/更新套餐的输入（行为 entity 规范化）。
type PricingPlanInput struct {
	Name        string
	Title       string
	Description string
	Status      string
	IsPublic    bool
	SortOrder   int
}

// PricingPlanModelInput 是管理端维护模型协议条目的输入。
type PricingPlanModelInput struct {
	PublicModel                string
	Protocol                   string
	UpstreamModel              string
	Direct                     bool
	AllowCompatibilityFallback bool
	Priority                   int
	Enabled                    bool
	Notes                      string
	Pricing                    *ChannelModelPricing
}

// PricingPlanRouteInput 是管理端维护内部池层条目的输入。
type PricingPlanRouteInput struct {
	GroupID  int64
	Priority int
	Enabled  bool
}

func normalizePricingPlanStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == PricingPlanStatusDisabled {
		return PricingPlanStatusDisabled
	}
	return PricingPlanStatusActive
}

func normalizePricingPlanProtocol(protocol string) string {
	return strings.ToLower(strings.TrimSpace(protocol))
}

func isPricingPlanProtocol(protocol string) bool {
	switch protocol {
	case PricingPlanProtocolChatCompletions, PricingPlanProtocolMessages, PricingPlanProtocolResponses:
		return true
	default:
		return false
	}
}

// normalizePricingPlanInput 规范化套餐输入，返回用于持久化的字段。
func normalizePricingPlanInput(input PricingPlanInput) PricingPlan {
	return PricingPlan{
		Name:        strings.TrimSpace(input.Name),
		Title:       strings.TrimSpace(input.Title),
		Description: strings.TrimSpace(input.Description),
		Status:      normalizePricingPlanStatus(input.Status),
		IsPublic:    input.IsPublic,
		SortOrder:   input.SortOrder,
	}
}

// normalizePricingPlanModelInput 规范化模型协议条目输入。
// upstream_model 为空时回填为 public_model：条目按具体模型精确匹配，
// 持久化层面保持与 composite 路由相同的契约。
func normalizePricingPlanModelInput(input PricingPlanModelInput) PricingPlanModel {
	model := PricingPlanModel{
		PublicModel:                strings.TrimSpace(input.PublicModel),
		Protocol:                   normalizePricingPlanProtocol(input.Protocol),
		UpstreamModel:              strings.TrimSpace(input.UpstreamModel),
		Direct:                     input.Direct,
		AllowCompatibilityFallback: input.AllowCompatibilityFallback,
		Priority:                   input.Priority,
		Enabled:                    input.Enabled,
		Notes:                      strings.TrimSpace(input.Notes),
		Pricing:                    input.Pricing,
	}
	if model.UpstreamModel == "" {
		model.UpstreamModel = model.PublicModel
	}
	return model
}

// normalizePricingPlanRouteInput 规范化内部池层输入：只含数值/布尔字段，
// 无需字符串归一化。
func normalizePricingPlanRouteInput(input PricingPlanRouteInput) PricingPlanRoute {
	return PricingPlanRoute{
		GroupID:  input.GroupID,
		Priority: input.Priority,
		Enabled:  input.Enabled,
	}
}
