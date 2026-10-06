package admin

import (
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 票 34：智谱建号收口为「仅登录授权」。
//
// 通用建号接口（POST /api/v1/admin/accounts）不再接受手动 API Key 形态的智谱凭据：
// 智谱账号只有一个合法来源——登录授权产物（credentials 带 auth_flow=bigmodel_oauth，
// 由专用端点 /api/v1/admin/zhipu/oauth/create-from-login 落库）。手动 api_key、缺 key、
// 空凭据等一切非托管形态一律 4xx，并在 message 里指回登录建号端点。
//
// 刻意只管建号：编辑接口（PUT /api/v1/admin/accounts/:id）不校验，存量手动智谱账号
// （如生产 KDY 渠道账号）必须继续可编辑、可运行。
const (
	// zhipuLoginOnlyCreateReason 是拒绝时的稳定错误码（前端可据此分支）。
	zhipuLoginOnlyCreateReason = "ZHIPU_LOGIN_ONLY"
	// zhipuLoginOnlyCreateMessage 指向登录授权建号端点。
	zhipuLoginOnlyCreateMessage = "zhipu accounts can only be created through login authorization " +
		"(POST /api/v1/admin/zhipu/oauth/create-from-login); manual API key creation is disabled"
	// zhipuCredentialAuthFlowKey 是登录托管标记的凭据键，与 service.Account.IsZhipuLoginManaged
	// 的判定同源（service 侧的同名常量为包内私有，此处按 handler 包的既有做法声明字面量）。
	zhipuCredentialAuthFlowKey = "auth_flow"
)

// zhipuLoginOnlyCreateError 判定一次智谱建号请求是否为「非登录授权形态」。
// 返回 nil 表示放行；非 nil 是直接可写的 4xx。
func zhipuLoginOnlyCreateError(platform string, credentials map[string]any) error {
	if platform != service.PlatformZhipu {
		return nil
	}
	if raw, ok := credentials[zhipuCredentialAuthFlowKey].(string); ok && raw == service.ZhipuLoginAuthFlow {
		return nil
	}
	return infraerrors.BadRequest(zhipuLoginOnlyCreateReason, zhipuLoginOnlyCreateMessage)
}
