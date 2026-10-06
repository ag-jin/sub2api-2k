//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// zhipuSignHandlerServiceStub 是 handler 窄接缝的替身：记录收到的更新请求，返回预设响应。
type zhipuSignHandlerServiceStub struct {
	configView    service.ZhipuSignConfigView
	updateView    service.ZhipuSignConfigView
	updateErr     error
	updateCalls   []service.ZhipuSignConfigUpdate
	status        *service.ZhipuSignStatus
	statusErr     error
	configCalls   int
	statusQueries int
}

func (s *zhipuSignHandlerServiceStub) ConfigView(context.Context) service.ZhipuSignConfigView {
	s.configCalls++
	return s.configView
}

func (s *zhipuSignHandlerServiceStub) Update(_ context.Context, update service.ZhipuSignConfigUpdate) (service.ZhipuSignConfigView, error) {
	s.updateCalls = append(s.updateCalls, update)
	if s.updateErr != nil {
		return service.ZhipuSignConfigView{}, s.updateErr
	}
	return s.updateView, nil
}

func (s *zhipuSignHandlerServiceStub) Status(context.Context) (*service.ZhipuSignStatus, error) {
	s.statusQueries++
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return s.status, nil
}

func newZhipuSignHandlerRouter(stub *zhipuSignHandlerServiceStub) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewZhipuSignHandler(stub)
	router := gin.New()
	router.GET("/api/v1/admin/zhipu/sign/config", handler.GetConfig)
	router.PUT("/api/v1/admin/zhipu/sign/config", handler.UpdateConfig)
	router.GET("/api/v1/admin/zhipu/sign/status", handler.GetStatus)
	return router
}

