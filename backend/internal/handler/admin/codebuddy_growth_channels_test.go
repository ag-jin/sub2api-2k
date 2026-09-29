package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// Scenario：`GET /admin/codebuddy/growth/channels` 的 **JSON 契约**必须包含授权字段。
//
// ## 为什么必须有这条测试
//
// 本端点的 handler 曾手写 `gin.H{...}` 只挑四个字段，于是新增
// `auto_authorized` / `auto_authorization` 后**静默丢掉**：
//   - `GrowthChannelSpecsForAPI()` 的单测断言的是**结构体**，结构体字段齐全 → 绿；
//   - 前端 spec 用的是**自己的 mock**，不经过这个 handler → 绿；
//   - 只有真跑 HTTP（dev 门禁 C）才暴露：`auto_authorized` 返回 `None`，
//     前端拿不到"已授权"标记，界面上三条已授权的 full 通道显示成普通状态。
//
// 所以这条测试直接断言**序列化后的 JSON**（而不是结构体），把
// "结构体有 → JSON 必须有"钉死。手写筛选字段的写法会立刻让它变红。
func TestCodeBuddyGrowthChannelsJSONCarriesAuthorizationFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/admin/codebuddy/growth/channels", (&CodeBuddyAdminHandler{}).GrowthChannels)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/codebuddy/growth/channels", nil))

	require.Equal(t, http.StatusOK, recorder.Code)

	// 解成原始 map 而不是结构体：要断言的是**实际下发的字段**。
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Channels []map[string]any `json:"channels"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.NotEmpty(t, envelope.Data.Channels, "通道列表不应为空")

	byKey := map[string]map[string]any{}
	for _, channel := range envelope.Data.Channels {
		key, _ := channel["key"].(string)
		require.NotEmpty(t, key, "每条通道都必须有 key")
		byKey[key] = channel
	}

	// 每条通道都必须带 auto_authorized 字段（即使是 false，也不能缺键——
	// 缺键让前端 v-if 判不出"明确未授权"与"后端忘了给"）。
	for key, channel := range byKey {
		require.Contains(t, channel, "auto_authorized",
			"通道 %s 的 JSON 缺少 auto_authorized（handler 是否在手写筛选字段？）", key)
		require.Contains(t, channel, "auto_runnable", "通道 %s 缺少 auto_runnable", key)
		require.Contains(t, channel, "rationale", "通道 %s 缺少 rationale", key)
	}

	// 已授权的 full 通道：必须同时带 auto_authorized=true 与非空授权依据。
	for _, key := range []string{
		codebuddy.CodeBuddyGrowthChannelAdopt,
		codebuddy.CodeBuddyGrowthChannelNightCat,
		codebuddy.CodeBuddyGrowthChannelSchool,
	} {
		channel, ok := byKey[key]
		require.True(t, ok, "通道 %s 未出现在 JSON 里", key)
		require.Equal(t, true, channel["auto_authorized"],
			"通道 %s 已获授权，JSON 里 auto_authorized 应为 true", key)
		authorization, _ := channel["auto_authorization"].(string)
		require.NotEmpty(t, authorization,
			"通道 %s 已授权但 JSON 里没有授权依据（前端无法展示可追溯性）", key)
		// 性质不得因授权而改变。
		require.Equal(t, "full", channel["tier"],
			"通道 %s 的性质应仍为 full（授权只改政策）", key)
	}

	// 抽奖：未授权，且不得进自动排程。
	lottery, ok := byKey[codebuddy.CodeBuddyGrowthChannelLottery]
	require.True(t, ok, "lottery 未出现在 JSON 里")
	require.Equal(t, false, lottery["auto_runnable"], "lottery 不得允许自动调度")
	require.Equal(t, false, lottery["auto_authorized"], "lottery 未获授权")
}
