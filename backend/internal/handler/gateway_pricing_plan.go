package handler

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 本文件承载 API Key 定价套餐在网关 handler 侧的共享逻辑：模型×协议准入
// （resolvePricingPlanGatewayState）、有序内部路由层的展开与逐层绑定
// （bindNextLayer/advanceLayer）以及账号级原生协议筛选。六个入口
// （GatewayHandler 的 Messages/ChatCompletions/Responses 与 OpenAIGatewayHandler
// 的三件套）复用同一状态机，避免逐 handler 复制业务规则。

// planGroupResolver 把内部 group ID 解析为可计费/可调度的 Group 对象。
type planGroupResolver func(ctx context.Context, groupID int64) (*service.Group, error)

// pricingPlanNoLayersMessage 套餐没有可用路由层时各端点统一回给客户端的文案。
const pricingPlanNoLayersMessage = "No available accounts: this pricing plan has no active route layers"

// pricingPlanGatewayState 是套餐 Key 请求的网关准入状态（模型解析后构建）。
// active=false 表示 legacy 无套餐路径，行为与现状完全一致。
type pricingPlanGatewayState struct {
	active  bool
	offer   *service.APIKeyAuthPricingPlanModelSnapshot
	layers  []int64 // 有序启用路由层（内部 group ID，priority 升序）
	current int     // 当前层下标；-1 = 尚未绑定
}

// resolvePricingPlanGatewayState 在解析 model 之后调用：
//   - 未绑定套餐 → active=false，无需拒绝（legacy 行为不变）。
//   - 绑定套餐且快照缺失 → 拒绝（套餐不可用，通用授权语义）。
//   - 无匹配的启用 model×protocol 条目 → 拒绝（返回三元组，
//     handler 用自家协议错误路径渲染，且不进入选号）。
//   - 通过后返回分层状态；层解析失败等不在此报错（由绑定阶段跳过该层）。
func resolvePricingPlanGatewayState(apiKey *service.APIKey, model, inboundProtocol string) (pricingPlanGatewayState, int, string, string) {
	if apiKey == nil || apiKey.PricingPlanID == nil {
		return pricingPlanGatewayState{}, 0, "", ""
	}
	if apiKey.PricingPlanSnapshot == nil || apiKey.PricingPlanSnapshot.PlanID <= 0 {
		return pricingPlanGatewayState{}, http.StatusForbidden, "permission_error",
			"The pricing plan for this API key is currently unavailable"
	}
	offer := service.FindAPIKeyPricingPlanOffer(apiKey, model, inboundProtocol)
	if offer == nil {
		return pricingPlanGatewayState{}, http.StatusForbidden, "permission_error",
			"This model is not included in your pricing plan for this endpoint"
	}
	apiKey.ActivePricingPlanOffer = offer
	return pricingPlanGatewayState{
		active:  true,
		offer:   offer,
		layers:  service.PricingPlanLayerGroupIDs(apiKey.PricingPlanSnapshot),
		current: -1,
	}, 0, "", ""
}

// hasNextLayer 报告是否还有后续层可绑定。
func (s *pricingPlanGatewayState) hasNextLayer() bool {
	return s != nil && s.active && s.current+1 < len(s.layers)
}

// bindNextLayer 把下一层绑定到 apiKey 副本（GroupID/Group，供下游计费与
// 选号直接依赖）与请求 ctx（ctxkey.Group，供利润门等读取），并把该层分组
// 标记为当前层。层内分组解析失败/停用时跳过该层继续；返回 false 表示层已
// 耗尽。内部 group 数据只进请求上下文与 apiKey 副本，不进入公开 DTO。
func (s *pricingPlanGatewayState) bindNextLayer(c *gin.Context, apiKey *service.APIKey, resolve planGroupResolver) bool {
	if !s.hasNextLayer() {
		return false
	}
	s.current++
	groupID := s.layers[s.current]
	if resolve == nil || apiKey == nil {
		return false
	}
	group, err := resolve(c.Request.Context(), groupID)
	if err != nil || group == nil || !group.IsActive() {
		// 层分组缺失/停用：跳过该层，不暴露内部数据。
		return s.bindNextLayer(c, apiKey, resolve)
	}
	gid := groupID
	apiKey.GroupID = &gid
	apiKey.Group = group
	if c != nil && c.Request != nil {
		ctx := context.WithValue(c.Request.Context(), ctxkey.Group, group)
		c.Request = c.Request.WithContext(ctx)
	}
	return true
}

// advanceLayer 层内无可选账号（或 failover 耗尽且未提交响应）时推进到下一层。
// 仅当流尚未开始且未写出任何响应字节（未到 response commit）时允许跨层重试；
// 一旦响应已提交，保持既有行为（按层耗尽/上游失败处理），不跨层。
func (s *pricingPlanGatewayState) advanceLayer(c *gin.Context, apiKey *service.APIKey, resolve planGroupResolver, streamStarted bool) bool {
	if s == nil || !s.active {
		return false
	}
	if streamStarted || (c != nil && c.Writer != nil && c.Writer.Size() > 0) {
		return false
	}
	return s.bindNextLayer(c, apiKey, resolve)
}

// accountPlanProtocolAllowed 层内选号后的原生协议终检：账号协议不满足套餐
// 模型协议的出站筛选时返回 false，调用方排除该账号并在本层内重新选号。
func (s *pricingPlanGatewayState) accountPlanProtocolAllowed(account *service.Account) bool {
	if s == nil || !s.active || account == nil {
		return true // legacy 路径恒放行
	}
	// direct 产品条目必须维持原生协议。兼容转换只允许明确标为非直连，且显式
	// 开启 compatibility fallback 的 Chat 产品。
	allowCompatibilityFallback := !s.offer.Direct && s.offer.AllowCompatibilityFallback
	return service.AccountMatchesPricingPlanProtocol(account, s.offer.Protocol, allowCompatibilityFallback)
}
