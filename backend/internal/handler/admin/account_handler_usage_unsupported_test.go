package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type usageUnsupportedAccountRepo struct {
	service.AccountRepository
	accounts []*service.Account
}

func (r *usageUnsupportedAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, account := range r.accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return nil, service.ErrAccountNotFound
}

// Scenario: 平台不支持用量查询的账号（codebuddy 短路、无 base_url 的 apikey
// fallback）请求 GET /admin/accounts/:id/usage 时必须得到 4xx 客户端错误
// （reason=USAGE_UNSUPPORTED），而不是落入通用 500 internal error。
func TestGetUsageUnsupportedAccountReturnsClientError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &usageUnsupportedAccountRepo{
		accounts: []*service.Account{
			{
				ID:       11,
				Platform: service.PlatformCodeBuddy,
				Type:     service.AccountTypeAPIKey,
				Credentials: map[string]any{
					"access_token": "at",
					"base_url":     "https://copilot.tencent.com",
				},
			},
			{
				ID:       12,
				Platform: service.PlatformAnthropic,
				Type:     service.AccountTypeAPIKey,
				Extra:    map[string]any{},
			},
		},
	}
	usageSvc := service.NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil, nil)
	handler := &AccountHandler{accountUsageService: usageSvc}

	for _, tc := range []struct {
		name       string
		accountID  string
		targetCode int
	}{
		{name: "codebuddy short-circuit", accountID: "11", targetCode: http.StatusBadRequest},
		{name: "api key fallback", accountID: "12", targetCode: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/admin/accounts/"+tc.accountID+"/usage", nil)
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = request
			ctx.Params = gin.Params{{Key: "id", Value: tc.accountID}}

			handler.GetUsage(ctx)

			require.Equal(t, tc.targetCode, recorder.Code)
			body := recorder.Body.String()
			require.Contains(t, body, `"reason":"USAGE_UNSUPPORTED"`)
			require.Contains(t, body, "does not support usage query")
			require.NotContains(t, body, "internal error")
		})
	}
}
