package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexFingerprintIDsContextKey 是暂存在 gin context 的收敛 ID 集合键。
// 由 Forward（非透传）或 forwardOpenAIPassthrough（透传）解析后写入，请求
// 构造器读取用于出站头改写——请求体与出站头必须共享同一份 IDs，保证
// turn_id 等随机字段一致。
const codexFingerprintIDsContextKey = "codex_fingerprint_ids"

// stageCodexFingerprintIDs 将本 attempt 解析出的收敛 ID 暂存到 gin context。
// 必须无条件覆写（含 nil）：failover 从收敛账号切到 off 账号时，上一账号的
// IDs 不得残留并被误应用到新账号的出站头（typed-nil 由应用侧 nil 守卫吸收）。
func stageCodexFingerprintIDs(c *gin.Context, ids *codexFingerprintIDs) {
	if c != nil {
		c.Set(codexFingerprintIDsContextKey, ids)
	}
}

func stagedCodexFingerprintIDs(c *gin.Context, account *Account) *codexFingerprintIDs {
	if c == nil || account == nil || !account.UsesOpenAICodexProtocol() {
		return nil
	}
	value, ok := c.Get(codexFingerprintIDsContextKey)
	if !ok {
		return nil
	}
	ids, ok := value.(*codexFingerprintIDs)
	if !ok || ids == nil || ids.accountID != account.ID {
		return nil
	}
	return ids
}

// applyStagedCodexFingerprintHeaders 读取 context 暂存的收敛 ID 并改写出站头。
// 非透传与透传两个请求构造器共用本函数，防止应用语义漂移。仅解析该
// snapshot 的 OAuth 账号可读取，避免 stale context 跨账号 failover 泄漏。
func applyStagedCodexFingerprintHeaders(c *gin.Context, account *Account, h http.Header) {
	applyCodexFingerprintHeaders(h, stagedCodexFingerprintIDs(c, account))
}

func applyStagedCodexFingerprintClientMetadata(c *gin.Context, account *Account, reqBody map[string]any) bool {
	return applyCodexFingerprintClientMetadata(reqBody, stagedCodexFingerprintIDs(c, account))
}

// codexFingerprintMode 控制 OAuth 账号出站请求的设备指纹收敛强度。
// 多人共享同一 OAuth 账号时，每个用户的 Codex 客户端会携带各自不同的
// installation_id / session_id / thread_id，上游据此判定设备数和会话数。
// 收敛模式将这些标识改写为账号级恒定值，减少上游可见的设备/会话指纹。
type codexFingerprintMode string

const (
	// codexFingerprintOff 不做任何收敛，原样透传客户端标识。
	// 只由管理员显式配置 off 启用；OpenAI OAuth / setup-token 的缺省值是 device。
	codexFingerprintOff codexFingerprintMode = "off"
	// codexFingerprintDevice 收敛 installation_id 为账号级恒定值，并补齐客户端
	// 缺失的 session / thread / window / turn metadata 头；客户端已有的 session /
	// thread / window 保持账号级 scoped 派生，不折叠成账号常量。
	// 上游看到 1 台设备 + 多会话（每用户各自的 session）。
	codexFingerprintDevice codexFingerprintMode = "device"
	// codexFingerprintSession 收敛 installation_id + session_id，
	// thread_id 按客户端原始 session-id 确定性派生（每个真实 Codex 会话一个独立线程）。
	// 上游看到 1 台设备 + 1 会话 + N 线程，最接近正常用户 spawn 子代理的模式。
	codexFingerprintSession codexFingerprintMode = "session"
	// codexFingerprintFull 收敛所有标识：installation_id + session_id + thread_id。
	// 上游看到 1 台设备 + 1 会话 + 1 线程，最激进。
	codexFingerprintFull codexFingerprintMode = "full"
)

const (
	codexFingerprintModeExtraKey = "codex_fingerprint_mode"
	codexFingerprintSeedExtraKey = "codex_fingerprint_seed"
)

