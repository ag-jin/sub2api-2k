package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 平台功能设置端点测试（A4 批 4.0 的 HTTP 层）。
//
// 为什么必须测端点而不是只测 service：本轮已两次踩到「service 层做完了但没人调用」
// （调度器未进 ProviderSet、4.6 冷却执行者从未注入）。端点这一环同理——
// service 方法齐全但没注册路由，用户裁定「在设置里能设置平台功能」就落不了地，
// 而所有 service 单测仍然全绿。

// platformFeatureHandlerRepo settings stub（只实现端点路径会用到的读/写）。
type platformFeatureHandlerRepo struct {
	vals map[string]string
}

func (r *platformFeatureHandlerRepo) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := r.vals[key]; ok {
		return v, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *platformFeatureHandlerRepo) Set(_ context.Context, key, value string) error {
	r.vals[key] = value
	return nil
}

func (r *platformFeatureHandlerRepo) Get(_ context.Context, _ string) (*service.Setting, error) {
	panic("unused")
}
func (r *platformFeatureHandlerRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	panic("unused")
}
func (r *platformFeatureHandlerRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	panic("unused")
}
func (r *platformFeatureHandlerRepo) GetAll(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.vals {
		out[k] = v
	}
	return out, nil
}
func (r *platformFeatureHandlerRepo) Delete(_ context.Context, _ string) error { panic("unused") }

var _ service.SettingRepository = (*platformFeatureHandlerRepo)(nil)

// setupPlatformFeatureRouter 挂上真实 handler + 真实路由路径。
func setupPlatformFeatureRouter(t *testing.T) (*gin.Engine, *platformFeatureHandlerRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &platformFeatureHandlerRepo{vals: map[string]string{}}
	settingService := service.NewSettingService(repo, &config.Config{})
	handler := NewSettingHandler(settingService, nil, nil, nil, nil, nil, nil)

	router := gin.New()
	router.GET("/api/v1/admin/settings/platform-features", handler.GetPlatformFeatures)
	router.PUT("/api/v1/admin/settings/platform-features", handler.UpdatePlatformFeatures)
	return router, repo
}

// platformFeaturesResponse 反序列化 GET/PUT 的响应体。
type platformFeaturesResponse struct {
	Data struct {
		Platforms []struct {
			Platform string `json:"platform"`
			Features []struct {
				Key   string `json:"key"`
				Kind  string `json:"kind"`
				Title string `json:"title"`
				Value struct {
					Enabled bool `json:"enabled"`
					Start   *struct {
						Hour   int `json:"hour"`
						Minute int `json:"minute"`
					} `json:"start"`
					End *struct {
						Hour   int `json:"hour"`
						Minute int `json:"minute"`
					} `json:"end"`
				} `json:"value"`
			} `json:"features"`
		} `json:"platforms"`
	} `json:"data"`
}

func decodePlatformFeatures(t *testing.T, rec *httptest.ResponseRecorder) platformFeaturesResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var decoded platformFeaturesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded), "body=%s", rec.Body.String())
	return decoded
}

func findCodeBuddyCheckin(t *testing.T, resp platformFeaturesResponse) (kind string, enabled bool, start, end string) {
	t.Helper()
	for _, group := range resp.Data.Platforms {
		if group.Platform != service.PlatformCodeBuddy {
			continue
		}
		for _, feature := range group.Features {
			if feature.Key != service.CodeBuddyCheckinFeatureKey {
				continue
			}
			kind = feature.Kind
			enabled = feature.Value.Enabled
			if feature.Value.Start != nil {
				start = timeOfDayHHMM(feature.Value.Start.Hour, feature.Value.Start.Minute)
			}
			if feature.Value.End != nil {
				end = timeOfDayHHMM(feature.Value.End.Hour, feature.Value.End.Minute)
			}
			return kind, enabled, start, end
		}
	}
	t.Fatalf("codebuddy/%s 未出现在响应里：%+v", service.CodeBuddyCheckinFeatureKey, resp.Data.Platforms)
	return
}

// timeOfDayHHMM 渲染 HH:MM（测试内自备，避免与 service 的 Format 互证）。
func timeOfDayHHMM(hour, minute int) string {
	two := func(v int) string {
		if v < 10 {
			return "0" + strconv.Itoa(v)
		}
		return strconv.Itoa(v)
	}
	return two(hour) + ":" + two(minute)
}

// Scenario：GET 在**从未配置**时也要返回 codebuddy 分组，且签到为关闭 + 09:00–11:00。
// 这同时证明三件事：注册表被读到、默认值被补齐、默认关闭。
func TestPlatformFeaturesEndpointGetDefaults(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	resp := decodePlatformFeatures(t, doJSON(t, router, http.MethodGet, "/api/v1/admin/settings/platform-features", nil))

	kind, enabled, start, end := findCodeBuddyCheckin(t, resp)
	require.Equal(t, "time_range", kind)
	require.False(t, enabled, "从未配置时签到必须默认关闭")
	require.Equal(t, "09:00", start)
	require.Equal(t, "11:00", end)
}

