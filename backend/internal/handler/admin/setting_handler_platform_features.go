package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
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
			})
		}
		out.Platforms = append(out.Platforms, item)
	}
	return out
}
