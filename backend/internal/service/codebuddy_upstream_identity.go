package service

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// CodeBuddy 出站请求头规范化（对齐官方客户端形态）。
//
// 背景：上游按请求头与内容指纹识别"非官方客户端"，命中即整条请求 400（业务码 11128）。
// 我们此前的出站头是最小集 + 自报名号 UA（codebuddy2openai/2.0），与官方客户端差异明显。
// 本文件把 chat / 连通性测试 / token 刷新三条链路的头规范成官方形态。
//
// 事实来源：
//   - 参考实现 workbuddy2api internal/upstream/headers.go（MIT，2026-09-14 核对源码）：
//     UA 三段式与版本、X-CodeBuddy-Request、Origin/Referer 与 Accept-Language 按 realm、
//     X-No-* 缺省声明、归属头（X-Product / X-Agent-Purpose / X-IDE-*）、会话头族
//     （X-Conversation-* / X-Request-ID / B3 链路）；
//   - 本仓 proto-run 实证（2026-09-11）：身份头集合（X-User-Id / X-Enterprise-Id /
//     X-Tenant-Id / X-Domain）+ Bearer 被上游接受。
//
// 明确不做（与参考实现一致）：UA 随机化（官方是确定性的）；客户端 IP 透传
// （X-Forwarded-For / X-Real-IP，默认不把内网/代理 IP 暴露给上游）；设备令牌伪造
// （仅在账号凭据提供 device_token 时注入）。
const (
	// codeBuddyDefaultClientVersion 官方桌面分发包版本段（UA 的 `WorkBuddy/<ver>`）。
	codeBuddyDefaultClientVersion = "5.5.4"
	// codeBuddyDefaultCLIVersion 官方内置 CLI 版本段（UA 的 `CLI/<ver>`）。
	codeBuddyDefaultCLIVersion = "2.137.1"
	// codeBuddyDefaultClientName 归属头里的客户端名（X-Product / X-IDE-Name / X-IDE-Type）。
	codeBuddyDefaultClientName = "WorkBuddy"
	// codeBuddyDefaultAgentIntent 会话意图头默认值（官方 CLI：meta["codebuddy.ai/mode"] ?? "craft"）。
	codeBuddyDefaultAgentIntent = "craft"

	codeBuddyCNOrigin     = "https://www.codebuddy.cn"
	codeBuddyGlobalOrigin = "https://www.workbuddy.ai"

	codeBuddyCNBrandDomain     = "www.codebuddy.cn"
	codeBuddyGlobalBrandDomain = "www.workbuddy.ai"

	// codeBuddyConversationReqIDContextKey 会话轮级聚合主键在 gin context 的惰性缓存键：
	// 同一次入站请求的所有上游尝试（重试/换号）复用同一 ID，与官方"一次 user send
	// 内所有 tool call/重试复用同一 X-Conversation-Request-ID"语义一致。
	codeBuddyConversationReqIDContextKey = "codebuddy_conversation_request_id"
)

// codeBuddyRealmKind 账号所属品牌域：CN（codebuddy.cn）或 global（workbuddy.ai）。
type codeBuddyRealmKind string

const (
	codeBuddyRealmKindCN     codeBuddyRealmKind = "cn"
	codeBuddyRealmKindGlobal codeBuddyRealmKind = "global"
)

func codeBuddyAccountRealm(a *Account) codeBuddyRealmKind {
	if a == nil {
		return codeBuddyRealmKindCN
	}
	switch strings.ToLower(strings.TrimSpace(a.GetCredential("realm"))) {
	case string(codeBuddyRealmKindGlobal):
		return codeBuddyRealmKindGlobal
	case string(codeBuddyRealmKindCN):
		return codeBuddyRealmKindCN
	}
	haystack := strings.ToLower(a.GetCredential("domain") + " " + a.GetOpenAIBaseURL())
	if strings.Contains(haystack, "workbuddy") {
		return codeBuddyRealmKindGlobal
	}
	return codeBuddyRealmKindCN
}

// codeBuddyOrigin 返回该账号 realm 对应的 Origin/Referer 基础域。
func codeBuddyOrigin(realm codeBuddyRealmKind) string {
	if realm == codeBuddyRealmKindGlobal {
		return codeBuddyGlobalOrigin
	}
	return codeBuddyCNOrigin
}

