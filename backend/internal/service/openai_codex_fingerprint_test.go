package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const testCodexFingerprintSeed = "11111111-1111-4111-8111-111111111111"

func newTestOAuthAccount(id int64, extra map[string]any) *Account {
	if codexFingerprintModeRequiresSeed(resolvedCodexFingerprintMode(PlatformOpenAI, AccountTypeOAuth, extra)) {
		if extra == nil {
			extra = make(map[string]any)
		}
		if _, exists := extra[codexFingerprintSeedExtraKey]; !exists {
			extra[codexFingerprintSeedExtraKey] = testCodexFingerprintSeed
		}
	}
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
}

// 默认开启后，新账号即使没有显式模式也必须落一个系统管理的 seed；
// 显式 off 与 API-key 账号不落。
// 存量账号无 seed 且管理员编辑账号时，更新路径必须持久化与运行期一致的
// 命名空间派生 seed，而不是随机值（否则身份永久轮换）。
func TestPrepareCodexFingerprintExtraForUpdatePersistsNamespaceDerivedSeed(t *testing.T) {
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "chatgpt-account",
			"chatgpt_user_id":    "user-1",
		},
	}
	namespace := codexAccountIdentityNamespace(account)
	require.NotEmpty(t, namespace)

	updated := prepareCodexFingerprintExtraForUpdate(account, nil)
	seed, ok := codexFingerprintSeed(updated)
	require.True(t, ok, "更新路径必须落库 seed")
	assert.Equal(t, deriveStableUUIDv4("sub2api:codex-fingerprint-seed:v1:"+namespace), seed,
		"seed 必须是运行期同款命名空间派生值")

	// 再次编辑时原样保留，不能轮换。
	account.Extra = updated
	again := prepareCodexFingerprintExtraForUpdate(account, updated)
	assert.Equal(t, seed, again[codexFingerprintSeedExtraKey])
}

func TestPrepareCodexFingerprintExtraForUpdateRandomSeedWithoutNamespace(t *testing.T) {
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	updated := prepareCodexFingerprintExtraForUpdate(account, nil)
	seed, ok := codexFingerprintSeed(updated)
	require.True(t, ok)
	require.NotEmpty(t, seed)
}

func TestPrepareCodexFingerprintExtraForCreateDefaultsDeviceSeed(t *testing.T) {
	oauthExtra := prepareCodexFingerprintExtraForCreate(PlatformOpenAI, AccountTypeOAuth, nil)
	require.NotNil(t, oauthExtra)
	seed, ok := codexFingerprintSeed(oauthExtra)
	require.True(t, ok)
	require.NotEmpty(t, seed)

	setupExtra := prepareCodexFingerprintExtraForCreate(PlatformOpenAI, AccountTypeSetupToken, nil)
	_, ok = codexFingerprintSeed(setupExtra)
	require.True(t, ok)

	offExtra := prepareCodexFingerprintExtraForCreate(PlatformOpenAI, AccountTypeOAuth, map[string]any{codexFingerprintModeExtraKey: "off"})
	_, ok = codexFingerprintSeed(offExtra)
	require.False(t, ok, "显式 off 不应创建 seed")

	apiKeyExtra := prepareCodexFingerprintExtraForCreate(PlatformOpenAI, AccountTypeAPIKey, nil)
	_, ok = codexFingerprintSeed(apiKeyExtra)
	require.False(t, ok, "API-key 账号不参与指纹收敛")
}

// --- deriveStableUUIDv4 ---

func TestDeriveStableUUIDv4_Deterministic(t *testing.T) {
	a := deriveStableUUIDv4("test-seed-1")
	b := deriveStableUUIDv4("test-seed-1")
	assert.Equal(t, a, b, "同一种子应返回相同结果")
}

func TestDeriveStableUUIDv4_DifferentSeeds(t *testing.T) {
	a := deriveStableUUIDv4("seed-a")
	b := deriveStableUUIDv4("seed-b")
	assert.NotEqual(t, a, b, "不同种子应返回不同结果")
}

func TestDeriveStableUUIDv4_ValidFormat(t *testing.T) {
	result := deriveStableUUIDv4("test-seed")
	parsed, err := uuid.Parse(result)
	require.NoError(t, err, "应返回合法 UUID 格式")
	assert.Equal(t, uuid.Version(4), parsed.Version(), "应为 UUIDv4")
	assert.Equal(t, uuid.RFC4122, parsed.Variant(), "应为 RFC4122 变体")
}

// --- GetCodexFingerprintMode ---

func TestGetCodexFingerprintMode(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected codexFingerprintMode
	}{
		{"nil 账号", nil, codexFingerprintOff},
		{"非 OAuth 账号", &Account{Platform: PlatformOpenAI, Type: "api_key"}, codexFingerprintOff},
		{"OpenAI setup token", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Extra: map[string]any{codexFingerprintModeExtraKey: "session"}}, codexFingerprintSession},
		{"OpenAI setup token 默认开启", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, codexFingerprintDevice},
		{"Anthropic setup token", &Account{Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Extra: map[string]any{codexFingerprintModeExtraKey: "session"}}, codexFingerprintOff},
		// 默认开启：OAuth / setup-token 缺省为 device（只收敛设备标识并补齐缺失头），
		// 显式 off 仍是唯一关闭方式。
		{"无 extra 默认 device", newTestOAuthAccount(1, nil), codexFingerprintDevice},
		{"空值默认 device", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: ""}), codexFingerprintDevice},
		{"非法值默认 device", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "invalid"}), codexFingerprintDevice},
		{"显式 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"}), codexFingerprintOff},
		{"device", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "device"}), codexFingerprintDevice},
		{"session", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"}), codexFingerprintSession},
		{"full", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "full"}), codexFingerprintFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.account.GetCodexFingerprintMode())
		})
	}
}

// --- resolveConvergedInstallationID ---

