package service

import "testing"

// TestDeepseekCrossPlatformAllowed verifies a deepseek account may join
// anthropic/kiro groups (Anthropic-protocol entry points) but not others, and
// that the existing same-platform / antigravity-mixed rules are unaffected.
func TestDeepseekCrossPlatformAllowed(t *testing.T) {
	svc := &GatewayService{}

	cases := []struct {
		name      string
		acctPlat  string
		groupPlat string
		useMixed  bool
		want      bool
	}{
		// deepseek cross-platform: allowed into anthropic & kiro groups
		{"deepseek→anthropic", PlatformDeepseek, PlatformAnthropic, false, true},
		{"deepseek→kiro", PlatformDeepseek, PlatformKiro, false, true},
		// deepseek NOT into openai/gemini/antigravity groups (different protocol)
		{"deepseek→openai", PlatformDeepseek, PlatformOpenAI, false, false},
		{"deepseek→gemini", PlatformDeepseek, PlatformGemini, false, false},
		{"deepseek→antigravity", PlatformDeepseek, PlatformAntigravity, false, false},
		// deepseek→deepseek same-platform still works
		{"deepseek→deepseek", PlatformDeepseek, PlatformDeepseek, false, true},
		// other platforms must NOT leak into anthropic/kiro via the new rule
		{"openai→anthropic blocked", PlatformOpenAI, PlatformAnthropic, false, false},
		{"gemini→kiro blocked", PlatformGemini, PlatformKiro, false, false},
		{"anthropic→kiro blocked", PlatformAnthropic, PlatformKiro, false, false},
		// claude account into its own anthropic group
		{"anthropic→anthropic", PlatformAnthropic, PlatformAnthropic, false, true},
		// kiro account into kiro group
		{"kiro→kiro", PlatformKiro, PlatformKiro, false, true},
		// deepseek cross rule independent of useMixed
		{"deepseek→anthropic mixed", PlatformDeepseek, PlatformAnthropic, true, true},
	}
	for _, c := range cases {
		acct := &Account{Platform: c.acctPlat}
		if got := svc.isAccountAllowedForPlatform(acct, c.groupPlat, c.useMixed); got != c.want {
			t.Errorf("%s: isAccountAllowedForPlatform(%s,%s,mixed=%v)=%v want %v",
				c.name, c.acctPlat, c.groupPlat, c.useMixed, got, c.want)
		}
	}

	// nil account is never allowed
	if svc.isAccountAllowedForPlatform(nil, PlatformAnthropic, false) {
		t.Error("nil account must not be allowed")
	}
}

// TestDeepseekModelSupportGuard verifies a deepseek account only matches deepseek
// models (never a claude request), regardless of model_mapping — the safety guard
// that makes cross-group routing correct.
func TestDeepseekModelSupportGuard(t *testing.T) {
	svc := &GatewayService{}
	deepseekAcct := &Account{Platform: PlatformDeepseek}

	// deepseek models → supported
	for _, m := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-pro-thinking", "DeepSeek-V4-Pro"} {
		if !svc.isModelSupportedByAccount(deepseekAcct, m) {
			t.Errorf("deepseek account should support %q", m)
		}
	}
	// claude/other models → NOT supported (would otherwise be wrongly picked in a kiro group)
	for _, m := range []string{"claude-opus-4-6", "claude-sonnet-4-5", "gpt-4", "gemini-3-pro", ""} {
		if svc.isModelSupportedByAccount(deepseekAcct, m) {
			t.Errorf("deepseek account must NOT support %q", m)
		}
	}
}

// TestOpenCodeCrossPlatformAllowed verifies an opencode account may join
// anthropic/kiro groups but not other platform groups.
func TestOpenCodeCrossPlatformAllowed(t *testing.T) {
	svc := &GatewayService{}

	cases := []struct {
		name      string
		groupPlat string
		useMixed  bool
		want      bool
	}{
		{"opencode→anthropic", PlatformAnthropic, false, true},
		{"opencode→kiro", PlatformKiro, false, true},
		{"opencode→openai", PlatformOpenAI, false, false},
		{"opencode→gemini", PlatformGemini, false, false},
		{"opencode→antigravity", PlatformAntigravity, false, false},
		{"opencode→opencode", PlatformOpenCode, false, true},
		{"opencode→anthropic mixed", PlatformAnthropic, true, true},
	}

	for _, c := range cases {
		acct := &Account{Platform: PlatformOpenCode}
		if got := svc.isAccountAllowedForPlatform(acct, c.groupPlat, c.useMixed); got != c.want {
			t.Errorf("%s: isAccountAllowedForPlatform(opencode,%s,mixed=%v)=%v want %v",
				c.name, c.groupPlat, c.useMixed, got, c.want)
		}
	}
}

// TestOpenCodeModelSupportGuard verifies an opencode account only matches GLM
// and deepseek models, never Claude or unrelated model families.
func TestOpenCodeModelSupportGuard(t *testing.T) {
	svc := &GatewayService{}
	openCodeAcct := &Account{Platform: PlatformOpenCode}

	for _, m := range []string{"glm-5.2", "glm-5.1", "glm-5", "deepseek-v4-pro", "DeepSeek-V4-Pro"} {
		if !svc.isModelSupportedByAccount(openCodeAcct, m) {
			t.Errorf("opencode account should support %q", m)
		}
	}

	for _, m := range []string{"claude-opus-4-6", "claude-sonnet-4-5", "gpt-4", "gemini-3-pro", ""} {
		if svc.isModelSupportedByAccount(openCodeAcct, m) {
			t.Errorf("opencode account must NOT support %q", m)
		}
	}
}
