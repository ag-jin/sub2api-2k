package admin

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 平台功能设置端点（A4 批 4.0 的「设置页可利用」这一环）。
//
// 背景：service 层已有「平台 → 功能」注册表与读写方法（platform_feature_settings.go），
// 但此前没有任何 HTTP 端点——管理员与前端都够不着，用户裁定「在设置里添加一个
// 平台功能，不同平台的选择可以在这里设置」落不了地。
//
// 端点形态对齐既有设置端点先例（rectifier / stream-timeout / beta-policy）：
// 独立 GET + PUT，不复用 PUT /admin/settings（后者是整表单语义，改一个功能要提交
// 完整 SystemSettings，前端极难用对且极易误清其他设置）。
//
// 形状：GET 按平台分组返回该平台已注册功能与当前**生效值**（默认值已补齐）；
// PUT 稀疏提交（只给要改的项），未提交项保持原值。

// PlatformFeatureItem 单个功能项（含前端渲染所需的形状信息）。
type PlatformFeatureItem struct {
	Key         string                       `json:"key"`
	Kind        string                       `json:"kind"` // bool | time_range
	Title       string                       `json:"title"`
	Description string                       `json:"description"`
	Value       service.PlatformFeatureValue `json:"value"`
	// SupportsImmediateRun 该功能是否具备"立即执行"能力（设置页据此渲染按钮）。
	//
	// 由服务端注册表回答，前端不猜、不硬编码 featureKey：新增支持立即执行的
	// 功能后，设置页无需改动就会显示按钮。
	SupportsImmediateRun bool `json:"supports_immediate_run"`
}

// PlatformFeatureGroup 一个平台的功能分组。
type PlatformFeatureGroup struct {
	Platform string                `json:"platform"`
	Features []PlatformFeatureItem `json:"features"`
}

// PlatformFeaturesResponse GET/PUT 响应。
type PlatformFeaturesResponse struct {
	Platforms []PlatformFeatureGroup `json:"platforms"`
}

// UpdatePlatformFeatureItem PUT 请求里的单项。
type UpdatePlatformFeatureItem struct {
	Platform string                       `json:"platform"`
	Key      string                       `json:"key"`
	Value    service.PlatformFeatureValue `json:"value"`
}

// UpdatePlatformFeaturesRequest PUT 请求体（稀疏：只提交要改的项）。
type UpdatePlatformFeaturesRequest struct {
	Features []UpdatePlatformFeatureItem `json:"features"`
}

// GetPlatformFeatures 读取全部平台级功能设置（按平台分组）。
// GET /api/v1/admin/settings/platform-features
func (h *SettingHandler) GetPlatformFeatures(c *gin.Context) {
	groups, err := h.settingService.GetPlatformFeatureGroups(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, platformFeatureGroupsToResponse(groups))
}

// UpdatePlatformFeatures 稀疏更新若干平台级功能设置。
// PUT /api/v1/admin/settings/platform-features
//
// 未知平台/未声明功能 → 400 且**整次写不做**（不落半套配置）。
func (h *SettingHandler) UpdatePlatformFeatures(c *gin.Context) {
	var req UpdatePlatformFeaturesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.Features) == 0 {
		response.BadRequest(c, "features is required")
		return
	}

	updates := make([]service.PlatformFeatureUpdate, 0, len(req.Features))
	for _, item := range req.Features {
		updates = append(updates, service.PlatformFeatureUpdate{
			Platform: item.Platform,
			Key:      item.Key,
			Value:    item.Value,
		})
	}

	if err := h.settingService.UpdatePlatformFeatures(c.Request.Context(), updates); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// 回读生效值返回，避免前端拿到自己提交的未归一化结果
	// （越界/零长时间点会被服务端修正成默认值）。
	groups, err := h.settingService.GetPlatformFeatureGroups(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, platformFeatureGroupsToResponse(groups))
}

func platformFeatureGroupsToResponse(groups []service.PlatformFeatureReadGroup) PlatformFeaturesResponse {
	out := PlatformFeaturesResponse{Platforms: make([]PlatformFeatureGroup, 0, len(groups))}
	for _, group := range groups {
		item := PlatformFeatureGroup{
			Platform: group.Platform,
			Features: make([]PlatformFeatureItem, 0, len(group.Features)),
		}
		for _, feature := range group.Features {
			item.Features = append(item.Features, PlatformFeatureItem{
				Key:         feature.Key,
				Kind:        string(feature.Kind),
				Title:       feature.Title,
				Description: feature.Description,
				Value:       feature.Value,
				// 直接问注册表，不猜 Key。
				SupportsImmediateRun: service.PlatformFeatureSupportsImmediateRun(
					group.Platform, feature.Key,
				),
			})
		}
		out.Platforms = append(out.Platforms, item)
	}
	return out
}

// RunPlatformFeatureRequest POST 请求体：点名要立即执行哪个功能。
type RunPlatformFeatureRequest struct {
	Platform string `json:"platform"`
	Key      string `json:"key"`
}

// PlatformFeatureRunResponse 立即执行的回执。
//
// `Summary` 是服务端生成的一句话结果（各功能回执形状差异大，设置页不该逐个解析）；
// `Detail` 是原始回执，供排查用，前端不解析。
type PlatformFeatureRunResponse struct {
	Summary string `json:"summary"`
	Detail  any    `json:"detail,omitempty"`
}

// RunPlatformFeatureNow 立即执行某平台功能。
// POST /api/v1/admin/settings/platform-features/run
//
// 用途：平台功能只有"开关+时段"时，用户唯一的触发手段是**等窗口**——
// 窗口设得晚、或当天已经错过（在窗口外才配置），就只能等第二天。
// 本端点给设置页一个"现在就跑"的按钮，把"再等一天"变成一次性动作。
//
// 与开关/窗口的关系：**都不读**。开关管自动排程，本端点是"人明确要求现在执行"；
// 窗口更不该管——"错过窗口想补一次"正是本能力要解决的场景。
// 这与各平台既有手动端点的口径一致。
//
// 分级：执行者的动作仍受各平台自己的合规约束（例如 codebuddy 的成长链只跑
// 已授权自动的通道，未授权通道结构上带不上）。本端点只是"触发器"，
// 不改变任何动作的合规级别。
func (h *SettingHandler) RunPlatformFeatureNow(c *gin.Context) {
	if h == nil || h.settingService == nil {
		response.BadRequest(c, "setting service is not enabled")
		return
	}
	var req RunPlatformFeatureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Platform) == "" || strings.TrimSpace(req.Key) == "" {
		response.BadRequest(c, "platform and key are required")
		return
	}

	result, err := h.settingService.RunPlatformFeatureNow(c.Request.Context(), req.Platform, req.Key)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 审计：记"谁在什么时候手动触发了哪个平台功能"。手动触发是显式的人为动作，
	// 事后追查"这批积分怎么来的"时这条是唯一线索。
	middleware.SetAuditAction(c, "admin.settings.platform_feature.run")
	middleware.SetAuditExtra(c, map[string]any{
		"platform": req.Platform,
		"key":      req.Key,
	})

	out := PlatformFeatureRunResponse{}
	if result != nil {
		out.Summary = result.Summary
		out.Detail = result.Detail
	}
	response.Success(c, out)
}