func TestResolveConvergedInstallationID_UsesDeviceID(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{"openai_device_id": "real-device-id"})
	assert.Equal(t, "real-device-id", resolveConvergedInstallationID(account, testCodexFingerprintSeed))
}

func TestResolveConvergedInstallationID_DerivesFromSeed(t *testing.T) {
	account := newTestOAuthAccount(42, nil)
	result := resolveConvergedInstallationID(account, testCodexFingerprintSeed)
	_, err := uuid.Parse(result)
	require.NoError(t, err, "派生值应为合法 UUID")
	assert.Equal(t, result, resolveConvergedInstallationID(account, testCodexFingerprintSeed), "确定性")
}

func TestResolveConvergedInstallationID_DifferentSeeds(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	a := resolveConvergedInstallationID(account, testCodexFingerprintSeed)
	b := resolveConvergedInstallationID(account, "22222222-2222-4222-8222-222222222222")
	assert.NotEqual(t, a, b)
}

// --- resolveConvergedThreadID ---

func TestResolveConvergedThreadID_PerClientSession(t *testing.T) {
	a := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	b := resolveConvergedThreadID(testCodexFingerprintSeed, "session-bbb")
	assert.NotEqual(t, a, b, "不同客户端 session 应得到不同 thread_id")
}

func TestResolveConvergedThreadID_Deterministic(t *testing.T) {
	a := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	b := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	assert.Equal(t, a, b, "同一客户端 session 应得到相同 thread_id")
}

func TestResolveConvergedThreadID_EmptySession(t *testing.T) {
	assert.Equal(t, "", resolveConvergedThreadID(testCodexFingerprintSeed, ""))
}

// --- off 模式：resolveCodexFingerprintIDsFromRequest 返回 nil ---

func TestResolveCodexFingerprintIDsFromRequest_ExplicitOff(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	assert.Nil(t, ids, "显式 off 模式应返回 nil")
}

// 默认开启（device）：无 extra 的存量 OAuth 账号也收敛设备标识并补齐缺失头。
func TestResolveCodexFingerprintIDsFromRequest_DefaultIsDeviceOn(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids, "无 extra 应默认开启 device 收敛")
	assert.Equal(t, codexFingerprintDevice, ids.mode)
	assert.NotEmpty(t, ids.installationID)
}

// 存量账号没有系统管理的 seed 时，必须按稳定凭据命名空间派生，而不是退回 off
// 或使用部署相对的本地行 ID。
func TestResolveCodexFingerprintIDsFromRequest_DerivesSeedFromCredentialNamespace(t *testing.T) {
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{codexFingerprintModeExtraKey: "session"},
		Credentials: map[string]any{
			"chatgpt_account_id": "chatgpt-account",
			"chatgpt_user_id":    "user-1",
		},
	}
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids)
	assert.NotEmpty(t, ids.installationID)

	again := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, again)
	assert.Equal(t, ids.installationID, again.installationID, "派生 seed 必须稳定")
}

// 管理员显式 opt-in 的账号行为不变。
func TestResolveCodexFingerprintIDsFromRequest_ExplicitOptInHonored(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: mode})
			ids := resolveCodexFingerprintIDsFromRequest(account, nil)
			require.NotNil(t, ids, "显式配置必须生效")
			assert.Equal(t, codexFingerprintMode(mode), ids.mode)
			assert.NotEmpty(t, ids.installationID)
		})
	}
}

func TestResolveCodexFingerprintIDsFromRequest_EnabledModesRequireValidSeed(t *testing.T) {
	for _, tt := range []struct {
		name  string
		extra map[string]any
	}{
		{name: "missing", extra: map[string]any{codexFingerprintModeExtraKey: "device"}},
		{name: "missing with device override", extra: map[string]any{codexFingerprintModeExtraKey: "device", "openai_device_id": "real-device"}},
		{name: "blank", extra: map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: ""}},
		{name: "uppercase", extra: map[string]any{codexFingerprintModeExtraKey: "full", codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-AAAAAAAAAAAA"}},
		{name: "nil uuid", extra: map[string]any{codexFingerprintModeExtraKey: "device", codexFingerprintSeedExtraKey: "00000000-0000-0000-0000-000000000000"}},
		{name: "non string", extra: map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: 123}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: tt.extra}
			require.Nil(t, resolveCodexFingerprintIDsFromRequest(account, nil))
		})
	}
}

// --- applyCodexFingerprintHeaders: off 模式 ---

func TestApplyCodexFingerprintHeaders_OffMode(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-installation-id", "original-install-id")
	h.Set("x-codex-window-id", "original-window-id")

	applyCodexFingerprintHeaders(h, nil)

	assert.Equal(t, "original-install-id", h.Get("x-codex-installation-id"), "nil ids 不改写")
	assert.Equal(t, "original-window-id", h.Get("x-codex-window-id"), "nil ids 不改写")
}

// --- applyCodexFingerprintHeaders: device 模式 ---

func TestApplyCodexFingerprintHeaders_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","sandbox":"seccomp"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-window:0")
	h.Set("x-codex-turn-metadata", turnMetadata)

	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	applyCodexFingerprintHeaders(h, ids)

	assert.Equal(t, "converged-device", h.Get("x-codex-installation-id"), "installation_id 应收敛")
	assert.Equal(t, "user-window:0", h.Get("x-codex-window-id"), "device 模式不改写 window_id")

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "user-session", meta["session_id"], "device 模式不改写 session_id")
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

// --- applyCodexFingerprintHeaders: session 模式 ---