func canonicalCodexFingerprintSeed(value any) (string, bool) {
	raw, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(raw)
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil || trimmed != parsed.String() {
		return "", false
	}
	return trimmed, true
}

func newCodexFingerprintSeed() string {
	return uuid.NewString()
}

func stripCodexFingerprintSeed(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	stripped := maps.Clone(extra)
	delete(stripped, codexFingerprintSeedExtraKey)
	return stripped
}

func codexFingerprintModeFromExtra(extra map[string]any) codexFingerprintMode {
	mode, ok := explicitCodexFingerprintMode(extra)
	if !ok {
		return codexFingerprintOff
	}
	return mode
}

// explicitCodexFingerprintMode 只读取管理员显式写入的合法值；缺省/空/非法
// 返回 ok=false，由 resolvedCodexFingerprintMode 决定默认值。
func explicitCodexFingerprintMode(extra map[string]any) (codexFingerprintMode, bool) {
	if extra == nil {
		return codexFingerprintOff, false
	}
	raw, _ := extra[codexFingerprintModeExtraKey].(string)
	switch mode := codexFingerprintMode(strings.TrimSpace(raw)); mode {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return mode, true
	default:
		return codexFingerprintOff, false
	}
}

// defaultCodexFingerprintMode 是 OpenAI OAuth / setup-token 账号的缺省收敛模式。
//
// 默认 device：收敛每个账号的 installation_id（设备感），但保留客户端
// session-id / thread-id 的（账号, 客户端值）级确定性——默认模式不得把
// 所有客户端折叠成同一个账号级 session。session / full 仍是管理员显式 opt-in
// 的更强收敛。
func defaultCodexFingerprintMode(platform, accountType string) codexFingerprintMode {
	if platform == PlatformOpenAI && (accountType == AccountTypeOAuth || accountType == AccountTypeSetupToken) {
		return codexFingerprintDevice
	}
	return codexFingerprintOff
}

// resolvedCodexFingerprintMode 合并显式配置与平台默认值。
func resolvedCodexFingerprintMode(platform, accountType string, extra map[string]any) codexFingerprintMode {
	if mode, ok := explicitCodexFingerprintMode(extra); ok {
		return mode
	}
	return defaultCodexFingerprintMode(platform, accountType)
}

func codexFingerprintModeRequiresSeed(mode codexFingerprintMode) bool {
	switch mode {
	case codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return true
	default:
		return false
	}
}

func codexFingerprintSeed(extra map[string]any) (string, bool) {
	if extra == nil {
		return "", false
	}
	return canonicalCodexFingerprintSeed(extra[codexFingerprintSeedExtraKey])
}

// resolveCodexFingerprintSeed 返回账号可用的稳定 seed。
// 优先使用系统管理的 codex_fingerprint_seed；存量账号没有该键时，退回到
// 凭据命名空间（ChatGPT account/user 或 setup-token 指纹）派生的确定性 seed。
// 本地行 ID 是部署相对的，绝不能作为上游身份的种子。
func resolveCodexFingerprintSeed(account *Account) (string, bool) {
	if account == nil {
		return "", false
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		return seed, true
	}
	if namespace := codexAccountIdentityNamespace(account); namespace != "" {
		return deriveStableUUIDv4("sub2api:codex-fingerprint-seed:v1:" + namespace), true
	}
	return "", false
}

func prepareCodexFingerprintExtraForCreate(platform, accountType string, extra map[string]any) map[string]any {
	prepared := stripCodexFingerprintSeed(extra)
	// seed 只服务于 OpenAI OAuth / setup-token 的 Codex 指纹收敛。API-key 账号
	// 运行期 GetCodexFingerprintMode 恒为 off（不参与指纹），即使 extra 里显式
	// 写了 session/full 也不得铸造或持久化 seed——创建/复制路径一律 strip-only。
	if platform != PlatformOpenAI || (accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) {
		return prepared
	}
	if !codexFingerprintModeRequiresSeed(resolvedCodexFingerprintMode(platform, accountType, prepared)) {
		return prepared
	}
	if prepared == nil {
		prepared = make(map[string]any, 1)
	}
	prepared[codexFingerprintSeedExtraKey] = newCodexFingerprintSeed()
	return prepared
}

