package admin

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ZhipuSignHandler 暴露智谱签名 V4 的管理端配置与状态端点（design M3.1(d)(e) / 票 28）：
//
//	GET /api/v1/admin/zhipu/sign/config   读取生效配置（含被运行期覆盖的键）
//	PUT /api/v1/admin/zhipu/sign/config   强校验后写入修改（热更新，变更由审计中间件留痕）
//	GET /api/v1/admin/zhipu/sign/status   只读状态（私钥缓存 / 上次握手 / 连续失败 / 账号级熔断）
//
// 鉴权不是本文件的职责：三条路由一律挂在 routes/admin.go 的 admin 分组下，继承既有
// 管理端中间件（adminAuth + 面板限流 + 审计 + 合规守卫）。本文件与登记的路由都不提供
// 任何重置卡「使用」入口（R0），也不返回任何凭据或签名材料。
type ZhipuSignHandler struct {
	signService ZhipuSignAdminService
}

// ZhipuSignAdminService 是本 handler 依赖的窄接缝，生产实现是
// *service.ZhipuSignConfigService（票 28 的配置面）。
type ZhipuSignAdminService interface {
	// ConfigView 读取生效配置（运行层覆盖 > 部署层默认）。
	ConfigView(ctx context.Context) service.ZhipuSignConfigView
	// Update 校验并写入部分更新，返回更新后的生效视图。
	Update(ctx context.Context, update service.ZhipuSignConfigUpdate) (service.ZhipuSignConfigView, error)
	// Status 返回全局开关/降级策略的生效值 + 每个启用账号的私钥状态。
	Status(ctx context.Context) (*service.ZhipuSignStatus, error)
}

// NewZhipuSignHandler 构造管理端签名配置 handler。
func NewZhipuSignHandler(signService ZhipuSignAdminService) *ZhipuSignHandler {
	return &ZhipuSignHandler{signService: signService}
}

// GetConfig 读取签名生效配置。
// GET /api/v1/admin/zhipu/sign/config
func (h *ZhipuSignHandler) GetConfig(c *gin.Context) {
	if h == nil || h.signService == nil {
		response.Error(c, http.StatusServiceUnavailable, "zhipu sign config service is not available")
		return
	}
	response.Success(c, h.signService.ConfigView(c.Request.Context()))
}

// UpdateConfig 校验并写入签名配置修改（部分更新：只改传入的键）。
//
// 请求体是 service.ZhipuSignConfigUpdate（全部字段可选）；非法值返回 400 +
// reason=ZHIPU_SIGN_CONFIG_INVALID，且不写入任何键（旧值保持）。
// PUT /api/v1/admin/zhipu/sign/config
func (h *ZhipuSignHandler) UpdateConfig(c *gin.Context) {
	if h == nil || h.signService == nil {
		response.Error(c, http.StatusServiceUnavailable, "zhipu sign config service is not available")
		return
	}
	var req service.ZhipuSignConfigUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	view, err := h.signService.Update(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}

// GetStatus 读取签名状态（供前端 29 的「渠道签名 V4」区块）。
// GET /api/v1/admin/zhipu/sign/status
func (h *ZhipuSignHandler) GetStatus(c *gin.Context) {
	if h == nil || h.signService == nil {
		response.Error(c, http.StatusServiceUnavailable, "zhipu sign config service is not available")
		return
	}
	status, err := h.signService.Status(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}