func TestApplyCodexFingerprintHeaders_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","thread_id":"user-thread","turn_id":"user-turn","window_id":"user-thread:0","sandbox":"seccomp","thread_source":"user"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-thread:0")
	h.Set("x-codex-turn-metadata", turnMetadata)
	h.Set("x-client-request-id", "user-thread")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	applyCodexFingerprintHeaders(h, ids)

	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account, seed)
	convergedSession := resolveConvergedSessionID(seed)
	convergedThread := resolveConvergedThreadID(seed, "client-session-aaa")

	assert.Equal(t, convergedInstall, h.Get("x-codex-installation-id"))
	assert.Equal(t, convergedSession, h.Get("session-id"))
	assert.Empty(t, h.Get("session_id"), "出站不再使用下划线 session_id")
	assert.Equal(t, convergedThread, h.Get("thread-id"))
	assert.Equal(t, convergedThread, h.Get("x-client-request-id"))
	assert.Equal(t, convergedThread+":0", h.Get("x-codex-window-id"),
		"window-id 必须是出站 thread-id 的 <thread>:0 形态")

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, convergedThread, meta["thread_id"])
	assert.NotEqual(t, "user-turn", meta["turn_id"], "turn_id 应被新生成的值替换")
	assert.Equal(t, "seccomp", meta["sandbox"], "sandbox 保留原样")
	assert.Equal(t, "user", meta["thread_source"], "thread_source 保留原样")
}

// 默认 device 模式：客户端完全没带编码身份头时，网关按账号命名空间确定性地
// 补齐官方 0.163 的连字符头名与 turn metadata。
func TestApplyCodexFingerprintHeaders_DefaultDeviceSynthesizesMissingIdentity(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	require.Equal(t, codexFingerprintDevice, account.GetCodexFingerprintMode())

	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids)

	h := make(http.Header)
	applyCodexFingerprintHeaders(h, ids)

	assert.Equal(t, ids.installationID, h.Get("x-codex-installation-id"))
	require.NotEmpty(t, h.Get("session-id"))
	require.NotEmpty(t, h.Get("thread-id"))
	assert.Equal(t, h.Get("thread-id"), h.Get("x-client-request-id"), "x-client-request-id 必须与 thread-id 同值")
	assert.Empty(t, h.Get("session_id"), "出站头名统一为连字符形态")

	_, err := uuid.Parse(h.Get("session-id"))
	require.NoError(t, err)
	assert.Equal(t, h.Get("thread-id")+":0", h.Get("x-codex-window-id"),
		"window-id 必须是最终出站 thread-id 的 <thread>:0 形态")

	metadata := h.Get("x-codex-turn-metadata")
	require.NotEmpty(t, metadata, "缺失时网关必须合成最小 turn metadata")
	for _, field := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id"} {
		assert.NotEmpty(t, gjson.Get(metadata, field).String(), field)
	}
	assert.Equal(t, "turn", gjson.Get(metadata, "request_kind").String())
	assert.Equal(t, h.Get("session-id"), gjson.Get(metadata, "session_id").String())
	assert.Equal(t, h.Get("thread-id"), gjson.Get(metadata, "thread_id").String())
	assert.Equal(t, h.Get("x-codex-window-id"), gjson.Get(metadata, "window_id").String())

	again := make(http.Header)
	applyCodexFingerprintHeaders(again, ids)
	assert.Equal(t, h.Get("session-id"), again.Get("session-id"), "同一会话必须稳定")
	assert.Equal(t, h.Get("x-codex-window-id"), again.Get("x-codex-window-id"))
}

// 客户端带了标识时沿用现有账号级 scoped 逻辑：默认 device 模式只补缺失，
// 不得把客户端值替换成账号级常量。
func TestApplyCodexFingerprintHeaders_DefaultDeviceKeepsClientScopedIdentity(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session")
	clientHeaders.Set("thread-id", "client-thread")
	clientHeaders.Set("x-codex-window-id", "client-window")

	h := make(http.Header)
	// 真实请求构造器先写入隔离后的 session-id，再由账号 identity 收敛
	// thread-id / window-id。
	h.Set("session-id", isolateOpenAIUpstreamSessionID(7, account, "client-session"))
	h.Set("thread-id", "client-thread")
	h.Set("x-codex-window-id", "client-window")
	applyCodexAccountIdentityHeaders(h, account, 7)

	scopedSession := h.Get("session-id")
	scopedThread := h.Get("thread-id")
	scopedWindow := h.Get("x-codex-window-id")
	require.NotEqual(t, "client-session", scopedSession)
	require.True(t, strings.Contains(scopedSession, "-"), "session-id 应为 UUID 形态")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)
	applyCodexFingerprintHeaders(h, ids)

	assert.Equal(t, scopedSession, h.Get("session-id"), "客户端已带的 session 不得被账号常量覆盖")
	assert.Equal(t, scopedThread, h.Get("thread-id"))
	assert.Equal(t, scopedThread, h.Get("x-client-request-id"))
	assert.Equal(t, scopedWindow, h.Get("x-codex-window-id"), "客户端已带的 window 不得被覆盖")
	assert.NotEqual(t, ids.sessionID, h.Get("session-id"), "device 模式的 fallback 不应覆盖客户端值")
}

// --- session 模式：不同客户端得到不同 thread ---

func TestApplyCodexFingerprintHeaders_SessionMode_DifferentClients(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})

	makeTurnMeta := func() string {
		return `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`
	}

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDsFromRequest(account, clientA)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDsFromRequest(account, clientB)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("session-id"), hB.Get("session-id"), "session_id 应相同")
	assert.NotEqual(t, hA.Get("thread-id"), hB.Get("thread-id"), "不同客户端 thread_id 应不同")
	assert.NotEqual(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "不同客户端 window_id 应不同")
	assert.Equal(t, hA.Get("x-codex-installation-id"), hB.Get("x-codex-installation-id"))
}

// --- full 模式 ---

func TestApplyCodexFingerprintHeaders_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(seed)

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDsFromRequest(account, clientA)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDsFromRequest(account, clientB)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("thread-id"), hB.Get("thread-id"), "full 模式 thread_id 应相同")
	assert.Equal(t, convergedSession, hA.Get("thread-id"), "full 模式 thread_id 应等于 session_id")
	assert.Equal(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "full 模式 window_id 应相同")
}

