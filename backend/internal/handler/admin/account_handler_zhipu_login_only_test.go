package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 票 34：智谱建号收口为「仅登录授权」。通用建号接口（POST /api/v1/admin/accounts）
// 不再接受手动 API Key 形态的智谱凭据——智谱账号只能由登录授权产物
// （auth_flow=bigmodel_oauth）经专用端点 /api/v1/admin/zhipu/oauth/create-from-login 创建；
// 其它平台的手动 API Key 建号不受影响。
func TestAccountHandlerCreateZhipuLoginOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name        string
		body        map[string]any
		wantStatus  int
		wantCreated bool
	}{
		{
			name: "zhipu manual api key create is rejected",
			body: map[string]any{
				"name":     "zhipu-manual",
				"platform": "zhipu",
				"type":     "apikey",
				"credentials": map[string]any{
					"api_key":      "12345.secret",
					"base_url":     "https://open.bigmodel.cn/api/paas/v4",
					"account_mode": "payg",
					"api_protocol": "adaptive",
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "zhipu credentials without the login marker are rejected even without an api key",
			body: map[string]any{
				"name":        "zhipu-empty",
				"platform":    "zhipu",
				"type":        "apikey",
				"credentials": map[string]any{},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "zhipu login-managed credentials are not the manual shape and stay allowed",
			body: map[string]any{
				"name":     "zhipu-managed",
				"platform": "zhipu",
				"type":     "apikey",
				"credentials": map[string]any{
					"auth_flow":     "bigmodel_oauth",
					"api_key":       "12345.secret",
					"access_token":  "at-token",
					"zcodejwttoken": "jwt-token",
					"account_mode":  "coding",
					"api_protocol":  "adaptive",
				},
			},
			wantStatus:  http.StatusOK,
			wantCreated: true,
		},
		{
			name: "other platforms keep creating with a manual api key",
			body: map[string]any{
				"name":        "kimi-manual",
				"platform":    "kimi",
				"type":        "apikey",
				"credentials": map[string]any{"api_key": "sk-kimi"},
			},
			wantStatus:  http.StatusOK,
			wantCreated: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adminSvc := newStubAdminService()
			router := setupAccountMixedChannelRouter(adminSvc)

			raw, err := json.Marshal(tc.body)
			require.NoError(t, err)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code)

			if tc.wantCreated {
				require.Len(t, adminSvc.createdAccounts, 1)
				return
			}

			require.Empty(t, adminSvc.createdAccounts, "被拒的请求不得落到建号服务")

			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, "ZHIPU_LOGIN_ONLY", resp["reason"])
			require.Contains(t, resp["message"], "/api/v1/admin/zhipu/oauth/create-from-login")
		})
	}
}
