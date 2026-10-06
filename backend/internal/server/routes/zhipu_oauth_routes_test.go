//go:build unit

package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 智谱登录端点（票 04）必须挂在既有管理端分组下：未鉴权 401、非管理员 403。
// 同时守护 R0：管理面不得出现任何重置卡「使用」路由。
func TestZhipuOAuthAdminRoutesRequireAdminAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		ZhipuOAuth: adminhandler.NewZhipuOAuthHandler(nil, nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
		if strings.HasPrefix(route.Path, "/api/v1/admin/zhipu") {
			require.NotContains(t, route.Path, "/use", "R0：重置卡只读，管理面不得注册使用路由")
		}
	}

	paths := []struct {
		routePattern string
		requestPath  string
	}{
		{routePattern: "/api/v1/admin/zhipu/oauth/login-url", requestPath: "/api/v1/admin/zhipu/oauth/login-url"},
		{routePattern: "/api/v1/admin/zhipu/oauth/exchange", requestPath: "/api/v1/admin/zhipu/oauth/exchange"},
		{routePattern: "/api/v1/admin/zhipu/oauth/create-from-login", requestPath: "/api/v1/admin/zhipu/oauth/create-from-login"},
		{routePattern: "/api/v1/admin/zhipu/accounts/:id/relogin", requestPath: "/api/v1/admin/zhipu/accounts/7/relogin"},
	}
	for _, path := range paths {
		require.True(t, registered[http.MethodPost+" "+path.routePattern],
			"缺少管理端路由 POST %s（冻结契约，前端 05 逐字对齐）", path.routePattern)
	}

	for _, path := range paths {
		for _, tc := range []struct {
			name       string
			auth       string
			wantStatus int
		}{
			{name: "unauthenticated", wantStatus: http.StatusUnauthorized},
			{name: "non-admin", auth: "Bearer user-token", wantStatus: http.StatusForbidden},
		} {
			t.Run(path.requestPath+"/"+tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path.requestPath, strings.NewReader(`{}`))
				request.Header.Set("Content-Type", "application/json")
				if tc.auth != "" {
					request.Header.Set("Authorization", tc.auth)
				}
				router.ServeHTTP(recorder, request)
				require.Equal(t, tc.wantStatus, recorder.Code)
			})
		}
	}
}