// --- H1 修复验证：头和体的 turn_id 一致性 ---

func TestFingerprintIDs_HeaderAndBody_TurnID_Consistent(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-xyz")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	// 头改写
	h := http.Header{}
	h.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(h, ids)

	// 体改写（使用同一份 ids）
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "x",
			"session_id":              "x",
			"turn_id":                 "x",
			"x-codex-turn-metadata":   `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}
	applyCodexFingerprintClientMetadata(reqBody, ids)

	// 从头 turn-metadata JSON 提取 turn_id
	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	headerTurnID, ok := headerMeta["turn_id"].(string)
	require.True(t, ok, "头 turn-metadata 应包含 string 类型的 turn_id")

	// 从体 client_metadata 提取 turn_id
	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok, "请求体应包含 client_metadata")
	bodyTurnID, ok := cm["turn_id"].(string)
	require.True(t, ok, "体 client_metadata 应包含 string 类型的 turn_id")

	// 从体内嵌 turn-metadata JSON 提取 turn_id
	embeddedRaw, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok, "体 client_metadata 应包含 x-codex-turn-metadata 字符串")
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(embeddedRaw), &bodyMeta))
	bodyEmbeddedTurnID, ok := bodyMeta["turn_id"].(string)
	require.True(t, ok, "体内嵌 turn-metadata 应包含 string 类型的 turn_id")

	assert.Equal(t, headerTurnID, bodyTurnID, "头和体的 turn_id 必须一致")
	assert.Equal(t, headerTurnID, bodyEmbeddedTurnID, "头和体内嵌 turn-metadata 的 turn_id 必须一致")
	assert.Equal(t, ids.turnID, headerTurnID, "所有 turn_id 都应来自同一份 ids")
	assert.Equal(t, headerMeta["turn_started_at_unix_ms"], bodyMeta["turn_started_at_unix_ms"], "头和体的 timestamp 必须一致")
	assert.Equal(t, float64(ids.turnStartedAtUnixMs), headerMeta["turn_started_at_unix_ms"])
}

func TestFingerprintIDs_MalformedEmbeddedMetadataRebuiltConsistently(t *testing.T) {
	account := newTestOAuthAccount(2, map[string]any{codexFingerprintModeExtraKey: "session"})
	clientHeaders := make(http.Header)
	clientHeaders.Set("session-id", "client-session-malformed")
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	h := make(http.Header)
	h.Set("x-codex-turn-metadata", "{malformed")
	applyCodexFingerprintHeaders(h, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "client-session-malformed",
			"x-codex-turn-metadata": "[malformed",
		},
	}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))

	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	clientMetadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	bodyRaw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(bodyRaw), &bodyMeta))

	for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id", "turn_started_at_unix_ms"} {
		assert.Equal(t, headerMeta[key], bodyMeta[key], "rebuilt metadata field %s must match", key)
	}
}

// 客户端已声明的 request_kind（compaction/prewarm/memory）是有效语义，
// session/full 模式只能补缺失，不能改写成 turn。
func TestFingerprintIDs_SessionModePreservesClientRequestKind(t *testing.T) {
	account := newTestOAuthAccount(2, map[string]any{codexFingerprintModeExtraKey: "session"})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-request-kind")
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	const declared = `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0","request_kind":"compaction","sandbox":"seccomp"}`
	h := make(http.Header)
	h.Set("x-codex-turn-metadata", declared)
	applyCodexFingerprintHeaders(h, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "client-session-request-kind",
			"x-codex-turn-metadata": declared,
		},
	}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))

	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	assert.Equal(t, "compaction", headerMeta["request_kind"], "客户端声明的 request_kind 不得被改写")

	clientMetadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	var bodyMeta map[string]any
	embeddedBodyMetadata, ok := clientMetadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(embeddedBodyMetadata), &bodyMeta))
	assert.Equal(t, "compaction", bodyMeta["request_kind"], "body 内嵌 request_kind 不得被改写")
}

// session/full 模式下，出站头的 turn-metadata 与 body client_metadata 内嵌值
// 必须逐字段一致（同源生成，含 request_kind 的补齐语义）。
func TestFingerprintIDs_HeaderAndBodyEmbeddedMetadataParity(t *testing.T) {
	account := newTestOAuthAccount(3, map[string]any{codexFingerprintModeExtraKey: "full"})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-parity")
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	// 客户端未声明 request_kind；两侧都应补成 turn。
	const embedded = `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0","sandbox":"seatbelt"}`
	h := make(http.Header)
	h.Set("x-codex-turn-metadata", embedded)
	applyCodexFingerprintHeaders(h, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "client-session-parity",
			"x-codex-turn-metadata": embedded,
		},
	}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))

	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	clientMetadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	var bodyMeta map[string]any
	embeddedBodyMetadata, ok := clientMetadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(embeddedBodyMetadata), &bodyMeta))

	assert.Equal(t, "turn", headerMeta["request_kind"])
	assert.Equal(t, headerMeta, bodyMeta, "头与 body 内嵌 turn-metadata 必须逐字段一致")
}

// 本地行 ID 是部署相对的，不得参与上游身份种子：两个本地 ID 不同但凭据
// 相同的账号必须收敛到同一 installation/session。
func TestResolveCodexFingerprintIDs_SameCredentialsIgnoreLocalRowID(t *testing.T) {
	makeAccount := func(id int64) *Account {
		return &Account{
			ID:       id,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{codexFingerprintModeExtraKey: "session"},
			Credentials: map[string]any{
				"chatgpt_account_id": "shared-chatgpt-account",
				"chatgpt_user_id":    "shared-user",
			},
		}
	}

	idsA := resolveCodexFingerprintIDsFromRequest(makeAccount(1), nil)
	idsB := resolveCodexFingerprintIDsFromRequest(makeAccount(999), nil)
	require.NotNil(t, idsA)
	require.NotNil(t, idsB)
	assert.Equal(t, idsA.installationID, idsB.installationID, "installation-id 只能由凭据命名空间派生")
	assert.Equal(t, idsA.sessionID, idsB.sessionID)
}