func prepareCodexFingerprintExtraForUpdate(account *Account, extra map[string]any) map[string]any {
	prepared := stripCodexFingerprintSeed(extra)
	if account == nil || !account.IsOpenAIOAuthLike() {
		return prepared
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		if prepared == nil {
			prepared = make(map[string]any, 1)
		}
		prepared[codexFingerprintSeedExtraKey] = seed
		return prepared
	}
	if codexFingerprintModeRequiresSeed(resolvedCodexFingerprintMode(account.Platform, account.Type, prepared)) {
		if prepared == nil {
			prepared = make(map[string]any, 1)
		}
		// 优先持久化与运行期完全一致的命名空间派生 seed：存量账号本来没有
		// 该键时，运行期会按凭据命名空间派生；若更新路径写入随机值，管理员
		// 任意一次编辑都会让 installation/thread/window 永久轮换。
		seed := ""
		if namespace := codexAccountIdentityNamespace(account); namespace != "" {
			seed = deriveStableUUIDv4("sub2api:codex-fingerprint-seed:v1:" + namespace)
		} else {
			seed = newCodexFingerprintSeed()
		}
		prepared[codexFingerprintSeedExtraKey] = seed
	}
	return prepared
}

func sanitizedCodexFingerprintExtraUpdates(updates map[string]any) map[string]any {
	if updates == nil {
		return nil
	}
	sanitized := maps.Clone(updates)
	delete(sanitized, codexFingerprintSeedExtraKey)
	return sanitized
}

// ShouldEnsureCodexFingerprintSeedForExtraUpdates reports whether a JSONB key-level
// extra update is enabling Codex fingerprint convergence and therefore must atomically
// preserve or create the system-managed per-account seed in the repository update.
func ShouldEnsureCodexFingerprintSeedForExtraUpdates(updates map[string]any) bool {
	if updates == nil {
		return false
	}
	return codexFingerprintModeRequiresSeed(codexFingerprintModeFromExtra(updates))
}

// GetCodexFingerprintMode 从账号 extra JSON 读取指纹收敛模式。
//
// OpenAI OAuth / setup-token 账号默认开启（device）：即使 DB 里的存量账号
// 完全没有这个 extra 键，也会收敛账号级 installation_id 并补齐缺失的
// session / thread / window / turn metadata 头。
//
// 显式 `off` 是唯一的关闭方式；`session` / `full` 仍可由管理员显式启用，
// 语义保持现状（账号级折叠 session）。API-key 账号不参与收敛。
//
// 历史：v0.1.175（#5553）曾把缺省值当作 session，把每个客户端的
// session / thread / turn / window 全部折叠，随后 #5555/#5556/#5582 报告额度
// 缩水；#5610 因此改成显式 opt-in。本次按用户决策恢复默认开启，但默认只到
// device 强度，避免再次折叠 session。
func (a *Account) GetCodexFingerprintMode() codexFingerprintMode {
	if a == nil || !a.IsOpenAIOAuthLike() {
		return codexFingerprintOff
	}
	return resolvedCodexFingerprintMode(a.Platform, a.Type, a.Extra)
}