// codeBuddyAcceptLanguage 按 realm 返回 Accept-Language：官方桌面端按账号域发对应语言，
// 缺省/不一致会被上游风控按"语言缺失"误判（参考实现 D5）。
func codeBuddyAcceptLanguage(realm codeBuddyRealmKind) string {
	if realm == codeBuddyRealmKindGlobal {
		return "en-US"
	}
	return "zh-CN"
}

// codeBuddyClientVersion 生效的客户端版本段：凭据 client_version > 内置默认。
func codeBuddyClientVersion(a *Account) string {
	if a != nil {
		if v := strings.TrimSpace(a.GetCredential("client_version")); v != "" {
			return v
		}
	}
	return codeBuddyDefaultClientVersion
}

// codeBuddyCLIVersion 生效的 CLI 版本段：凭据 cli_version > 内置默认。
func codeBuddyCLIVersion(a *Account) string {
	if a != nil {
		if v := strings.TrimSpace(a.GetCredential("cli_version")); v != "" {
			return v
		}
	}
	return codeBuddyDefaultCLIVersion
}

// codeBuddyClientName 归属头里的客户端名：凭据 client_name > 内置默认。
// codeBuddyAgentIntent 会话意图（官方 CLI：session.meta["codebuddy.ai/mode"] ?? "craft"）。
// 官方在请求头里恒发该值；本仓无法感知客户端模式，按官方默认值 "craft" 注入，
// 可用凭据 agent_intent 覆盖。
func codeBuddyAgentIntent(a *Account) string {
	if a != nil {
		if v := strings.TrimSpace(a.GetCredential("agent_intent")); v != "" {
			return v
		}
	}
	return codeBuddyDefaultAgentIntent
}

func codeBuddyClientName(a *Account) string {
	if a != nil {
		if v := strings.TrimSpace(a.GetCredential("client_name")); v != "" {
			return v
		}
	}
	return codeBuddyDefaultClientName
}

// codeBuddyUserAgent 出站 UA（chat / 测试 / 刷新三链路共用）：
// 凭据 user_agent 显式覆盖 > 官方三段式 `WorkBuddy/<ver> <brand>/<ver> CLI/<cliVer>`。
// global 账号平台段是 `WorkBuddy AI`（送错品牌段会触发上游 403 code 11140）。
func codeBuddyUserAgent(a *Account) string {
	if a != nil {
		if v := strings.TrimSpace(a.GetCredential("user_agent")); v != "" {
			return v
		}
	}
	brand := "WorkBuddy"
	if codeBuddyAccountRealm(a) == codeBuddyRealmKindGlobal {
		brand = "WorkBuddy AI"
	}
	ver := codeBuddyClientVersion(a)
	return "WorkBuddy/" + ver + " " + brand + "/" + ver + " CLI/" + codeBuddyCLIVersion(a)
}

// codeBuddyHeaderDomain 返回 X-Domain 的取值。
// 取值顺序：凭据 x_domain 显式覆盖 > 凭据 domain（auth 域原值）> 空串（调用方改发
// X-No-Department-Info: 1，与官方 CLI 缺省约定一致，不凭空编造品牌域）。
// 观察到 domain 被填成端点主机（copilot.tencent.com）时告警一次，提示运维修账号数据。
func codeBuddyHeaderDomain(a *Account) string {
	if a == nil {
		return ""
	}
	if v := strings.TrimSpace(a.GetCredential("x_domain")); v != "" {
		return v
	}
	domain := strings.TrimSpace(a.GetCredential("domain"))
	if domain == "" {
		return ""
	}
	if codeBuddyDomainLooksLikeEndpointHost(a, domain) {
		logger.L().Warn("codebuddy account domain looks like endpoint host, expected login brand domain",
			zap.Int64("account_id", a.ID),
			zap.String("domain", domain),
			zap.String("hint", "X-Domain 应为登录域（如 www.codebuddy.cn）；可用凭据 x_domain 覆盖"),
		)
	}
	return domain
}

// codeBuddyDomainLooksLikeEndpointHost domain 是否等于（或落在）账号 base_url 的主机名。
func codeBuddyDomainLooksLikeEndpointHost(a *Account, domain string) bool {
	base := strings.ToLower(strings.TrimSpace(a.GetOpenAIBaseURL()))
	if base == "" {
		return false
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		if strings.EqualFold(domain, u.Hostname()) {
			return true
		}
	}
	return strings.EqualFold(domain, "copilot.tencent.com")
}

