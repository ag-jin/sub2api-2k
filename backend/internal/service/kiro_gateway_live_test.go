package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestKiroGatewayForward_Live exercises the full gateway path against the real
// Kiro backend. It is skipped unless KIRO_LIVE=1 is set, and reads credentials
// from the existing kiro-go config (read-only).
func TestKiroGatewayForward_Live(t *testing.T) {
	if os.Getenv("KIRO_LIVE") != "1" {
		t.Skip("set KIRO_LIVE=1 to run live gateway test")
	}

	raw, err := os.ReadFile("/opt/kiro-go/data/config.json")
	if err != nil {
		t.Fatalf("read kiro-go config: %v", err)
	}
	var cfg struct {
		Accounts []map[string]any `json:"accounts"`
		ProxyURL string           `json:"proxyURL"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	acc := cfg.Accounts[0]

	// Build Account with kiro credentials (mirrors what admin UI would store).
	creds := map[string]any{
		"refreshToken": acc["refreshToken"],
		"accessToken":  acc["accessToken"],
		"clientId":     acc["clientId"],
		"clientSecret": acc["clientSecret"],
		"authMethod":   acc["authMethod"],
		"region":       acc["region"],
		"machineId":    acc["machineId"],
		"profileArn":   acc["profileArn"],
		"proxyUrl":     cfg.ProxyURL,
	}
	if exp, ok := acc["expiresAt"].(float64); ok {
		creds["expiresAt"] = time.Unix(int64(exp), 0).Format(time.RFC3339)
	}

	account := &Account{
		ID:          999001,
		Name:        "kiro-test",
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: creds,
	}

	svc := NewKiroGatewayService(nil)

	body := []byte(`{"model":"claude-opus-4-7","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Reply with exactly: GW_OK"}]}`)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))

	result, err := svc.Forward(context.Background(), c, account, body)
	if err != nil {
		t.Fatalf("Forward failed: %v", err)
	}

	if result == nil {
		t.Fatal("nil result")
	}
	t.Logf("RequestID=%s Model=%s UpstreamModel=%s InputTokens=%d OutputTokens=%d Duration=%v",
		result.RequestID, result.Model, result.UpstreamModel,
		result.Usage.InputTokens, result.Usage.OutputTokens, result.Duration)

	respBody := rec.Body.String()
	if !strings.Contains(respBody, "message_start") {
		t.Errorf("response missing message_start: %s", respBody[:min2(300, len(respBody))])
	}
	if !strings.Contains(respBody, "GW_OK") {
		t.Errorf("response missing expected text GW_OK; body sample: %s", respBody[:min2(500, len(respBody))])
	}
	if result.Usage.InputTokens <= 0 {
		t.Errorf("expected positive input tokens, got %d", result.Usage.InputTokens)
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