// deriveStableUUIDv4 从种子确定性派生一个 UUIDv4 格式的字符串。
// 同一种子永远返回同一值。
func deriveStableUUIDv4(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

// resolveConvergedInstallationID 返回账号级恒定的 installation_id。
// 优先使用管理员配置的真实 device_id，无则从系统管理的账号随机种子确定性派生。
func resolveConvergedInstallationID(account *Account, seed string) string {
	if account == nil {
		return ""
	}
	if deviceID := account.GetOpenAIDeviceID(); deviceID != "" {
		return deviceID
	}
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-install-id:v2:" + seed)
}

// resolveConvergedSessionID 返回账号级恒定的 session_id。
func resolveConvergedSessionID(seed string) string {
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-session-id:v2:" + seed)
}

// resolveConvergedThreadID 按客户端原始 session-id 确定性派生 thread_id。
// 每个真实 Codex 会话（不同客户端启动实例）获得一个独立线程，
// 模拟正常用户 spawn 子代理或开多窗口的模式。
func resolveConvergedThreadID(seed, clientSessionID string) string {
	if seed == "" || clientSessionID == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-thread-id:v2:" + seed + ":" + clientSessionID)
}

// resolveConvergedWindowID 返回官方 `<thread-id>:<n>` 形态的 window_id。
// 仓库解析真实官方入站 window-id 时按 ":" 切分取 thread 段
// （openai_ws_execution_scope.go），因此 window 必须挂在出站 thread-id 上，
// 不能是独立 UUID。同一线程的多轮请求共享 `<thread>:0`。
func resolveConvergedWindowID(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}
	return threadID + ":0"
}

// codexFingerprintIDs 收敛后的完整 ID 集合。
// 由 resolveCodexFingerprintIDs 一次性生成，同一个实例在头改写和体改写之间共享，
// 确保所有载体中的 turn_id 等随机字段一致。体改写时还会补记原始
// client_metadata.session_id，用于识别 root prompt_cache_key 的默认值。
type codexFingerprintIDs struct {
	accountID                     int64
	mode                          codexFingerprintMode
	installationID                string
	sessionID                     string
	threadID                      string
	turnID                        string
	windowID                      string
	turnStartedAtUnixMs           int64
	originalBodySessionID         string
	originalBodySessionIDCaptured bool
	// clientSessionID 是解析时使用的原始客户端会话标识（session-id 头 →
	// body client_metadata.session_id → prompt_cache_key）。请求构造器用它
	// 作为出站 session-id 的 fallback，保证头 / body / prompt_cache_key 同串。
	clientSessionID string
}

// resolveCodexFingerprintIDs 按收敛模式计算出站 ID 集合。
// clientSessionID 是客户端原始的会话标识（session-id 头，或 body
// client_metadata.session_id / prompt_cache_key fallback），用于按会话派生
// thread_id / window_id——每个真实 Codex 会话得到一个独立线程；device 与
// session 模式都使用它，full 模式才折叠成账号级单一线程。
// 返回 nil 表示 off 模式，不需要改写。
// 注意：包含随机生成的 turn_id，调用方必须只调用一次并共享结果给头改写和体改写。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode) *codexFingerprintIDs {
	if account == nil || mode == codexFingerprintOff {
		return nil
	}
	seed, ok := resolveCodexFingerprintSeed(account)
	if !ok {
		return nil
	}

	ids := &codexFingerprintIDs{
		accountID:           account.ID,
		mode:                mode,
		turnStartedAtUnixMs: time.Now().UnixMilli(),
	}

	ids.installationID = resolveConvergedInstallationID(account, seed)
	if ids.installationID == "" {
		return nil
	}

	switch mode {
	case codexFingerprintDevice:
		// 默认模式只补齐缺失标识，不覆盖客户端已有值。这里的 session/thread/
		// window 是客户端完全没带时的确定性 fallback；客户端带了值时由上游
		// 账号级 scoped 逻辑先行写入，apply 阶段不会覆盖。
		ids.sessionID = resolveConvergedSessionID(seed)
		ids.threadID = resolveConvergedThreadID(seed, clientSessionID)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = resolveConvergedWindowID(ids.threadID)
		return ids

	case codexFingerprintSession:
		ids.sessionID = resolveConvergedSessionID(seed)
		ids.threadID = resolveConvergedThreadID(seed, clientSessionID)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = resolveConvergedWindowID(ids.threadID)
		return ids

	case codexFingerprintFull:
		ids.sessionID = resolveConvergedSessionID(seed)
		ids.threadID = ids.sessionID
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		// full 模式把会话折叠成账号级单一线程，window 也必须账号级恒定，
		// 否则上游仍能看到按客户端会话分裂的窗口数。
		ids.windowID = resolveConvergedWindowID(ids.threadID)
		return ids
	}

	return nil
}