// Scenario：PUT 稀疏提交 enabled=true 后，GET 回读到开启，且默认窗口仍在。
// 并验证**独立 GET** 也能读到（PUT 的回读不是唯一证据）。
func TestPlatformFeaturesEndpointPutEnablesCheckin(t *testing.T) {
	router, repo := setupPlatformFeatureRouter(t)

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyCheckinFeatureKey,
				"value": map[string]any{"enabled": true}},
		},
	})
	resp := decodePlatformFeatures(t, rec)

	_, enabled, start, end := findCodeBuddyCheckin(t, resp)
	require.True(t, enabled, "PUT 后应开启")
	require.Equal(t, "09:00", start, "未提交时间点时保留注册默认窗口")
	require.Equal(t, "11:00", end)

	// 落库确实写到了 platform_features 键。
	require.Contains(t, repo.vals[service.PlatformFeatureStorageKey], "codebuddy")

	// 独立 GET 回读（证明是持久化生效，不是 PUT 的返回值凑巧对）。
	getResp := decodePlatformFeatures(t, doJSON(t, router, http.MethodGet, "/api/v1/admin/settings/platform-features", nil))
	_, getEnabled, getStart, getEnd := findCodeBuddyCheckin(t, getResp)
	require.True(t, getEnabled)
	require.Equal(t, "09:00", getStart)
	require.Equal(t, "11:00", getEnd)
}

// Scenario：PUT 自定义窗口生效。
func TestPlatformFeaturesEndpointPutCustomWindow(t *testing.T) {
	router, repo := setupPlatformFeatureRouter(t)

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyCheckinFeatureKey,
				"value": map[string]any{
					"enabled": true,
					"start":   map[string]any{"hour": 14, "minute": 30},
					"end":     map[string]any{"hour": 15, "minute": 30},
				}},
		},
	})
	resp := decodePlatformFeatures(t, rec)

	_, enabled, start, end := findCodeBuddyCheckin(t, resp)
	require.True(t, enabled)
	require.Equal(t, "14:30", start)
	require.Equal(t, "15:30", end)

	// 落库形态：归一化后的文档写在 platform_features 键上。
	require.Contains(t, repo.vals[service.PlatformFeatureStorageKey], "codebuddy")
}

// Scenario：**零长窗口**（start == end）被服务端修正回默认，不回显用户提交的坏值。
// 否则"功能开着但窗口永不命中"会静默失效，比报错更难查。
func TestPlatformFeaturesEndpointNormalizesZeroLengthWindow(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyCheckinFeatureKey,
				"value": map[string]any{
					"enabled": true,
					"start":   map[string]any{"hour": 10, "minute": 0},
					"end":     map[string]any{"hour": 10, "minute": 0},
				}},
		},
	})
	resp := decodePlatformFeatures(t, rec)

	_, enabled, start, end := findCodeBuddyCheckin(t, resp)
	require.True(t, enabled)
	require.Equal(t, "09:00", start, "零长窗口应被修正回默认")
	require.Equal(t, "11:00", end)
}

// Scenario：未注册平台 / 未声明功能 → 400，且**整次写不做**（不落半套配置）。
func TestPlatformFeaturesEndpointRejectsUnknown(t *testing.T) {
	router, repo := setupPlatformFeatureRouter(t)

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": "never-declared", "value": map[string]any{"enabled": true}},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": "never-registered", "key": "whatever", "value": map[string]any{"enabled": true}},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	require.NotContains(t, repo.vals, service.PlatformFeatureStorageKey, "拒绝时不得落库")
}

// Scenario：空 features → 400（不给"什么都不提交=清空"的歧义入口）。
func TestPlatformFeaturesEndpointRejectsEmptyFeatures(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features",
		map[string]any{"features": []any{}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// Scenario：请求体不是合法 JSON → 400。
func TestPlatformFeaturesEndpointRejectsMalformedBody(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/platform-features", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// Scenario：**稀疏语义**——只提交一个项时，既有文档里其他平台/功能的键不得被清掉。
// 这条挡住"读改写退化成整文档覆盖"的回归。
func TestPlatformFeaturesEndpointIsSparse(t *testing.T) {
	router, repo := setupPlatformFeatureRouter(t)

	// 预置一份含额外键的文档（模拟历史配置）。
	repo.vals[service.PlatformFeatureStorageKey] = `{"codebuddy":{"checkin":{"enabled":true,"start":{"hour":9,"minute":0},"end":{"hour":11,"minute":0}}}}`

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyCheckinFeatureKey,
				"value": map[string]any{"enabled": false}},
		},
	})
	resp := decodePlatformFeatures(t, rec)

	_, enabled, start, end := findCodeBuddyCheckin(t, resp)
	require.False(t, enabled, "本次提交的项应生效")
	require.Equal(t, "09:00", start, "未提交的时间点应保留")
	require.Equal(t, "11:00", end)
}

// --- A5 批 5.4：活跃上报作为平台功能项 ---

