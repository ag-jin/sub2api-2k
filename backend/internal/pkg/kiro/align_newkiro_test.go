package kiro

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAlignNewKiro_0_12_301 verifies the conversation+usage request fingerprint
// matches a real Kiro IDE 0.12.301 session captured 2026-06-08 (us-east-1).
func TestAlignNewKiro_0_12_301(t *testing.T) {
	cfg := DefaultKiroConfig()
	cred := &KiroCredential{
		MachineID:  "6fbfde8f935fb057d5c2b4513369aa92f20baf00ed0023f123c74f3eaf8ad17c",
		APIRegion:  "us-east-1",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK",
	}
	ep := NewEndpoint(cfg)
	mid := cred.EffectiveMachineID()

	// --- conversation endpoint ---
	wantURL := "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"
	if got := ep.APIURL(cred); got != wantURL {
		t.Errorf("APIURL\n got=%s\nwant=%s", got, wantURL)
	}

	req := httptest.NewRequest(http.MethodPost, wantURL, strings.NewReader("{}"))
	req.Header = http.Header{} // reset so we only see what DecorateRequest sets
	ep.DecorateRequest(req, cred, "TESTTOKEN")
	h := req.Header
	checks := map[string]string{
		"content-type":                "application/json",
		"x-amzn-codewhisperer-optout": "true",
		"x-amzn-kiro-agent-mode":      "vibe",
		"host":                        "runtime.us-east-1.kiro.dev",
		"amz-sdk-request":             "attempt=1; max=3",
		"Connection":                  "close",
		"Authorization":               "Bearer TESTTOKEN",
	}
	for k, want := range checks {
		if got := h.Get(k); got != want {
			t.Errorf("header %s\n got=%q\nwant=%q", k, got, want)
		}
	}
	wantUA := "aws-sdk-js/1.0.39 ua/2.1 os/" + cred.EffectiveSystemVersion(cfg) +
		" lang/js md/nodejs#22.22.0 api/codewhispererstreaming#1.0.39 m/N KiroIDE-0.12.301-" + mid
	if got := h.Get("user-agent"); got != wantUA {
		t.Errorf("user-agent\n got=%s\nwant=%s", got, wantUA)
	}
	wantXUA := "aws-sdk-js/1.0.39 KiroIDE-0.12.301-" + mid
	if got := h.Get("x-amz-user-agent"); got != wantXUA {
		t.Errorf("x-amz-user-agent\n got=%s\nwant=%s", got, wantXUA)
	}
	if h.Get("amz-sdk-invocation-id") == "" {
		t.Error("amz-sdk-invocation-id missing")
	}

	t.Logf("conversation UA: %s", wantUA)
}