// extractClientSessionID 从请求头中提取客户端原始的会话标识。
// 优先取 session-id（连字符形式，Codex CLI 标准），回退到 session_id
// 与老版本的 conversation_id。返回的值尚未被隔离逻辑改写，是客户端的真实标识。
func extractClientSessionID(h http.Header) string {
	return firstNonEmptyHeader(h, "session-id", "session_id", "conversation-id", "conversation_id")
}

// resolveCodexFingerprintIDsFromRequest 从客户端原始请求头中提取 session-id，
// 结合账号配置一次性解析收敛 ID 集合。调用方应将返回的 ids 同时传给
// applyCodexFingerprintHeaders 和 applyCodexFingerprintClientMetadata。
func resolveCodexFingerprintIDsFromRequest(account *Account, clientHeaders http.Header) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsFromRequestWithFallback(account, clientHeaders, "")
}

// resolveCodexFingerprintIDsFromRequestWithFallback 在客户端没有显式 session 头时，
// 允许调用方把 body 的 client_metadata.session_id 或 prompt_cache_key 作为会话
// fallback，确保默认 device 模式合成的 thread/window 仍按会话（而不是所有请求
// 共享账号级窗口）稳定，并让请求构造器复用同一个原始会话值。
func resolveCodexFingerprintIDsFromRequestWithFallback(account *Account, clientHeaders http.Header, fallbackSessionID string) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	clientSessionID := ""
	if clientHeaders != nil {
		clientSessionID = extractClientSessionID(clientHeaders)
	}
	if clientSessionID == "" {
		clientSessionID = strings.TrimSpace(fallbackSessionID)
	}
	ids := resolveCodexFingerprintIDs(account, clientSessionID, mode)
	if ids != nil {
		ids.clientSessionID = clientSessionID
	}
	return ids
}

// stagedCodexFingerprintClientSessionID 返回本次 attempt 解析出的原始客户端
// 会话标识，供请求构造器补出站 session-id 的 fallback。
func stagedCodexFingerprintClientSessionID(c *gin.Context, account *Account) string {
	if ids := stagedCodexFingerprintIDs(c, account); ids != nil {
		return ids.clientSessionID
	}
	return ""
}

