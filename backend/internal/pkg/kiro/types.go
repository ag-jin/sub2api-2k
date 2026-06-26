package kiro

import (
	"crypto/rand"
	"math/big"
	"os"
	"strings"
	"time"
)

// KiroCredential represents a Kiro IDE credential (IdC or Social auth).
type KiroCredential struct {
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	AuthMethod   string `json:"authMethod,omitempty"` // "idc" or "social"
	// Provider mirrors the Kiro IDE / kam export field: BuilderId / Enterprise /
	// Github / Google. It determines profileArn resolution exactly like the IDE's
	// FixedProfileArns map (BuilderId/Github/Google use a fixed ARN and never call
	// ListAvailableProfiles; only Enterprise/IdC fetches it). Empty for legacy
	// accounts imported before this field existed.
	Provider     string `json:"provider,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Region       string `json:"region,omitempty"`
	AuthRegion   string `json:"authRegion,omitempty"`
	APIRegion    string `json:"apiRegion,omitempty"`
	MachineID    string `json:"machineId,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	ProxyURL     string `json:"proxyUrl,omitempty"`
}

// EffectiveAuthRegion returns the region to use for token refresh.
// Priority: credential.AuthRegion > credential.Region > "us-east-1"
func (c *KiroCredential) EffectiveAuthRegion() string {
	if c.AuthRegion != "" {
		return c.AuthRegion
	}
	if c.Region != "" {
		return c.Region
	}
	return "us-east-1"
}

// EffectiveAPIRegion returns the region to use for API calls.
// Mirrors kiro.rs: credential.api_region > config.api_region > config.region.
// Note kiro.rs intentionally does NOT fall back to credential.region here
// (that only affects auth/token-refresh region), so we don't either.
func (c *KiroCredential) EffectiveAPIRegion() string {
	if c.APIRegion != "" {
		return c.APIRegion
	}
	return "us-east-1"
}

// IsExpired checks if the token is expired (with 5-minute buffer).
func (c *KiroCredential) IsExpired() bool {
	if c.ExpiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		// Try alternate formats
		t, err = time.Parse("2006-01-02T15:04:05.000Z", c.ExpiresAt)
		if err != nil {
			return true
		}
	}
	return time.Now().Add(5 * time.Minute).After(t)
}

// IsExpiringSoon checks if the token expires within 10 minutes.
func (c *KiroCredential) IsExpiringSoon() bool {
	if c.ExpiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", c.ExpiresAt)
		if err != nil {
			return true
		}
	}
	return time.Now().Add(10 * time.Minute).After(t)
}

// IsIdC returns true if this credential uses IdC/Builder-ID/IAM authentication.
// Case-insensitive: kam exports authMethod as "IdC" (capitalized), so an exact
// lowercase compare would mis-classify kam-imported IdC accounts as social and
// route them through the wrong refresh endpoint.
func (c *KiroCredential) IsIdC() bool {
	m := strings.ToLower(strings.TrimSpace(c.AuthMethod))
	return m == "idc" || m == "builder-id" || m == "iam" ||
		(m == "" && c.ClientID != "" && c.ClientSecret != "")
}

// Provider constants mirror the Kiro IDE export values (and kam's export).
const (
	ProviderBuilderID  = "BuilderId"
	ProviderEnterprise = "Enterprise"
	ProviderGithub     = "Github"
	ProviderGoogle     = "Google"
)

// Fixed profileArns, copied verbatim from the Kiro IDE 0.12.316 FixedProfileArns
// map (extension.js). The IDE uses these for BuilderId / Github / Google and
// NEVER calls ListAvailableProfiles for those account types — replicating this
// is both a correctness fix (BuilderId has no permission to call
// ListAvailableProfiles → 403) and the IDE-accurate behavior.
const (
	BuilderIDProfileArn = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	SocialProfileArn    = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
)

// NormalizedProvider returns the canonical provider, deriving it when the
// explicit Provider field is absent (legacy accounts). Social auth → Google
// (shared social ARN); IdC without provider stays "" so the caller treats it as
// Enterprise (must fetch via ListAvailableProfiles).
func (c *KiroCredential) NormalizedProvider() string {
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "builderid":
		return ProviderBuilderID
	case "enterprise":
		return ProviderEnterprise
	case "github":
		return ProviderGithub
	case "google":
		return ProviderGoogle
	}
	// No explicit provider: infer from auth method. Social → shared social ARN.
	if !c.IsIdC() {
		return ProviderGoogle
	}
	return "" // IdC with unknown provider → treat as Enterprise (fetch profile)
}

// FixedProfileArn returns the IDE-fixed profileArn for this credential's
// provider, or "" if the provider requires fetching it via ListAvailableProfiles
// (Enterprise/IdC). Mirrors the IDE FixedProfileArns lookup.
func (c *KiroCredential) FixedProfileArn() string {
	switch c.NormalizedProvider() {
	case ProviderBuilderID:
		return BuilderIDProfileArn
	case ProviderGithub, ProviderGoogle:
		return SocialProfileArn
	}
	return ""
}

// TokenResult is the result of a token refresh operation.
type TokenResult struct {
	AccessToken  string
	RefreshToken string // may be rotated
	ProfileArn   string
	ExpiresIn    int64 // seconds
}

// Frame represents a parsed AWS Event Stream message frame.
type Frame struct {
	Headers map[string]string
	Payload []byte
}

// MessageType returns the :message-type header value.
func (f *Frame) MessageType() string {
	return f.Headers[":message-type"]
}

// EventType returns the :event-type header value.
func (f *Frame) EventType() string {
	return f.Headers[":event-type"]
}

// ContentType returns the :content-type header value.
func (f *Frame) ContentType() string {
	return f.Headers[":content-type"]
}