// findCodeBuddyActivity 同 findCodeBuddyCheckin，取活跃上报项。
func findCodeBuddyActivity(t *testing.T, resp platformFeaturesResponse) (kind string, enabled bool, start, end string) {
	t.Helper()
	for _, group := range resp.Data.Platforms {
		if group.Platform != service.PlatformCodeBuddy {
			continue
		}
		for _, feature := range group.Features {
			if feature.Key != service.CodeBuddyActivityFeatureKey {
				continue
			}
			kind = feature.Kind
			enabled = feature.Value.Enabled
			if feature.Value.Start != nil {
				start = timeOfDayHHMM(feature.Value.Start.Hour, feature.Value.Start.Minute)
			}
			if feature.Value.End != nil {
				end = timeOfDayHHMM(feature.Value.End.Hour, feature.Value.End.Minute)
			}
			return kind, enabled, start, end
		}
	}
	t.Fatalf("codebuddy/%s 未出现在响应里：%+v", service.CodeBuddyActivityFeatureKey, resp.Data.Platforms)
	return
}

// Scenario：从未配置时，活跃上报项也要出现在设置里、默认关闭、默认 10:00–11:00。
// 这条同时守住"注册了但没被端点读出来"这类静默失败（本轮已踩过两次同类）。
func TestPlatformFeaturesEndpointExposesActivityReport(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	resp := decodePlatformFeatures(t, doJSON(t, router, http.MethodGet, "/api/v1/admin/settings/platform-features", nil))

	kind, enabled, start, end := findCodeBuddyActivity(t, resp)
	require.Equal(t, "time_range", kind)
	require.False(t, enabled, "从未配置时活跃上报必须默认关闭（对外写操作）")
	require.Equal(t, "10:00", start, "默认时点对齐参考实现 activity_hours=[10]")
	require.Equal(t, "11:00", end)
}

// Scenario：开启活跃上报不影响签到项（两个功能项各自独立）。
// 这是"同一平台多功能项"最容易出的错：稀疏写入把同平台另一项重置。
func TestPlatformFeaturesEndpointActivityDoesNotDisturbCheckin(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	// 先各自开启一次
	doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyCheckinFeatureKey,
				"value": map[string]any{"enabled": true}},
		},
	})
	doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyActivityFeatureKey,
				"value": map[string]any{"enabled": true}},
		},
	})

	resp := decodePlatformFeatures(t, doJSON(t, router, http.MethodGet, "/api/v1/admin/settings/platform-features", nil))

	_, checkinEnabled, checkinStart, _ := findCodeBuddyCheckin(t, resp)
	require.True(t, checkinEnabled, "写活跃上报不该把签到关掉")
	require.Equal(t, "09:00", checkinStart, "签到的窗口也不该被改动")

	_, activityEnabled, activityStart, _ := findCodeBuddyActivity(t, resp)
	require.True(t, activityEnabled)
	require.Equal(t, "10:00", activityStart)
}

// Scenario：活跃上报的自定义时点可下发并回读。
func TestPlatformFeaturesEndpointActivityCustomWindow(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)

	doJSON(t, router, http.MethodPut, "/api/v1/admin/settings/platform-features", map[string]any{
		"features": []map[string]any{
			{"platform": service.PlatformCodeBuddy, "key": service.CodeBuddyActivityFeatureKey,
				"value": map[string]any{
					"enabled": true,
					"start":   map[string]any{"hour": 21, "minute": 0},
					"end":     map[string]any{"hour": 22, "minute": 30},
				}},
		},
	})

	resp := decodePlatformFeatures(t, doJSON(t, router, http.MethodGet, "/api/v1/admin/settings/platform-features", nil))
	_, enabled, start, end := findCodeBuddyActivity(t, resp)
	require.True(t, enabled)
	require.Equal(t, "21:00", start)
	require.Equal(t, "22:30", end)
}

// Scenario：每个注册的功能项都必须带可读 Title（前端用 feature.title || feature.key
// 兜底，Title 为空会让管理员在设置页看到裸 key，等于没做 UI）。
// 这是"新增功能项忘了给文案"的回归保护——注册表本身不强制 Title。
func TestPlatformFeaturesEveryRegisteredFeatureHasTitle(t *testing.T) {
	router, _ := setupPlatformFeatureRouter(t)
	resp := decodePlatformFeatures(t, doJSON(t, router, http.MethodGet, "/api/v1/admin/settings/platform-features", nil))

	require.NotEmpty(t, resp.Data.Platforms, "注册表不该为空")
	for _, group := range resp.Data.Platforms {
		require.NotEmpty(t, group.Features, "平台 %s 应至少有一个功能项", group.Platform)
		for _, feature := range group.Features {
			require.NotEmpty(t, feature.Title,
				"平台 %s 的功能 %s 缺少 Title（设置页会显示裸 key）", group.Platform, feature.Key)
			require.NotEqual(t, feature.Key, feature.Title,
				"平台 %s 的功能 %s 的 Title 与 key 相同，等于没给文案", group.Platform, feature.Key)
		}
	}
}