// codexFingerprintTurnMetadataFields 返回 session/full 模式下需要收敛的
// 身份字段集：snake_case、ASCII、体积有界。turn_id 每个请求新生成，其余
// 字段按账号/会话稳定。
//
// request_kind 故意不在此集合中：客户端可能已声明 compaction / prewarm /
// memory（见 openai_ws_execution_scope.go），这类语义必须保留，只有缺失时
// 才由 fillCodexTurnMetadataFields 补成 "turn"。出站头与 body 内嵌
// metadata 都经由本函数生成，保证两处逐字段一致。
func codexFingerprintTurnMetadataFields(ids *codexFingerprintIDs) map[string]any {
	return map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMs,
	}
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 在 buildUpstreamRequest 的白名单透传之后、enforceCodexIdentityHeaders 之前调用。
func applyCodexFingerprintHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}

	// 所有非 off 模式都收敛 installation_id。
	h.Set("x-codex-installation-id", ids.installationID)

	if ids.mode == codexFingerprintDevice {
		// 默认模式：只补齐客户端缺失的官方新命名头，不覆盖已有值。
		setOpenAIHeaderIfMissing(h, "session-id", ids.sessionID, "session_id")
		threadID := firstNonEmptyHeader(h, "thread-id", "thread_id")
		if threadID == "" {
			threadID = ids.threadID
		}
		setOpenAIHeaderIfMissing(h, "thread-id", threadID)
		// x-client-request-id 与 thread-id 同值；客户端已带时不覆盖。
		setOpenAIHeaderIfMissing(h, "x-client-request-id", threadID)
		// window 必须挂在最终出站 thread-id 上（官方形态 `<thread>:0`）。
		setOpenAIHeaderIfMissing(h, "x-codex-window-id", resolveConvergedWindowID(threadID), "window_id")
		rewriteCodexTurnMetadataFields(h, map[string]any{"installation_id": ids.installationID})
		// 用最终 header 值回填 metadata，保证客户端已带的会话标识与
		// header 一致（而不是退回到账号级 fallback）。
		fillCodexTurnMetadataFields(h, map[string]any{
			"session_id":   firstNonEmptyHeader(h, "session-id", "session_id"),
			"thread_id":    firstNonEmptyHeader(h, "thread-id", "thread_id"),
			"turn_id":      ids.turnID,
			"window_id":    firstNonEmptyHeader(h, "x-codex-window-id", "window_id"),
			"request_kind": "turn",
		})
		return
	}

	// session / full 模式：改写所有相关头，并统一为官方连字符头名。
	h.Del("session_id")
	h.Set("session-id", ids.sessionID)
	h.Set("thread-id", ids.threadID)
	h.Set("x-client-request-id", ids.threadID)
	h.Set("x-codex-window-id", ids.windowID)

	fields := codexFingerprintTurnMetadataFields(ids)
	rewriteCodexTurnMetadataFields(h, fields)
	// request_kind 只补缺失：客户端已声明的 compaction / prewarm / memory
	// 是有效语义，不能被改写成 turn。
	fillCodexTurnMetadataFields(h, map[string]any{"request_kind": "turn"})
}

func setOpenAIHeaderIfMissing(h http.Header, name, value string, aliases ...string) {
	if h == nil || value == "" {
		return
	}
	if firstNonEmptyHeader(h, append([]string{name}, aliases...)...) != "" {
		return
	}
	h.Set(name, value)
}

// rewriteCodexTurnMetadataFields 解析 x-codex-turn-metadata 头中的 JSON，
// 替换指定字段后回写。合法对象保留未指定字段（如 sandbox、thread_source）；
// 非法/非对象/缺失时重建为最小合法 metadata，避免 flat 与 embedded identity 分裂。
func rewriteCodexTurnMetadataFields(h http.Header, fields map[string]any) {
	if h == nil {
		return
	}
	raw := strings.TrimSpace(h.Get(openAIWSTurnMetadataHeader))
	metadata := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
			metadata = make(map[string]any, len(fields))
		}
	}
	for k, v := range fields {
		metadata[k] = v
	}
	rebuilt, err := marshalCodexTurnMetadata(metadata)
	if err != nil {
		return
	}
	h.Set(openAIWSTurnMetadataHeader, string(rebuilt))
}

// fillCodexTurnMetadataFields 只在字段缺失时补齐，不覆盖客户端已有值。
func fillCodexTurnMetadataFields(h http.Header, fields map[string]any) {
	if h == nil || len(fields) == 0 {
		return
	}
	raw := strings.TrimSpace(h.Get(openAIWSTurnMetadataHeader))
	metadata := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
			metadata = make(map[string]any, len(fields))
		}
	}
	for k, v := range fields {
		if _, exists := metadata[k]; !exists {
			metadata[k] = v
		}
	}
	rebuilt, err := marshalCodexTurnMetadata(metadata)
	if err != nil {
		return
	}
	h.Set(openAIWSTurnMetadataHeader, string(rebuilt))
}

