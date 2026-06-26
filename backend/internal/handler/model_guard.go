package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// modelGuardErrorMessage 是模型不在分组输出清单时的统一提示文案。
const modelGuardErrorMessage = "model is not available in the bound group's allowed list"

// apiKeyModelAllowed 判断该 API key 是否允许请求 reqModel（对外名）。
//
// L1（单绑 key）语义：只看 key 绑定的 group——若该 group 启用了
// EnforceModelsList（清单即白名单），则 reqModel 必须命中清单，否则拒绝。
// 未启用强制清单、或 group 为空时一律放行（保持存量行为不变）。
//
// 校验针对“对外名”（用户实际请求的模型名），在模型别名映射 *之前* 执行：
// 清单是对外契约，映射是对内实现，先校验对外名才不会被映射规则绕过。
//
// 返回 true = 放行；false = 应拒绝（调用方按各自协议渲染 400 错误）。
func apiKeyModelAllowed(apiKey *service.APIKey, reqModel string) bool {
	if apiKey == nil || apiKey.Group == nil {
		return true
	}
	g := apiKey.Group
	if !g.EnforceModelsListActive() {
		return true
	}
	return g.AllowsOutputModel(reqModel)
}
