//go:build unit

package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CodeBuddy「API 线路」账号的测连通：GET {base_url}/models（Bearer=api_key），
// HTTP 200 即成功。腾讯 auth JSON 账号的 /v2/chat/completions 连通口径见
// account_test_service_codebuddy_test.go（回归基线，不随本改动变化）。

func TestAccountTestService_CodeBuddyAPIMode_ModelsProbe(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotAccept string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"deepseek-v4.1-flash","object":"model"}]}`)
	}))
	defer srv.Close()

	account := &Account{
		ID:       801,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-relay",
			"base_url": srv.URL + "/v1",
		},
	}

	svc := newCodeBuddyTestService(account)
	c, rec := newTestContext()
	require.NoError(t, svc.TestAccountConnection(c, account.ID, "", "hi", ""))

	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/v1/models", gotPath)
	assert.Equal(t, "Bearer sk-relay", gotAuth)
	assert.Equal(t, "application/json", gotAccept)

	success := false
	for _, ev := range parseCodeBuddyTestEvents(rec) {
		if ev.Type == "test_complete" {
			success = ev.Success
		}
	}
	assert.True(t, success, "GET /models 返回 200 应以 test_complete success 结束")
}

func TestAccountTestService_CodeBuddyAPIMode_ModelsProbeNon200Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	account := &Account{
		ID:       802,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-relay",
			"base_url": srv.URL + "/v1",
		},
	}

	svc := newCodeBuddyTestService(account)
	c, rec := newTestContext()
	_ = svc.TestAccountConnection(c, account.ID, "", "hi", "")

	out := rec.Body.String()
	assert.Contains(t, out, "returned 401", "应透出上游状态码")
	assert.Contains(t, out, "invalid api key", "应透出上游错误信息")
}
