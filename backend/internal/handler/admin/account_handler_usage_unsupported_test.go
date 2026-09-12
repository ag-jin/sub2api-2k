package admin

import (
	"context"
	"encoding/json"
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

// Scenario（改写自原 codebuddy 400 短路语义，A2 新语义）：codebuddy 账号的
// GET /admin/accounts/:id/usage 现在接入 CodeBuddy 实时积分 fetcher，
// 返回 UpstreamBalanceUsage 同构余额（upstream_balance.balance）而非 400；
// credits fetcher 未配置时仍保持 USAGE_UNSUPPORTED 短路；无 base_url 的
// anthropic apikey fallback 维持 400 客户端错误。
func TestGetUsageUnsupportedAccountReturnsClientError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fetcher := service.NewCodeBuddyCreditsFetcher(nil).
		WithTestBaseURL(codeBuddyUsageFixtureServer(t).URL)
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
	// 支持 credits 的 usage service：codebuddy 走 fetcher 余额。
	usageSvc := service.NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil, nil)
	usageSvc.SetCodeBuddyCreditsFetcher(fetcher)
	// 不支持 credits 的 usage service（未注入 fetcher）：保持 USAGE_UNSUPPORTED。
	usageSvcLegacy := service.NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil, nil)
	handler := &AccountHandler{accountUsageService: usageSvc}
	handlerLegacy := &AccountHandler{accountUsageService: usageSvcLegacy}

	// 1) codebuddy + credits fetcher → 200 + upstream_balance.balance。
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/accounts/11/usage", nil)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "id", Value: "11"}}
	handler.GetUsage(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		UpstreamBalance *struct {
			Balance *float64 `json:"balance"`
			Status  string   `json:"status"`
		} `json:"upstream_balance"`
	}
	// response.Success 包裹 {code,message,data}：先解信封再读 usage。
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.NoError(t, json.Unmarshal(envelope.Data, &payload))
	require.NotNil(t, payload.UpstreamBalance)
	require.NotNil(t, payload.UpstreamBalance.Balance)
	// 实证两套餐 Precise 求和：499.95 + 100 = 599.95。
	require.InDelta(t, 599.95, *payload.UpstreamBalance.Balance, 1e-9)

	// 2) codebuddy 无 fetcher → 400 USAGE_UNSUPPORTED（既有语义保留）。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/admin/accounts/11/usage", nil)
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "id", Value: "11"}}
	handlerLegacy.GetUsage(ctx)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "USAGE_UNSUPPORTED")

	// 3) anthropic apikey fallback → 400 USAGE_UNSUPPORTED。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/admin/accounts/12/usage", nil)
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "id", Value: "12"}}
	handlerLegacy.GetUsage(ctx)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "does not support usage query")
	require.NotContains(t, recorder.Body.String(), "internal error")
}

// codeBuddyUsageFixtureServer 测试计费端点（两套餐 Precise 求和 599.95）。
func codeBuddyUsageFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	const envelope = `{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacityRemainPrecise":"499.95","CycleCapacityRemain":499,"CycleCapacitySize":500},
		{"CycleCapacityRemainPrecise":"100","CycleCapacityRemain":100,"CapacityRemain":100}
	]}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelope))
	}))
	t.Cleanup(server.Close)
	return server
}