// codeBuddyMessageID 生成会话头族里消息级 ID（32 位 hex，每条出站独立）。
func codeBuddyMessageID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 熵源不可用属系统级故障；此处退化为时间无关的固定前缀会破坏唯一性，
		// 故仍返回占位 hex 并让调用方照常发送（上游只要求形状合法）。
		return strings.Repeat("0", 32)
	}
	return hex.EncodeToString(buf)
}

// codeBuddyConversationRequestID 返回本次入站请求的聚合主键（32 位 hex）。
// 归一化要求：B3 链路族只认 16/32 位 hex，因此这里恒产 32 位 hex；
// 同一入站请求内所有上游尝试复用同值（gin context 惰性缓存）。
func codeBuddyConversationRequestID(c *gin.Context) string {
	if c == nil {
		return codeBuddyMessageID()
	}
	if v, ok := c.Get(codeBuddyConversationReqIDContextKey); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	seed := fmt.Sprintf("sub2api:codebuddy-conversation-request:%d:%s",
		getAPIKeyIDFromContext(c), codeBuddyMessageID())
	sum := sha1.Sum([]byte(seed))
	id := hex.EncodeToString(sum[:])[:32]
	c.Set(codeBuddyConversationReqIDContextKey, id)
	return id
}

// applyCodeBuddyCommonUpstreamHeaders 官方公共头（chat / 测试 / 刷新共用）。
// Authorization 由调用方按各自链路注入，不在此设置。
func applyCodeBuddyCommonUpstreamHeaders(header http.Header, account *Account, streaming bool) {
	realm := codeBuddyAccountRealm(account)
	header.Set("Content-Type", "application/json")
	if streaming {
		// 官方 chat 流式 Accept 形态（参考实现 ChatHeaders 覆盖 CommonHeaders 的取值）。
		header.Set("Accept", "application/json, text/event-stream")
	} else {
		header.Set("Accept", "application/json")
	}
	header.Set("X-Requested-With", "XMLHttpRequest")
	origin := codeBuddyOrigin(realm)
	header.Set("Origin", origin)
	header.Set("Referer", origin+"/")
	header.Set("User-Agent", codeBuddyUserAgent(account))
	// X-CodeBuddy-Request：官方客户端风控闸门头，所有 API 请求必带（参考实现 D1）。
	header.Set("X-CodeBuddy-Request", "1")
	header.Set("Accept-Language", codeBuddyAcceptLanguage(realm))
}

// applyCodeBuddyIdentityHeaders 账号身份头。
//
// 规则逐条对齐官方 CLI 的鉴权拦截器（app.asar.unpacked/cli/dist/codebuddy.js，
// 2026-09-14 读源码实证）：
//
//	if (!config.headers["X-No-Authorization"])      → Authorization: Bearer <accessToken>
//	if (!X-User-Id && uid && !X-No-User-Id)          → X-User-Id: uid          ，否则 X-No-User-Id: 1
//	if (!X-Enterprise-Id && enterpriseId && !X-No-…) → X-Enterprise-Id: 企业ID ，否则 X-No-Enterprise-Id: 1
//	if (!X-Department-Info && departmentFullName)    → X-Department-Info: 部门，否则 X-No-Department-Info: 1
//	if (!X-Tenant-Id && enterpriseId)                → X-Tenant-Id: 企业ID（无企业则不发）
//	if (!X-Domain && auth.domain)                    → X-Domain: 登录域（无则**不发**，没有 X-No-Domain）
//
// 注意：X-No-Department-Info 声明的是「无部门」，与 X-Domain 无关——早期实现把"无登录域"
// 误用成 X-No-Department-Info，已按官方口径修正。
func applyCodeBuddyIdentityHeaders(header http.Header, account *Account) {
	if uid := account.GetCodeBuddyUID(); uid != "" {
		header.Set("X-User-Id", uid)
	} else {
		header.Set("X-No-User-Id", "1")
	}
	if enterpriseID := account.GetCodeBuddyEnterpriseID(); enterpriseID != "" {
		header.Set("X-Enterprise-Id", enterpriseID)
		header.Set("X-Tenant-Id", enterpriseID)
	} else {
		header.Set("X-No-Enterprise-Id", "1")
	}
	if department := account.GetCodeBuddyDepartment(); department != "" {
		header.Set("X-Department-Info", department)
	} else {
		header.Set("X-No-Department-Info", "1")
	}
	if domain := codeBuddyHeaderDomain(account); domain != "" {
		header.Set("X-Domain", domain)
	}
}