// 客户端只有 prompt_cache_key 时，device 模式的 thread/window 必须按该
// 会话跨请求稳定（不能每条请求换一个窗口）。
func TestResolveCodexFingerprintIDsFromRequestWithFallback_PromptCacheKeyOnlyStable(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	require.Equal(t, codexFingerprintDevice, account.GetCodexFingerprintMode())

	first := resolveCodexFingerprintIDsFromRequestWithFallback(account, nil, "cache-key-only")
	second := resolveCodexFingerprintIDsFromRequestWithFallback(account, nil, "cache-key-only")
	other := resolveCodexFingerprintIDsFromRequestWithFallback(account, nil, "cache-key-other")
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.NotNil(t, other)

	assert.Equal(t, first.threadID, second.threadID, "同一 prompt_cache_key 的 thread 必须稳定")
	assert.Equal(t, first.windowID, second.windowID, "同一 prompt_cache_key 的 window 必须稳定")
	assert.Equal(t, first.threadID+":0", first.windowID)
	assert.NotEqual(t, first.threadID, other.threadID, "不同会话应得到不同 thread")
}

// 默认 device 模式 + 客户端只在 body client_metadata.session_id 里提供会话：
// 出站 session-id 头必须回落到 body 值，与 body / prompt_cache_key 同串。
func TestBuildUpstreamRequest_DefaultDeviceBodyOnlySessionMatchesHeader(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2101, nil)
	c := newFingerprintStageTestContext(t)

	body := map[string]any{
		"model":            "gpt-5.6-sol",
		"prompt_cache_key": "body-only-session",
		"client_metadata":  map[string]any{"session_id": "body-only-session"},
		"input":            []any{},
	}
	// 复刻 Forward：先做账号级 scoping，再解析/应用指纹；body 原始值作为 fallback。
	applyCodexAccountIdentityClientMetadataMap(body, account, 0)
	ids := resolveCodexFingerprintIDsFromRequestWithFallback(account, c.Request.Header, "body-only-session")
	require.NotNil(t, ids)
	require.True(t, applyCodexFingerprintClientMetadata(body, ids))
	stageCodexFingerprintIDs(c, ids)

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := svc.buildUpstreamRequest(context.Background(), c, account, raw, "test-token", true, "", false)
	require.NoError(t, err)

	clientMetadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	bodySession := clientMetadata["session_id"]
	require.NotEmpty(t, bodySession)
	assert.NotEqual(t, ids.sessionID, req.Header.Get("session-id"), "body 已提供会话时不得退回账号级常量")
	assert.Equal(t, bodySession, req.Header.Get("session-id"), "出站头 session-id 必须与 body client_metadata.session_id 同串")
	assert.Equal(t, bodySession, body["prompt_cache_key"], "prompt_cache_key 必须与 body session 同串")
	assert.Equal(t, req.Header.Get("thread-id")+":0", req.Header.Get("x-codex-window-id"))
}

// 客户端同时带不同值的 thread-id 与 x-client-request-id 时，入站
// x-client-request-id 必须在收敛前删除，最终恒等于出站 thread-id。
func TestBuildUpstreamRequest_ClientRequestIDConvergesToThreadID(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2102, nil)
	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("thread-id", "client-thread")
	c.Request.Header.Set("x-client-request-id", "different-client-request-id")

	ids := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	req, err := svc.buildUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-5.6-sol","input":[]}`), "test-token", true, "", false)
	require.NoError(t, err)

	require.NotEmpty(t, req.Header.Get("thread-id"))
	require.Equal(t, req.Header.Get("thread-id"), req.Header.Get("x-client-request-id"),
		"x-client-request-id 必须收敛为最终 thread-id")
	assert.NotEqual(t, "different-client-request-id", req.Header.Get("x-client-request-id"))
}

// 真实透传构造器上的默认 device 模式头体一致性：session-id 头、body
// client_metadata.session_id、prompt_cache_key 三处必须同串，installation
// 也必须与体内映身份一致。
func TestBuildUpstreamRequestOpenAIPassthrough_DefaultDeviceHeaderBodyParity(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2103, map[string]any{"openai_oauth_passthrough": true})
	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session-id", "client-session-device")

	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"client-session-device","client_metadata":{"session_id":"client-session-device"},"input":[],"stream":true}`)
	scopedBody, _, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 0)
	require.NoError(t, err)

	ids := resolveCodexFingerprintIDsFromRequestWithFallback(account, c.Request.Header, "client-session-device")
	require.NotNil(t, ids)
	fpBody, changed, err := applyCodexFingerprintClientMetadataRaw(scopedBody, ids)
	require.NoError(t, err)
	require.True(t, changed)
	stageCodexFingerprintIDs(c, ids)

	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, fpBody, "test-token")
	require.NoError(t, err)

	bodySession := gjson.GetBytes(fpBody, "client_metadata.session_id").String()
	require.NotEmpty(t, bodySession)
	assert.Equal(t, bodySession, req.Header.Get("session-id"), "session-id 头必须与 body 同串")
	assert.Equal(t, bodySession, gjson.GetBytes(fpBody, "prompt_cache_key").String(), "prompt_cache_key 必须与 body 同串")
	assert.Equal(t, gjson.GetBytes(fpBody, "client_metadata.x-codex-installation-id").String(),
		req.Header.Get("x-codex-installation-id"), "installation-id 头体必须一致")
	assert.Equal(t, req.Header.Get("thread-id")+":0", req.Header.Get("x-codex-window-id"))
}

// version 头删除只收敛到 Codex 协议账号；API-key / 第三方兼容上游仍允许
// 管理员通过头覆写配置 version。
func TestBuildUpstreamRequest_APIKeyPreservesVersionHeaderOverride(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       2104,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":                 "sk-test",
			"header_override_enabled": true,
			"header_overrides":        map[string]any{"version": "2026-01-01"},
		},
	}
	c := newFingerprintStageTestContext(t)

	req, err := svc.buildUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-5.6-sol","input":[]}`), "sk-test", true, "", false)
	require.NoError(t, err)
	// 覆写表以 wire casing 直写 http.Header（见 ApplyHeaderOverrides/resolveWireCasing），
	// "version" 不在 headerWireCasing 表中，键保持小写；Header.Get 的 canonical
	// 查找（"Version"）会 miss，故按大小写不敏感方式取实际出站值。
	assert.Equal(t, "2026-01-01", headerValueFold(req.Header, "version"),
		"非 Codex 协议上游必须保留管理员 version 头覆写")
}