func zhipuSignHandlerDo(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

// GET /sign/config 返回生效配置与「已覆盖键」列表（前端 29 与 L3 演练 31 的契约）。
func TestZhipuSignHandlerGetConfigReturnsEffectiveValues(t *testing.T) {
	t.Parallel()

	stub := &zhipuSignHandlerServiceStub{
		configView: service.ZhipuSignConfigView{
			ZhipuSignConfig: service.ZhipuSignConfig{
				SignV4Enabled:                    true,
				SignClientVersion:                "0.16.9",
				SignPowBits:                      8,
				SignKeyTTLMinutes:                1440,
				SignHandshakeBackoffSeconds:      30,
				SignAccountCircuitBreakThreshold: 10,
				SignFailPolicy:                   service.ZhipuSignFailPolicyOpen,
				SignAlertEnabled:                 true,
				SignReconcileIntervalHours:       6,
				SignReconcileDeviationThreshold:  0.70,
			},
			OverriddenKeys: []string{"gateway.zhipu.sign_pow_bits"},
		},
	}
	router := newZhipuSignHandlerRouter(stub)

	recorder := zhipuSignHandlerDo(router, http.MethodGet, "/api/v1/admin/zhipu/sign/config", "")

	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope struct {
		Code int                         `json:"code"`
		Data service.ZhipuSignConfigView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.Equal(t, "0.16.9", envelope.Data.SignClientVersion)
	require.Equal(t, 8, envelope.Data.SignPowBits)
	require.Equal(t, service.ZhipuSignFailPolicyOpen, envelope.Data.SignFailPolicy)
	require.Equal(t, []string{"gateway.zhipu.sign_pow_bits"}, envelope.Data.OverriddenKeys)
	require.Equal(t, 1, stub.configCalls)
}

// PUT /sign/config：请求体按部分更新解析（只传的键才非 nil），成功后返回生效视图。
func TestZhipuSignHandlerUpdateConfigBindsPartialUpdate(t *testing.T) {
	t.Parallel()

	stub := &zhipuSignHandlerServiceStub{
		updateView: service.ZhipuSignConfigView{
			ZhipuSignConfig: service.ZhipuSignConfig{SignFailPolicy: service.ZhipuSignFailPolicyClosed, SignPowBits: 8},
			OverriddenKeys:  []string{"gateway.zhipu.sign_fail_policy"},
		},
	}
	router := newZhipuSignHandlerRouter(stub)

	recorder := zhipuSignHandlerDo(router, http.MethodPut, "/api/v1/admin/zhipu/sign/config", `{"sign_fail_policy":"closed"}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, stub.updateCalls, 1)
	require.NotNil(t, stub.updateCalls[0].SignFailPolicy)
	require.Equal(t, "closed", *stub.updateCalls[0].SignFailPolicy)
	require.Nil(t, stub.updateCalls[0].SignPowBits, "未传的键不得被解析成零值覆盖")
	require.Nil(t, stub.updateCalls[0].SignClientVersion)

	var envelope struct {
		Data service.ZhipuSignConfigView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, service.ZhipuSignFailPolicyClosed, envelope.Data.SignFailPolicy)
}

// 请求体类型错误（例如把数字写成字符串）在绑定阶段就被拒，服务层不会被调用。
func TestZhipuSignHandlerUpdateConfigRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	stub := &zhipuSignHandlerServiceStub{}
	router := newZhipuSignHandlerRouter(stub)

	recorder := zhipuSignHandlerDo(router, http.MethodPut, "/api/v1/admin/zhipu/sign/config", `{"sign_pow_bits":"high"}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, stub.updateCalls)
	require.Contains(t, recorder.Body.String(), "Invalid request body")
}

// 校验失败必须原样透出可读错误：HTTP 400 + reason=ZHIPU_SIGN_CONFIG_INVALID + 指明键名。
func TestZhipuSignHandlerUpdateConfigMapsValidationErrorToBadRequest(t *testing.T) {
	t.Parallel()

	stub := &zhipuSignHandlerServiceStub{
		updateErr: infraerrors.BadRequest(service.ZhipuSignConfigReasonInvalid,
			"sign_pow_bits must be an integer in [0,16], got 20"),
	}
	router := newZhipuSignHandlerRouter(stub)

	recorder := zhipuSignHandlerDo(router, http.MethodPut, "/api/v1/admin/zhipu/sign/config", `{"sign_pow_bits":20}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, http.StatusBadRequest, envelope.Code)
	require.Equal(t, service.ZhipuSignConfigReasonInvalid, envelope.Reason)
	require.Contains(t, envelope.Message, "sign_pow_bits")
}

// GET /sign/status 的响应契约（前端 29 冻结字段名）：全局生效值 + 每个启用账号的状态，
// 含 TODO(#24) 的占位字段（runtime_state_available=false 表示熔断状态尚未接线）。
func TestZhipuSignHandlerGetStatusReturnsFrozenContract(t *testing.T) {
	t.Parallel()

	handshakedAt := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	stub := &zhipuSignHandlerServiceStub{
		status: &service.ZhipuSignStatus{
			SignV4Enabled:  true,
			SignFailPolicy: service.ZhipuSignFailPolicyOpen,
			Accounts: []service.ZhipuSignAccountStatus{
				{
					AccountID:           7,
					AccountName:         "zhipu-signed",
					KeyCached:           true,
					LastHandshakeAt:     &handshakedAt,
					ConsecutiveFailures: 2,
				},
				{
					AccountID:             9,
					AccountName:           "zhipu-cold",
					KeyCached:             false,
					RuntimeStateAvailable: false,
				},
			},
		},
	}
	router := newZhipuSignHandlerRouter(stub)

	recorder := zhipuSignHandlerDo(router, http.MethodGet, "/api/v1/admin/zhipu/sign/status", "")

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	for _, field := range []string{
		`"sign_v4_enabled"`, `"sign_fail_policy"`, `"accounts"`, `"account_id"`, `"account_name"`,
		`"key_cached"`, `"last_handshake_at"`, `"key_expires_at"`, `"consecutive_failures"`,
		`"circuit_break_tripped"`, `"circuit_break_reason"`, `"runtime_state_available"`,
	} {
		require.Contains(t, body, field, "状态响应必须冻结这些字段（前端 29 逐字对齐）")
	}
	require.Contains(t, body, `"last_handshake_at":"2026-10-06T12:00:00Z"`)
	require.Contains(t, body, `"last_handshake_at":null`, "未握手账号必须显式 null，而不是零值时间")

	var envelope struct {
		Data service.ZhipuSignStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Accounts, 2)
	require.Equal(t, int64(7), envelope.Data.Accounts[0].AccountID)
	require.Equal(t, 2, envelope.Data.Accounts[0].ConsecutiveFailures)
	require.False(t, envelope.Data.Accounts[1].RuntimeStateAvailable, "TODO(#24)：熔断状态未接线")
}

// 状态查询失败返回 500（与「没有启用账号」区分），不静默给出空列表。
func TestZhipuSignHandlerGetStatusMapsQueryFailureToInternalError(t *testing.T) {
	t.Parallel()

	stub := &zhipuSignHandlerServiceStub{statusErr: errors.New("list zhipu accounts: db down")}
	router := newZhipuSignHandlerRouter(stub)

	recorder := zhipuSignHandlerDo(router, http.MethodGet, "/api/v1/admin/zhipu/sign/status", "")

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, 1, stub.statusQueries)
}

// 未接线（构造缺服务）时三条路由都必须返回 503，而不是 panic。
func TestZhipuSignHandlerWithoutServiceReturnsServiceUnavailable(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	handler := NewZhipuSignHandler(nil)
	router := gin.New()
	router.GET("/api/v1/admin/zhipu/sign/config", handler.GetConfig)
	router.PUT("/api/v1/admin/zhipu/sign/config", handler.UpdateConfig)
	router.GET("/api/v1/admin/zhipu/sign/status", handler.GetStatus)

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/zhipu/sign/config"},
		{http.MethodPut, "/api/v1/admin/zhipu/sign/config"},
		{http.MethodGet, "/api/v1/admin/zhipu/sign/status"},
	} {
		recorder := zhipuSignHandlerDo(router, request.method, request.path, `{}`)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	}
}