// applyCodeBuddyAttributionHeaders 用量归属头：真实桌面端发 X-Agent-Purpose="conversation"
// 与 X-IDE-*，上游据此在用量统计里识别 client/agentPurpose（缺失即归因为空）。
func applyCodeBuddyAttributionHeaders(header http.Header, account *Account) {
	name := codeBuddyClientName(account)
	header.Set("X-Agent-Purpose", "conversation")
	header.Set("X-Agent-Intent", codeBuddyAgentIntent(account))
	header.Set("X-IDE-Name", name)
	header.Set("X-IDE-Type", name)
	header.Set("X-IDE-Version", codeBuddyClientVersion(account))
	header.Set("X-Product", name)
}

// applyCodeBuddyConversationHeaders 会话头族（CN/global 同构）：
//   - X-Conversation-ID：会话级，仅透传请求体里的 conversationId/conversation_id，
//     客户端没给就不发（不伪造，避免误导上游后台建错会话）；
//   - X-Conversation-Request-ID：对话轮级聚合主键，必发；
//   - X-Request-ID / X-Conversation-Message-ID：消息级（每条出站独立）；
//   - X-Root-Request-ID / X-Trace-ID：请求追溯；
//   - X-B3-*：链路族，TraceId 取 32 位 hex 主键，SpanId 取消息 ID 前 16 位。
func applyCodeBuddyConversationHeaders(header http.Header, c *gin.Context, body []byte) {
	if conversationID := codeBuddyRequestBodyConversationID(body); conversationID != "" {
		header.Set("X-Conversation-ID", conversationID)
	}
	convReqID := codeBuddyConversationRequestID(c)
	messageID := codeBuddyMessageID()

	header.Set("X-Conversation-Request-ID", convReqID)
	header.Set("X-Conversation-Message-ID", messageID)
	header.Set("X-Request-ID", messageID)
	header.Set("X-Root-Request-ID", convReqID)
	header.Set("X-Trace-ID", convReqID)
	header.Set("X-B3-TraceId", convReqID)
	header.Set("X-B3-SpanId", messageID[:16])
	header.Set("X-B3-Sampled", "1")
}

// codeBuddyRequestBodyConversationID 从入站请求体取会话 ID（snake/camel 两种键）。
func codeBuddyRequestBodyConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if v := strings.TrimSpace(gjson.GetBytes(body, "conversation_id").String()); v != "" {
		return v
	}
	return strings.TrimSpace(gjson.GetBytes(body, "conversationId").String())
}

// applyCodeBuddyChatUpstreamHeaders 把 chat（含账号连通性测试）链路的全部规范头写入上游请求：
// 公共头 + 身份头 + 归属头 + 会话头族 +（可选）设备令牌。
func applyCodeBuddyChatUpstreamHeaders(header http.Header, account *Account, c *gin.Context, body []byte) {
	if account == nil || !account.IsCodeBuddy() {
		return
	}
	applyCodeBuddyCommonUpstreamHeaders(header, account, true)
	applyCodeBuddyIdentityHeaders(header, account)
	applyCodeBuddyAttributionHeaders(header, account)
	applyCodeBuddyConversationHeaders(header, c, body)
	// 设备令牌：仅当账号凭据提供（每号）时注入；鉴权/刷新类请求不注入
	// （参考实现：给刷新请求带设备令牌可能被判异常客户端）。
	if token := strings.TrimSpace(account.GetCredential("device_token")); token != "" {
		header.Set("X-Device-Token", token)
	}
}

// applyCodeBuddyRefreshUpstreamHeaders token 刷新链路的规范头：
// 公共头 + 身份头（非空才发）+ X-Refresh-Token + X-Auth-Refresh-Source: plugin。
// 不带设备令牌、归属头与会话头族（与官方刷新请求形态一致）。
func applyCodeBuddyRefreshUpstreamHeaders(header http.Header, account *Account) {
	if account == nil || !account.IsCodeBuddy() {
		return
	}
	applyCodeBuddyCommonUpstreamHeaders(header, account, false)
	applyCodeBuddyIdentityHeaders(header, account)
	header.Set("X-Refresh-Token", account.GetCodeBuddyRefreshToken())
	header.Set("X-Auth-Refresh-Source", "plugin")
}