// applyCodexFingerprintClientMetadata 按预计算的收敛 ID 改写请求体中的 client_metadata。
// 使用与头改写相同的 ids 实例，确保 turn_id 等随机字段一致。
func applyCodexFingerprintClientMetadata(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil || ids == nil {
		return false
	}

	captureCodexFingerprintOriginalBodySessionID(ids, reqBody["client_metadata"])
	existing, _ := reqBody["client_metadata"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}

	modified := false
	if applyCodexFingerprintToClientMetadataMap(existing, ids) {
		reqBody["client_metadata"] = existing
		modified = true
	}
	if applyCodexFingerprintPromptCacheKey(reqBody, ids) {
		modified = true
	}
	return modified
}

// applyCodexFingerprintToClientMetadataMap 是 client_metadata 改写的共享核心，
// map 版（非透传，body 已解码）与 raw 字节版（透传热路径）都经由它，保证两条
// 路径的收敛语义永不漂移。
func applyCodexFingerprintToClientMetadataMap(existing map[string]any, ids *codexFingerprintIDs) bool {
	if existing == nil || ids == nil {
		return false
	}

	modified := false

	if ids.installationID != "" {
		existing["x-codex-installation-id"] = ids.installationID
		modified = true
	}

	if ids.mode == codexFingerprintDevice {
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"installation_id": ids.installationID,
		})
		return modified
	}

	// session / full 模式
	existing["session_id"] = ids.sessionID
	existing["thread_id"] = ids.threadID
	existing["turn_id"] = ids.turnID
	existing["x-codex-window-id"] = ids.windowID

	// 出站头与 body 内嵌 metadata 共用同一字段集；request_kind 只补缺失，
	// 保留客户端声明的 compaction / prewarm / memory。
	rewriteClientMetadataEmbeddedTurnMetadata(existing, codexFingerprintTurnMetadataFields(ids))
	fillClientMetadataEmbeddedTurnMetadata(existing, map[string]any{"request_kind": "turn"})
	return true
}

func captureCodexFingerprintOriginalBodySessionID(ids *codexFingerprintIDs, clientMetadata any) {
	if ids == nil || ids.originalBodySessionIDCaptured {
		return
	}
	ids.originalBodySessionIDCaptured = true
	if clientMetadata == nil {
		return
	}
	switch metadata := clientMetadata.(type) {
	case map[string]any:
		if sessionID, ok := metadata["session_id"].(string); ok {
			ids.originalBodySessionID = strings.TrimSpace(sessionID)
		}
	case map[string]string:
		ids.originalBodySessionID = strings.TrimSpace(metadata["session_id"])
	}
}

func captureCodexFingerprintOriginalBodySessionIDRaw(ids *codexFingerprintIDs, value gjson.Result) {
	if ids == nil || ids.originalBodySessionIDCaptured {
		return
	}
	ids.originalBodySessionIDCaptured = true
	if value.Exists() && value.Type == gjson.String {
		ids.originalBodySessionID = strings.TrimSpace(value.String())
	}
}

func shouldRewriteCodexFingerprintPromptCacheKey(ids *codexFingerprintIDs, promptCacheKey string) bool {
	if ids == nil || !ids.originalBodySessionIDCaptured || ids.originalBodySessionID == "" || ids.sessionID == "" {
		return false
	}
	if ids.mode != codexFingerprintSession && ids.mode != codexFingerprintFull {
		return false
	}
	return promptCacheKey == ids.originalBodySessionID
}

func applyCodexFingerprintPromptCacheKey(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil {
		return false
	}
	promptCacheKey, ok := reqBody["prompt_cache_key"].(string)
	if !ok || strings.TrimSpace(promptCacheKey) == "" || !shouldRewriteCodexFingerprintPromptCacheKey(ids, promptCacheKey) {
		return false
	}
	if promptCacheKey == ids.sessionID {
		return false
	}
	reqBody["prompt_cache_key"] = ids.sessionID
	return true
}