// --- applyCodexFingerprintClientMetadata ---

func TestApplyCodexFingerprintClientMetadata_OffMode(t *testing.T) {
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original",
		},
	}
	modified := applyCodexFingerprintClientMetadata(reqBody, nil)
	assert.False(t, modified, "nil ids 不改写")
}

func TestApplyCodexFingerprintClientMetadata_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"user-session","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "user-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "converged-device", cm["x-codex-installation-id"])
	assert.Equal(t, "user-session", cm["session_id"], "device 模式不改 session_id")

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "original-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account, seed)
	convergedSession := resolveConvergedSessionID(seed)
	convergedThread := resolveConvergedThreadID(seed, "client-session-aaa")

	assert.Equal(t, convergedInstall, cm["x-codex-installation-id"])
	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedThread, cm["thread_id"])
	assert.Equal(t, convergedThread+":0", cm["x-codex-window-id"],
		"window_id 必须是出站 thread_id 的 <thread>:0 形态")

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "any-client")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "x",
			"thread_id":             "x",
			"x-codex-turn-metadata": `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(seed)

	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedSession, cm["thread_id"], "full 模式 thread_id 应等于 session_id")
}

// --- extractClientSessionID ---

func TestExtractClientSessionID(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		expected string
	}{
		{"连字符形式优先", func() http.Header {
			h := http.Header{}
			h.Set("session-id", "hyphen-form")
			h.Set("session_id", "underscore-form")
			return h
		}(), "hyphen-form"},
		{"回退到下划线形式", func() http.Header {
			h := http.Header{}
			h.Set("session_id", "underscore-form")
			return h
		}(), "underscore-form"},
		{"回退到老版本 conversation_id", func() http.Header {
			h := http.Header{}
			h.Set("conversation_id", "legacy-conversation")
			return h
		}(), "legacy-conversation"},
		{"都没有", http.Header{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractClientSessionID(tt.headers))
		})
	}
}

// --- 透传路径：raw 字节版 client_metadata 改写 ---

// rawVsMapClientMetadata 用同一份 ids 分别跑 map 版与 raw 字节版，
// 返回两侧最终的 client_metadata 解码结果。
func rawVsMapClientMetadata(t *testing.T, body []byte, ids *codexFingerprintIDs) (map[string]any, map[string]any) {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	applyCodexFingerprintClientMetadata(decoded, ids)
	mapCM, _ := decoded["client_metadata"].(map[string]any)

	rawBody, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	var rawDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	rawCM, _ := rawDecoded["client_metadata"].(map[string]any)
	return mapCM, rawCM
}

func cloneCodexFingerprintIDsForTest(ids *codexFingerprintIDs) *codexFingerprintIDs {
	if ids == nil {
		return nil
	}
	cloned := *ids
	cloned.originalBodySessionID = ""
	cloned.originalBodySessionIDCaptured = false
	return &cloned
}

func applyMapAndRawFingerprintBodiesForTest(t *testing.T, body []byte, ids *codexFingerprintIDs) (map[string]any, map[string]any) {
	t.Helper()

	mapIDs := cloneCodexFingerprintIDsForTest(ids)
	rawIDs := cloneCodexFingerprintIDsForTest(ids)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	applyCodexFingerprintClientMetadata(decoded, mapIDs)

	rawBody, _, err := applyCodexFingerprintClientMetadataRaw(body, rawIDs)
	require.NoError(t, err)
	var rawDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	return decoded, rawDecoded
}

func TestApplyCodexFingerprintPromptCacheKey_MapRawEquivalence(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode)+"/default", func(t *testing.T) {
			account := newTestOAuthAccount(4300, map[string]any{codexFingerprintModeExtraKey: string(mode)})
			ids := resolveCodexFingerprintIDs(account, "header-session", mode)
			require.NotNil(t, ids)

			body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"body-session","client_metadata":{"session_id":" body-session ","trace":"keep"},"input":[]}`)
			mapBody, rawBody := applyMapAndRawFingerprintBodiesForTest(t, body, ids)

			require.Equal(t, mapBody["prompt_cache_key"], rawBody["prompt_cache_key"])
			require.Equal(t, ids.sessionID, mapBody["prompt_cache_key"])
			mapCM, _ := mapBody["client_metadata"].(map[string]any)
			rawCM, _ := rawBody["client_metadata"].(map[string]any)
			require.Equal(t, ids.sessionID, mapCM["session_id"])
			require.Equal(t, mapCM["session_id"], rawCM["session_id"])
			require.Equal(t, "keep", rawCM["trace"])
		})
	}

	t.Run("explicit override", func(t *testing.T) {
		account := newTestOAuthAccount(4301, map[string]any{codexFingerprintModeExtraKey: "session"})
		ids := resolveCodexFingerprintIDs(account, "header-session", codexFingerprintSession)
		require.NotNil(t, ids)

		body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"explicit-cache","client_metadata":{"session_id":"body-session"},"input":[]}`)
		mapBody, rawBody := applyMapAndRawFingerprintBodiesForTest(t, body, ids)

		require.Equal(t, "explicit-cache", mapBody["prompt_cache_key"])
		require.Equal(t, "explicit-cache", rawBody["prompt_cache_key"])
		mapCM, _ := mapBody["client_metadata"].(map[string]any)
		rawCM, _ := rawBody["client_metadata"].(map[string]any)
		require.Equal(t, ids.sessionID, mapCM["session_id"])
		require.Equal(t, ids.sessionID, rawCM["session_id"])
	})
}

func TestApplyCodexFingerprintPromptCacheKey_Negatives(t *testing.T) {
	sessionAccount := newTestOAuthAccount(4310, map[string]any{codexFingerprintModeExtraKey: "session"})
	sessionIDs := resolveCodexFingerprintIDs(sessionAccount, "header-session", codexFingerprintSession)
	require.NotNil(t, sessionIDs)
	deviceAccount := newTestOAuthAccount(4311, map[string]any{codexFingerprintModeExtraKey: "device"})
	deviceIDs := resolveCodexFingerprintIDs(deviceAccount, "header-session", codexFingerprintDevice)
	require.NotNil(t, deviceIDs)

	tests := []struct {
		name          string
		body          []byte
		ids           *codexFingerprintIDs
		wantExists    bool
		wantCacheKey  any
		wantRawString string
	}{
		{
			name:       "missing key is not injected",
			body:       []byte(`{"client_metadata":{"session_id":"body-session"}}`),
			ids:        sessionIDs,
			wantExists: false,
		},
		{
			name:         "empty key preserved",
			body:         []byte(`{"prompt_cache_key":"","client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "",
		},
		{
			name:         "whitespace-different key is an explicit override",
			body:         []byte(`{"prompt_cache_key":" body-session ","client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: " body-session ",
		},
		{
			name:         "non-string key preserved",
			body:         []byte(`{"prompt_cache_key":123,"client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: float64(123),
		},
		{
			name:         "missing source metadata preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session"}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:         "non-string source session preserves key",
			body:         []byte(`{"prompt_cache_key":"123","client_metadata":{"session_id":123}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "123",
		},
		{
			name:         "non-object source metadata preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session","client_metadata":"bad"}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:         "device mode preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`),
			ids:          deviceIDs,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:          "off mode preserves body",
			body:          []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`),
			ids:           nil,
			wantExists:    true,
			wantCacheKey:  "body-session",
			wantRawString: `{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mapBody map[string]any
			require.NoError(t, json.Unmarshal(tt.body, &mapBody))
			changedMap := applyCodexFingerprintClientMetadata(mapBody, cloneCodexFingerprintIDsForTest(tt.ids))

			rawBody, changedRaw, err := applyCodexFingerprintClientMetadataRaw(tt.body, cloneCodexFingerprintIDsForTest(tt.ids))
			require.NoError(t, err)
			if tt.ids == nil {
				require.False(t, changedMap)
				require.False(t, changedRaw)
				require.JSONEq(t, tt.wantRawString, string(rawBody))
				return
			}
			require.True(t, changedMap)
			require.True(t, changedRaw)

			rawDecoded := map[string]any{}
			require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
			_, mapExists := mapBody["prompt_cache_key"]
			_, rawExists := rawDecoded["prompt_cache_key"]
			require.Equal(t, tt.wantExists, mapExists)
			require.Equal(t, tt.wantExists, rawExists)
			if tt.wantExists {
				require.Equal(t, tt.wantCacheKey, mapBody["prompt_cache_key"])
				require.Equal(t, tt.wantCacheKey, rawDecoded["prompt_cache_key"])
			}
		})
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_MatchesMapVariant(t *testing.T) {
	embedded := `{\"installation_id\":\"real-install\",\"session_id\":\"real-session\",\"sandbox\":\"seatbelt\"}`
	bodies := map[string]string{
		"no_client_metadata": `{"model":"gpt-5.6-sol","input":[],"stream":true}`,
		"object_with_extras": `{"model":"gpt-5.6-sol","client_metadata":{"session_id":"client-session","traceparent":"00-abc-def-01","x-codex-turn-metadata":"` + embedded + `"},"stream":true}`,
		"non_object_value":   `{"model":"gpt-5.6-sol","client_metadata":"bogus","stream":true}`,
	}
	for _, mode := range []codexFingerprintMode{codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull} {
		account := newTestOAuthAccount(4242, map[string]any{codexFingerprintModeExtraKey: string(mode)})
		ids := resolveCodexFingerprintIDs(account, "client-sess-raw", mode)
		require.NotNil(t, ids)
		for name, body := range bodies {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				mapCM, rawCM := rawVsMapClientMetadata(t, []byte(body), ids)
				assert.Equal(t, mapCM, rawCM, "raw 字节版与 map 版的 client_metadata 结果必须逐点一致")
			})
		}
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_PreservesUnrelatedFields(t *testing.T) {
	account := newTestOAuthAccount(4243, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-preserve", codexFingerprintSession)
	require.NotNil(t, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"hi"}],"stream":true,"prompt_cache_key":"pck-1"}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded))
	assert.Equal(t, "gpt-5.6-sol", decoded["model"])
	assert.Equal(t, "pck-1", decoded["prompt_cache_key"])
	assert.Equal(t, true, decoded["stream"])
	cm, _ := decoded["client_metadata"].(map[string]any)
	require.NotNil(t, cm)
	assert.Equal(t, ids.sessionID, cm["session_id"])
	assert.Equal(t, ids.turnID, cm["turn_id"])
}

