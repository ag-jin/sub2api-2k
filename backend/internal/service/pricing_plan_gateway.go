package service

import (
	"strings"
)

// 本文件提供定价套餐在网关口侧的纯函数辅助：入站协议归一化、模型×协议
// 准入条目查找、有序路由层展开与账号级原生协议筛选。全部为无副作用函数，
// 便于单元测试；热路径数据来自认证快照（APIKeyAuthPricingPlanSnapshot），
// 不回源数据库。

// NormalizeInboundPricingPlanProtocol 从请求路径归一化入站 API 协议：
//
//	/v1/messages（含 /openai/v1/messages） → messages
//	/v1/responses、/openai/v1/responses、/responses、/backend-api/codex/responses
//	   及全部子路径                       → responses
//	/v1/chat/completions（含 /openai 前缀） → chat_completions
//
// 未知路径返回空串。套餐条目协议与入站协议不一致时按不可用处理
// （handler 走自家协议错误路径），因此归一化必须与路由表一致。
func NormalizeInboundPricingPlanProtocol(path string) string {
	segs := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(path)), func(r rune) bool {
		return r == '/'
	})
	for i := 0; i < len(segs); i++ {
		switch segs[i] {
		case "messages":
			if i > 0 && (segs[i-1] == "v1" || segs[i-1] == "openai") {
				return PricingPlanProtocolMessages
			}
		case "responses":
			// 裸 /responses 与 /backend-api/codex/responses 等别名也属于
			// Responses 协议；chat/completions 后面的 "completions" 段不会
			// 误命中（"responses" 与 "completions" 是不同段）。
			return PricingPlanProtocolResponses
		case "chat":
			if i+1 < len(segs) && segs[i+1] == "completions" {
				return PricingPlanProtocolChatCompletions
			}
		}
	}
	return ""
}

// FindAPIKeyPricingPlanOffer 返回 apiKey 套餐内 model×protocol 的启用条目
// （快照仅收录启用条目）。未绑定套餐或未命中返回 nil —— 调用方据此走
// legacy 行为或"模型不在套餐内"拒绝路径。protocol 传归一化后的入站协议。
func FindAPIKeyPricingPlanOffer(apiKey *APIKey, model, protocol string) *APIKeyAuthPricingPlanModelSnapshot {
	if apiKey == nil || apiKey.PricingPlanID == nil || apiKey.PricingPlanSnapshot == nil {
		return nil
	}
	for i := range apiKey.PricingPlanSnapshot.Models {
		if m := &apiKey.PricingPlanSnapshot.Models[i]; m.PublicModel == model && m.Protocol == protocol {
			return m
		}
	}
	return nil
}

// PricingPlanLayerGroupIDs 展开套餐的有序启用路由层：按 priority 升序
// （快照构建时已排序），去重并跳过禁用/无效（GroupID<=0）层。返回的
// 内部 group ID 仅在本进程内使用，绝不进入公开 API。
func PricingPlanLayerGroupIDs(plan *APIKeyAuthPricingPlanSnapshot) []int64 {
	if plan == nil {
		return nil
	}
	var out []int64
	seen := make(map[int64]struct{}, len(plan.Routes))
	for _, route := range plan.Routes {
		if !route.Enabled || route.GroupID <= 0 {
			continue
		}
		if _, dup := seen[route.GroupID]; dup {
			continue
		}
		seen[route.GroupID] = struct{}{}
		out = append(out, route.GroupID)
	}
	return out
}

// AccountMatchesPricingPlanProtocol 校验账号的 api_protocol 是否满足套餐
// 模型协议的原生出站筛选（native protocol filtering）。allowCompatibilityFallback
// 是套餐条目声明的跨协议兼容兜底开关：
//
//   - messages：放行原生 anthropic 平台账号（本身就是 /v1/messages 出站池）、
//     api_protocol=anthropic 的账号与 adaptive CN 账号（按入站协议动态选端点）；
//     chat 协议账号仅在该条目允许兼容兜底时放行（网关负责跨协议转换）。
//   - responses：放行供应商原生支持 responses 的 openai/grok 平台账号、
//     adaptive 账号与 api_protocol=responses 的 deepseek 账号。
//   - chat_completions：优先 chat/adaptive 账号；anthropic 协议账号仅在该
//     条目允许兼容兜底时放行（网关负责跨协议转换）。
//
// 协议族账号（generic upstream/其余平台）按 chat 语义放行，不破坏既有
// 通用上游账号。
func AccountMatchesPricingPlanProtocol(account *Account, protocol string, allowCompatibilityFallback bool) bool {
	if account == nil {
		return false
	}
	switch protocol {
	case PricingPlanProtocolMessages:
		if account.Platform == PlatformAnthropic {
			return true
		}
		if account.IsAdaptiveAPIProtocol() {
			return true
		}
		switch account.GetAPIProtocol() {
		case APIProtocolAnthropic:
			return true
		case APIProtocolChatCompletions, APIProtocolResponses:
			return allowCompatibilityFallback
		default:
			return false
		}
	case PricingPlanProtocolResponses:
		if account.IsOpenAI() || account.IsGrok() {
			return true
		}
		if account.IsAdaptiveAPIProtocol() {
			return true
		}
		return account.GetAPIProtocol() == APIProtocolResponses
	case PricingPlanProtocolChatCompletions:
		// 原生 anthropic 平台账号或 api_protocol=anthropic 的账号走
		// chat 出站必须跨协议转换，仅在该条目允许兼容兜底时放行。
		if account.Platform == PlatformAnthropic {
			return allowCompatibilityFallback
		}
		switch account.GetAPIProtocol() {
		case APIProtocolAnthropic:
			return allowCompatibilityFallback
		default:
			// chat/adaptive/responses（deepseek）/通用上游账号：chat 出站原生或
			// 供应商可经既有转发路径服务，放行。
			return true
		}
	default:
		return true
	}
}