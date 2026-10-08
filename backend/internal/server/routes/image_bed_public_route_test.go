package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 图床公开直链的真实装配：真磁盘（repository.LocalImageBedStorage）+ 真
// service/provider + 真路由表。apiKeyAuth 一律拒绝并计数，用来证明读图确实匿名。
func TestImageBedPublicReadRouteServesLocalFileWithoutAPIKey(t *testing.T) {
	local := repository.NewLocalImageBedStorage(t.TempDir(), "")
	payload := []byte("\x89PNG\r\n\x1a\nfake-png-payload")
	url, err := local.Save(context.Background(), "bed/0123456789abcdef.png", "image/png", payload)
	require.NoError(t, err)
	// 无 public_base_url 时返回站点内相对直链——正是读路由的访问路径。
	require.Equal(t, "/v1/images/bed/0123456789abcdef.png", url)

	cfg := &config.Config{Gateway: config.GatewayConfig{
		MaxBodySize:     1024 * 1024,
		TextMaxBodySize: 1024 * 1024,
		// 没有 S3（settings=nil）：上传与读图都走本地磁盘后端。
		ImageBed: config.ImageBedConfig{Enabled: true, LocalEnabled: true},
	}}
	svc := service.ProvideImageBedService(nil, nil, nil, nil, cfg, local)
	t.Cleanup(svc.Stop)

	authCalls := 0
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{ImageBed: handler.ProvideImageBedHandler(svc)},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			authCalls++
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "API_KEY_REQUIRED", "API key is required")
		}),
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
	)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, url, nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "image/png", recorder.Header().Get("Content-Type"))
	require.Equal(t, payload, recorder.Body.Bytes())
	require.Zero(t, authCalls, "公开直链不得经过 API key 鉴权")

	// 不存在的 key → 404；穿越 key → 400（key 白名单校验在存储层，路由只做映射）。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/images/bed/ffffffffffffffff.png", nil))
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_not_found")

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/images/bed/..", nil))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_invalid_key")
	require.Zero(t, authCalls, "公开直链不得经过 API key 鉴权")
}