func TestApplyCodexFingerprintClientMetadataRaw_Noop(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, body, out)

	out, changed, err = applyCodexFingerprintClientMetadataRaw(nil, &codexFingerprintIDs{mode: codexFingerprintSession, installationID: "x"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, out)
}

// --- context 暂存与出站头应用（透传/非透传共用 seam）---

func newFingerprintStageTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestStageCodexFingerprintIDs_NilOverwritesPreviousAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	accountA := newTestOAuthAccount(1001, map[string]any{codexFingerprintModeExtraKey: "session"})
	idsA := resolveCodexFingerprintIDs(accountA, "sess-x", codexFingerprintSession)
	require.NotNil(t, idsA)
	stageCodexFingerprintIDs(c, idsA)

	// failover 切到 off 模式账号：无条件覆写为 nil，上一账号 IDs 不得残留
	stageCodexFingerprintIDs(c, nil)

	h := http.Header{}
	h.Set("session-id", "isolated-session")
	accountB := newTestOAuthAccount(1002, map[string]any{"codex_fingerprint_mode": "off"})
	applyStagedCodexFingerprintHeaders(c, accountB, h)
	assert.Equal(t, "isolated-session", h.Get("session-id"), "off 账号不得应用上一账号的收敛 ID")
	assert.Empty(t, h.Get("x-codex-installation-id"))
}

