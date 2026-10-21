package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveOpenAIWSSessionHeadersPrefersCodexHyphenHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("session-id", "codex-session")
	c.Request.Header.Set("session_id", "legacy-session")

	resolution := resolveOpenAIWSSessionHeaders(c, "prompt-cache")

	require.Equal(t, "codex-session", resolution.SessionID)
	require.Equal(t, "header_session-id", resolution.SessionSource)
}

func TestResolveOpenAIWSSessionHeadersFallsBackToLegacyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("session_id", "legacy-session")

	resolution := resolveOpenAIWSSessionHeaders(c, "prompt-cache")

	require.Equal(t, "legacy-session", resolution.SessionID)
	require.Equal(t, "header_session_id", resolution.SessionSource)
}

func TestResolveOpenAIWSSessionHeadersRecognizesHyphenConversationID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("conversation-id", "codex-conversation")

	resolution := resolveOpenAIWSSessionHeaders(c, "")

	require.Equal(t, "codex-conversation", resolution.SessionID)
	require.Equal(t, "header_conversation-id", resolution.SessionSource)
	require.Equal(t, "codex-conversation", resolution.ConversationID)
	require.Equal(t, "header_conversation-id", resolution.ConversationSource)
}

// API-key（非 Codex 协议）WS 账号维持改动前的下划线原值写法，
// Codex 协议账号才使用连字符头名。
func TestBuildOpenAIWSHeaders_APIKeyKeepsLegacyUnderscoreSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("session_id", "legacy-session")
	c.Request.Header.Set("conversation_id", "legacy-conversation")

	svc := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	headers, _, err := svc.buildOpenAIWSHeaders(
		context.Background(), c, account, "sk-test",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		false, "", "", "prompt-cache", "", "",
	)
	require.NoError(t, err)

	require.Equal(t, "legacy-session", headers.Get("session_id"))
	require.Equal(t, "legacy-conversation", headers.Get("conversation_id"))
	require.Empty(t, headers.Get("session-id"))
}
