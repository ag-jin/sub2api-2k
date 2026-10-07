package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupAccountPlatformCountsRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.GET("/api/v1/admin/accounts/platform-counts", handler.GetPlatformCounts)
	return router, adminSvc
}

// 侧边栏「账号管理」子项靠这个只读端点隐藏没有账号的平台，
// 契约是 {code:0, data:{<platform>: <count>}}，且只包含有账号的平台。
func TestAccountHandlerGetPlatformCounts(t *testing.T) {
	serviceErr := errors.New("platform counts unavailable")

	tests := []struct {
		name       string
		counts     map[string]int64
		err        error
		wantStatus int
		wantData   map[string]float64
	}{
		{
			name:       "按平台分组返回各自计数",
			counts:     map[string]int64{"anthropic": 3, "zhipu": 1, "codebuddy": 2},
			wantStatus: http.StatusOK,
			wantData:   map[string]float64{"anthropic": 3, "zhipu": 1, "codebuddy": 2},
		},
		{
			name:       "零账号平台不出现（服务层只回有账号的平台）",
			counts:     map[string]int64{"minimax": 4},
			wantStatus: http.StatusOK,
			wantData:   map[string]float64{"minimax": 4},
		},
		{
			name:       "一个账号都没有时返回空对象而不是 null",
			counts:     map[string]int64{},
			wantStatus: http.StatusOK,
			wantData:   map[string]float64{},
		},
		{
			name:       "服务层返回 nil map 时归一化为空对象",
			counts:     nil,
			wantStatus: http.StatusOK,
			wantData:   map[string]float64{},
		},
		{
			name:       "服务层失败时返回 500 且不带数据",
			err:        serviceErr,
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, adminSvc := setupAccountPlatformCountsRouter()
			adminSvc.accountPlatformCounts = tt.counts
			adminSvc.accountPlatformCountsErr = tt.err

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/platform-counts", nil)
			router.ServeHTTP(rec, req)

			require.Equal(t, tt.wantStatus, rec.Code)
			require.Equal(t, 1, adminSvc.getAccountPlatformCountsCalls)

			var payload struct {
				Code int                `json:"code"`
				Data map[string]float64 `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))

			if tt.wantStatus != http.StatusOK {
				require.NotEqual(t, 0, payload.Code)
				require.Nil(t, payload.Data)
				return
			}

			require.Equal(t, 0, payload.Code)
			require.Equal(t, tt.wantData, payload.Data)
		})
	}
}
