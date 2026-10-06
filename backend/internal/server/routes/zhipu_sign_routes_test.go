//go:build unit

package routes

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// zhipuSignRouteServiceStub 实现管理端签名 handler 的窄接缝
// （handler/admin.ZhipuSignAdminService），供路由注册与审计断言使用。
type zhipuSignRouteServiceStub struct {
	mu      sync.Mutex
	updates []service.ZhipuSignConfigUpdate
}

func (s *zhipuSignRouteServiceStub) ConfigView(context.Context) service.ZhipuSignConfigView {
	return service.ZhipuSignConfigView{
		ZhipuSignConfig: service.ZhipuSignConfig{
			SignV4Enabled:     true,
			SignClientVersion: "0.16.9",
			SignPowBits:       8,
			SignFailPolicy:    service.ZhipuSignFailPolicyOpen,
		},
		OverriddenKeys: []string{},
	}
}

func (s *zhipuSignRouteServiceStub) Update(_ context.Context, update service.ZhipuSignConfigUpdate) (service.ZhipuSignConfigView, error) {
	s.mu.Lock()
	s.updates = append(s.updates, update)
	s.mu.Unlock()
	return s.ConfigView(context.Background()), nil
}

func (s *zhipuSignRouteServiceStub) Status(context.Context) (*service.ZhipuSignStatus, error) {
	return &service.ZhipuSignStatus{
		SignV4Enabled:  true,
		SignFailPolicy: service.ZhipuSignFailPolicyOpen,
		Accounts:       []service.ZhipuSignAccountStatus{},
	}, nil
}

func (s *zhipuSignRouteServiceStub) updateCalls() []service.ZhipuSignConfigUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.ZhipuSignConfigUpdate(nil), s.updates...)
}

// zhipuSignRouteAuditRepo 捕获审计写入（service.AuditLogRepository 的最小实现）。
type zhipuSignRouteAuditRepo struct {
	mu   sync.Mutex
	logs []*service.AuditLog
}

func (r *zhipuSignRouteAuditRepo) BatchInsert(_ context.Context, logs []*service.AuditLog) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logs...)
	return int64(len(logs)), nil
}

func (r *zhipuSignRouteAuditRepo) Insert(_ context.Context, log *service.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}

func (r *zhipuSignRouteAuditRepo) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return &service.AuditLogList{}, nil
}

func (r *zhipuSignRouteAuditRepo) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, service.ErrAuditLogNotFound
}

func (r *zhipuSignRouteAuditRepo) Count(context.Context) (int64, error) { return 0, nil }

func (r *zhipuSignRouteAuditRepo) TruncateAll(context.Context) error { return nil }

func (r *zhipuSignRouteAuditRepo) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (r *zhipuSignRouteAuditRepo) entries() []*service.AuditLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*service.AuditLog(nil), r.logs...)
}

// 签名配置/状态端点必须挂在既有 admin 分组下（未鉴权 401、非管理员 403），
// 且管理面不得出现重置卡「使用」路由（R0 不变量）。
func TestZhipuSignAdminRoutesRequireAdminAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		ZhipuSign: adminhandler.NewZhipuSignHandler(&zhipuSignRouteServiceStub{}),
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

	signRoutes := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/admin/zhipu/sign/config"},
		{method: http.MethodPut, path: "/api/v1/admin/zhipu/sign/config"},
		{method: http.MethodGet, path: "/api/v1/admin/zhipu/sign/status"},
	}
	for _, route := range signRoutes {
		require.True(t, registered[route.method+" "+route.path],
			"缺少管理端路由 %s %s（前端 29 与票 31 逐字对齐）", route.method, route.path)
	}

	for _, route := range signRoutes {
		for _, tc := range []struct {
			name       string
			auth       string
			wantStatus int
		}{
			{name: "unauthenticated", wantStatus: http.StatusUnauthorized},
			{name: "non-admin", auth: "Bearer user-token", wantStatus: http.StatusForbidden},
		} {
			t.Run(route.method+" "+route.path+"/"+tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
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

// 配置变更必须产生审计记录（design M3.1(d)(e)：协议漂移的修改要留痕）。
// 审计走 admin 分组挂载的既有中间件，不需要 handler 自己写审计。
func TestZhipuSignConfigUpdateProducesAuditEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	auditRepo := &zhipuSignRouteAuditRepo{}
	auditService := service.NewAuditLogService(auditRepo, nil)
	auditService.Start()

	stub := &zhipuSignRouteServiceStub{}
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		ZhipuSign: adminhandler.NewZhipuSignHandler(stub),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	router := gin.New()
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, servermiddleware.NewAuditLogMiddleware(auditService), stepUp, nil, nil)

	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/zhipu/sign/config",
		bytes.NewBufferString(`{"sign_fail_policy":"closed"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	auditService.Stop()

	require.Len(t, stub.updateCalls(), 1, "审计断言的前提是请求确实到达了服务层")
	entries := auditRepo.entries()
	require.Len(t, entries, 1)
	require.Equal(t, "admin.zhipu.sign.config.update", entries[0].Action)
	require.Equal(t, http.StatusOK, entries[0].StatusCode)
	require.Equal(t, "/api/v1/admin/zhipu/sign/config", entries[0].Path)
	require.Contains(t, entries[0].RequestBody, "sign_fail_policy", "变更内容必须留痕可查")
}
