package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 平台功能设置的**路由注册可达性**测试（A4 批 4.0）。
//
// 为什么单独测路由：本轮已多次踩到「实现写完了但接线漏了」——
//   - 签到调度器没进 ProviderSet（wire_gen 未重生成，go build 直接失败）
//   - provideCleanup 收了形参却在闭包内引用 0 次
//   - 4.6 冷却执行者的 SetCodeBuddyCooldownApplier 全仓无人调用
//
// 端点同理：handler 写好了、单测全绿，但只要 `registerSettingsRoutes` 里那两行
// 漏了，管理员就永远够不着——用户裁定「在设置里能设置平台功能」依然落不了地。
//
// 这里**调用真实的 registerSettingsRoutes**，断言它把两条路径挂到了
// /admin/settings 之下。断言的是注册结果（gin 的路由表），不是源码文本。

// newAdminSettingsRouter 用真实 registerSettingsRoutes 注册设置路由。
//
// registerSettingsRoutes 只解引用 h.Admin.Setting（其余 handler 在别的
// register* 函数里用），所以只填 Setting 即可——这是**实现细节**，若将来
// 该函数用到更多 handler，这里会因为 nil 解引用立刻失败而不是静默跳过。
func newAdminSettingsRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	adminGroup := router.Group("/api/v1/admin")
	handlers := &handler.Handlers{
		Admin: &handler.AdminHandlers{
			Setting: &admin.SettingHandler{},
		},
	}
	registerSettingsRoutes(adminGroup, handlers)
	return router
}

// Scenario：GET/PUT /api/v1/admin/settings/platform-features 都真实注册成功。
//
// gin 对冲突路由会在注册时 panic；本测试不 recover，所以这条同时证明
// 新路径没有与既有设置端点撞车。
func TestPlatformFeaturesRoutesAreRegistered(t *testing.T) {
	router := newAdminSettingsRouter(t)

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	require.True(t, registered["GET /api/v1/admin/settings/platform-features"],
		"GET 平台功能端点未注册：管理员与前端都够不着，4.0 落不了地。已注册路径=%v", keysOf(registered))
	require.True(t, registered["PUT /api/v1/admin/settings/platform-features"],
		"PUT 平台功能端点未注册。已注册路径=%v", keysOf(registered))
}

// Scenario：既有设置端点未被本次改动挤掉（回归保护——注册顺序/前缀写错会连带失效）。
func TestExistingSettingsRoutesStillRegistered(t *testing.T) {
	router := newAdminSettingsRouter(t)

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	for _, path := range []string{
		"GET /api/v1/admin/settings",
		"PUT /api/v1/admin/settings",
		"GET /api/v1/admin/settings/rectifier",
		"GET /api/v1/admin/settings/web-search-emulation",
	} {
		require.True(t, registered[path], "既有设置端点 %s 不应消失", path)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