// applyCodexFingerprintClientMetadataRaw 在原始 JSON 字节上改写 client_metadata，
// 供透传路径使用——透传是热路径，禁止对可能高达数十 MB 的 body 做全量
// Unmarshal（见 forwardOpenAIPassthrough 的轻量提取注释）。实现为：gjson 提取
// client_metadata 小对象单独解码，经共享核心改写后 sjson 一次性拼回，body
// 其余字节原样保留；root prompt_cache_key 仅在可证明是 body session 默认值时
// 做标量改写。语义与 applyCodexFingerprintClientMetadata 逐点一致（含
// "非对象值整体替换为收敛集合"的行为）。
func applyCodexFingerprintClientMetadataRaw(body []byte, ids *codexFingerprintIDs) ([]byte, bool, error) {
	if len(body) == 0 || ids == nil {
		return body, false, nil
	}
	// 非 JSON 对象的 body（数组/标量/畸形）没有 client_metadata 语义，
	// sjson 在这类根上写字段会改写整体结构，直接放行保持原样。
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
		return body, false, nil
	}

	existing := map[string]any{}
	if cm := gjson.GetBytes(body, "client_metadata"); cm.IsObject() {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.GetBytes(body, "client_metadata.session_id"))
		if err := json.Unmarshal([]byte(cm.Raw), &existing); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for fingerprint: %w", err)
		}
	} else {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
	}

	next := body
	modified := false
	if applyCodexFingerprintToClientMetadataMap(existing, ids) {
		raw, err := json.Marshal(existing)
		if err != nil {
			return body, false, fmt.Errorf("encode converged client_metadata: %w", err)
		}
		var setErr error
		next, setErr = sjson.SetRawBytes(body, "client_metadata", raw)
		if setErr != nil {
			return body, false, fmt.Errorf("splice converged client_metadata: %w", setErr)
		}
		modified = true
	}
	promptCacheKey := gjson.GetBytes(body, "prompt_cache_key")
	if promptCacheKey.Exists() && promptCacheKey.Type == gjson.String && strings.TrimSpace(promptCacheKey.String()) != "" && shouldRewriteCodexFingerprintPromptCacheKey(ids, promptCacheKey.String()) {
		rewritten, err := sjson.SetBytes(next, "prompt_cache_key", ids.sessionID)
		if err != nil {
			return body, false, fmt.Errorf("splice converged prompt_cache_key: %w", err)
		}
		next = rewritten
		modified = true
	}
	return next, modified, nil
}

// rewriteClientMetadataEmbeddedTurnMetadata 改写 client_metadata 中内嵌的
// x-codex-turn-metadata JSON 字符串里的指定字段。非法/非对象值会重建，
// 避免 flat client_metadata 与 embedded metadata 暴露两套身份。
func rewriteClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any) {
	updateClientMetadataEmbeddedTurnMetadata(clientMetadata, fields, false)
}

// fillClientMetadataEmbeddedTurnMetadata 只在字段缺失时补齐内嵌 metadata，
// 不覆盖客户端已有值（例如 request_kind）。
func fillClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any) {
	updateClientMetadataEmbeddedTurnMetadata(clientMetadata, fields, true)
}

func updateClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any, fillOnly bool) {
	if clientMetadata == nil || len(fields) == 0 {
		return
	}
	raw, ok := clientMetadata[openAIWSTurnMetadataHeader].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		metadata = make(map[string]any, len(fields))
	}
	for k, v := range fields {
		if fillOnly {
			if _, exists := metadata[k]; exists {
				continue
			}
		}
		metadata[k] = v
	}
	if rebuilt, err := marshalCodexTurnMetadata(metadata); err == nil {
		clientMetadata[openAIWSTurnMetadataHeader] = string(rebuilt)
	}
}