// AssistantResponseEvent is emitted for text content from the assistant.
type AssistantResponseEvent struct {
	Content string `json:"content"`
}

// ToolUseEvent is emitted for tool/function calls.
type ToolUseEvent struct {
	Name      string `json:"name"`
	ToolUseID string `json:"toolUseId"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`
}

// ContextUsageEvent contains context usage percentage.
type ContextUsageEvent struct {
	PercentageUsed float64 `json:"percentageUsed"`
}

// --- New-Kiro (0.12.x) client fingerprint values ---
//
// Defaults captured from a real Kiro IDE 0.12.301 session (us-east-1) on
// 2026-06-08 via mitmproxy. The generateAssistantResponse path and the
// getUsageLimits/setUserPreference path use DIFFERENT aws-sdk version strings.
//
// These are package vars (not const) so they can be overridden via environment
// variables WITHOUT a code change/recompile when Kiro ships a new version. The
// defaults below stay correct until Kiro updates; to follow a new release just
// capture the new fingerprint once and set the matching env vars on the server,
// then restart. Env wins; unset falls back to the default. See envOr* below.
var (
	// AwsSdkVersionAPI is the aws-sdk-js version on the generateAssistantResponse
	// (runtime.kiro.dev) path. Override: KIRO_AWS_SDK_VERSION_API.
	AwsSdkVersionAPI = envOr("KIRO_AWS_SDK_VERSION_API", "1.0.39")
	// AwsSdkVersionMgmt is the aws-sdk-js version on the management.kiro.dev path
	// (getUsageLimits / setUserPreference). Override: KIRO_AWS_SDK_VERSION_MGMT.
	AwsSdkVersionMgmt = envOr("KIRO_AWS_SDK_VERSION_MGMT", "1.0.0")
	// DefaultKiroAgentMode is the x-amzn-kiro-agent-mode header value. Confirmed
	// by decompiling Kiro 0.12.301 (extension.js): the default chat (Vibe) mode
	// sends "vibe" (agentMode = isSpec ? "spec" : "vibe"). "vibe" is the most
	// common value and matches this stateless gateway; kiro.rs long used it.
	// NOTE: distinct from the body's agentTaskType (AgentTaskType.VIBE="vibe").
	// Override: KIRO_AGENT_MODE.
	DefaultKiroAgentMode = envOr("KIRO_AGENT_MODE", "vibe")
)

// envOr returns the env var value if set (non-empty), else the default.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// KiroConfig holds configuration for the Kiro provider.
type KiroConfig struct {
	KiroVersion   string // e.g. "0.12.301"
	NodeVersion   string // e.g. "22.22.0"
	SystemVersion string // e.g. "darwin#25.3.0"
	AgentMode     string // x-amzn-kiro-agent-mode; defaults to DefaultKiroAgentMode
}

// EffectiveAgentMode returns the configured agent mode, or the default.
func (c KiroConfig) EffectiveAgentMode() string {
	if c.AgentMode != "" {
		return c.AgentMode
	}
	return DefaultKiroAgentMode
}

// systemVersions mirrors kiro.rs default_system_version: real Kiro IDE only
// runs on macOS/Windows, so we present one of these (never "linux") to keep the
// client fingerprint consistent with the official IDE. Versions kept roughly
// current (a real 0.12.301 session was observed on darwin#25.3.0).
//
// Override via KIRO_OS_VERSIONS (comma-separated, e.g. "darwin#25.4.0,win32#10.0.22631")
// so OS strings can be refreshed without a recompile when Kiro/OS versions move.
var systemVersions = parseOSVersions(envOr("KIRO_OS_VERSIONS", "darwin#25.3.0,win32#10.0.22631"))

// parseOSVersions splits a comma-separated OS-version list, trimming blanks.
// Falls back to a safe default if the input yields nothing.
func parseOSVersions(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []string{"darwin#25.3.0", "win32#10.0.22631"}
	}
	return out
}

// DefaultKiroConfig returns defaults matching the current Kiro IDE client
// fingerprint (versions kept in sync with kiro.rs model/config.rs).
//
// IMPORTANT (multi-account isolation): SystemVersion is left EMPTY here on
// purpose. In a multi-account gateway, pinning every account to one OS string
// (as a single-user kiro.rs instance does) would make all accounts share an
// identical os/ fingerprint - a strong account-farm signal. Instead the OS is
// resolved PER ACCOUNT and kept stable per account via
// KiroCredential.EffectiveSystemVersion, so different accounts present a
// realistic mix of macOS/Windows while each account stays consistent.
func DefaultKiroConfig() KiroConfig {
	return KiroConfig{
		KiroVersion:   envOr("KIRO_VERSION", "0.12.301"),
		NodeVersion:   envOr("KIRO_NODE_VERSION", "22.22.0"),
		SystemVersion: "", // resolved per-account; see EffectiveSystemVersion
		AgentMode:     "", // resolved via EffectiveAgentMode -> DefaultKiroAgentMode
	}
}

// EffectiveSystemVersion returns a per-account-stable OS string. If the config
// pins one explicitly it wins; otherwise the OS is derived deterministically
// from the account's fingerprint so it stays constant for that account across
// requests but varies between accounts.
func (c *KiroCredential) EffectiveSystemVersion(cfg KiroConfig) string {
	if cfg.SystemVersion != "" {
		return cfg.SystemVersion
	}
	seed := c.MachineID
	if seed == "" {
		seed = c.RefreshToken
	}
	if seed == "" {
		return systemVersions[0]
	}
	// stable hash -> index
	h := sha256Hex("KiroOS/" + seed)
	// use first hex byte as selector
	idx := int(h[0]) % len(systemVersions)
	return systemVersions[idx]
}


// randInt returns a uniform random int in [0, n).
func randInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}
