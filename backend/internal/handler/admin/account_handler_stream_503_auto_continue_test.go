package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountAdminBoundariesRejectMalformedOpenAIStream503AutoContinueValue(t *testing.T) {
	const malformedExtra = `"extra":{"openai_stream_503_auto_continue":"true"}`

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		mount  func(*gin.Engine, *AccountHandler)
		setup  func(*stubAdminService)
	}{
		{
			name:   "create",
			method: http.MethodPost,
			path:   "/accounts",
			body:   `{"name":"account","platform":"openai","type":"apikey","credentials":{"api_key":"test"},` + malformedExtra + `}`,
			mount:  func(router *gin.Engine, handler *AccountHandler) { router.POST("/accounts", handler.Create) },
		},
		{
			name:   "batch create",
			method: http.MethodPost,
			path:   "/accounts/batch",
			body:   `{"accounts":[{"name":"account","platform":"openai","type":"apikey","credentials":{"api_key":"test"},` + malformedExtra + `}]}`,
			mount:  func(router *gin.Engine, handler *AccountHandler) { router.POST("/accounts/batch", handler.BatchCreate) },
		},
		{
			name:   "apply oauth credentials",
			method: http.MethodPost,
			path:   "/accounts/1/apply-oauth-credentials",
			body:   `{"type":"oauth","credentials":{"access_token":"new-token"},` + malformedExtra + `}`,
			mount: func(router *gin.Engine, handler *AccountHandler) {
				router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
			},
			setup: func(stub *stubAdminService) {
				stub.getAccountResult = &service.Account{
					ID:       1,
					Platform: service.PlatformOpenAI,
					Type:     service.AccountTypeOAuth,
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			stub := newStubAdminService()
			if tt.setup != nil {
				tt.setup(stub)
			}
			handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			tt.mount(router, handler)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			request.Header.Set("Content-Type", "application/json")

			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			var responseBody struct {
				Reason string `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &responseBody))
			require.Equal(t, "OPENAI_STREAM_503_AUTO_CONTINUE_INVALID", responseBody.Reason)
			require.Zero(t, stub.updateAccountCalls)
			require.Zero(t, stub.updateAccountExtraCalls)
		})
	}
}

func TestAccountCreateBoundaryDoesNotApplyOpenAIStream503ValidationToOtherPlatforms(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts", handler.Create)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewBufferString(
		`{"name":"account","platform":"anthropic","type":"apikey","credentials":{"api_key":"test"},"extra":{"openai_stream_503_auto_continue":"provider-owned"}}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
}