func TestApplyStagedCodexFingerprintRejectsDifferentOAuthAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	accountA := newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: "session"})
	idsA := resolveCodexFingerprintIDs(accountA, "sess-a", codexFingerprintSession)
	require.NotNil(t, idsA)
	stageCodexFingerprintIDs(c, idsA)

	accountB := newTestOAuthAccount(1004, map[string]any{codexFingerprintModeExtraKey: "session"})
	h := make(http.Header)
	h.Set("session-id", "account-b-session")
	applyStagedCodexFingerprintHeaders(c, accountB, h)
	assert.Equal(t, "account-b-session", h.Get("session-id"))
	assert.Empty(t, h.Get("x-codex-installation-id"))

	body := map[string]any{"client_metadata": map[string]any{"session_id": "account-b-session"}}
	assert.False(t, applyStagedCodexFingerprintClientMetadata(c, accountB, body))
	clientMetadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "account-b-session", clientMetadata["session_id"])
}

func TestApplyStagedCodexFingerprintHeaders_SkipsNonOAuthAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	oauthIDs := resolveCodexFingerprintIDs(newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: "session"}), "sess-y", codexFingerprintSession)
	require.NotNil(t, oauthIDs)
	stageCodexFingerprintIDs(c, oauthIDs)

	h := http.Header{}
	apiKeyAccount := &Account{ID: 1004, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	applyStagedCodexFingerprintHeaders(c, apiKeyAccount, h)
	assert.Empty(t, h.Get("x-codex-installation-id"), "stale 收敛 ID 不得应用到非 OAuth 账号")
}

func TestBuildUpstreamRequestOpenAIPassthrough_AppliesStagedFingerprint(t *testing.T) {
	svc := &OpenAIGatewayService{}
	// 收敛是显式 opt-in（#5610）：显式开启后验证透传路径的出站头收敛。
	account := newTestOAuthAccount(2001, map[string]any{
		"openai_oauth_passthrough": true,
		"codex_fingerprint_mode":   "session",
	})

	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session_id", "real-client-session")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1 (Ubuntu 22.4.0; x86_64) xterm-256color")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set("x-codex-turn-metadata", `{"installation_id":"real-install","session_id":"real-session","sandbox":"seatbelt"}`)

	// 复刻 forwardOpenAIPassthrough 的解析+暂存 seam（默认 session 模式）
	ids := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[],"stream":true}`)
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)

	assert.Equal(t, ids.sessionID, req.Header.Get("session-id"), "session 模式下出站 session-id 应为账号级收敛值")
	assert.Empty(t, req.Header.Get("session_id"))
	assert.Equal(t, ids.installationID, req.Header.Get("x-codex-installation-id"))
	assert.Equal(t, ids.windowID, req.Header.Get("x-codex-window-id"))
	assert.Equal(t, ids.threadID, req.Header.Get("x-client-request-id"))
	turnMetadata := req.Header.Get("x-codex-turn-metadata")
	require.NotEmpty(t, turnMetadata)
	assert.Contains(t, turnMetadata, ids.sessionID, "turn-metadata JSON 中的 session_id 应被收敛")
	assert.Contains(t, turnMetadata, `"sandbox":"seatbelt"`, "turn-metadata 未指定字段应原样保留")
}

func TestBuildUpstreamRequestOpenAIPassthrough_OffModeKeepsIsolatedSession(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2002, map[string]any{
		"openai_oauth_passthrough": true,
		"codex_fingerprint_mode":   "off",
	})

	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session_id", "real-client-session")
	c.Request.Header.Set("originator", "codex_cli_rs")

	ids := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header)
	require.Nil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[],"stream":true}`)
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)

	assert.NotEmpty(t, req.Header.Get("session-id"))
	assert.Empty(t, req.Header.Get("session_id"))
	assert.NotEqual(t, resolveConvergedSessionID(testCodexFingerprintSeed), req.Header.Get("session-id"), "off 模式不得收敛 session-id")
	assert.Empty(t, req.Header.Get("x-codex-window-id"))
}

func TestApplyCodexFingerprintClientMetadataRaw_NonObjectBodyUntouched(t *testing.T) {
	account := newTestOAuthAccount(4244, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-nonobj", codexFingerprintSession)
	require.NotNil(t, ids)

	for _, body := range []string{`[1,2,3]`, `"plain string"`, `not json at all`} {
		out, changed, err := applyCodexFingerprintClientMetadataRaw([]byte(body), ids)
		require.NoError(t, err)
		assert.False(t, changed, "非 JSON 对象 body 不应被改写: %s", body)
		assert.Equal(t, []byte(body), out)
	}
}
