package service

import (
	"fmt"
	"net/http"
	"strings"
)

// opencode GO（Console Go）出站必需头。详见 https://opencode.ai/docs/go/#where-can-i-use-it：
//   - 必须用自有 agent 名做 UA（"Identify itself with its own user agent, such as
//     my-coding-agent/1.0, rather than a generic SDK or HTTP-library name"）；
//   - 每个对话必须带稳定的 x-opencode-session（"Send a stable session ID in
//     x-opencode-session for each conversation so we can optimize routing and
//     prompt caching"），缺头上游直接 400 MissingSessionID。
//
// 这两条对所有面向 opencode 上游的请求都成立，因此必须由**同一处**注入：此前只有
// 真实转发路径（sendCCUpstreamRequestOnce）注入，管理面连通性测试
// （testOpenAIChatCompletionsConnection）与模型目录同步
// （buildOpenAIUpstreamModelsRequest）各写各的头，漏了会话头 → "正常调用 200、
// 面板点测试 400 MissingSessionID"（2026-09-17 用户在生产实测）。
const (
	// openCodeUpstreamUserAgent 是 opencode GO 出站的自有 UA 兜底（调用方已有
	// 自定义 UA 时以调用方为准）。
	openCodeUpstreamUserAgent = "sub2api/1.0"
	// openCodeSessionHeader 是 opencode GO 强制要求的会话头名。
	openCodeSessionHeader = "x-opencode-session"
)

// openCodeSessionHeaderValue 派生会话头取值。无状态网关拿不到客户端真实会话 ID，
// 复用 Claude 伪装路径同款"会话级稳定种子"近似：同一对话（首条 user 消息不变）
// 跨轮稳定，跨 API Key / 账号 / 对话互不相同。keyID 为 0 时（管理面测试等没有
// 客户端 API Key 的场景）退化为"账号 + 内容"维度，同样稳定。
func openCodeSessionHeaderValue(accountID, keyID int64, body []byte) string {
	return generateSessionUUID(fmt.Sprintf(
		"sub2api:opencode-session:u%d:a%d:%s", keyID, accountID, extractFirstUserText(body),
	))
}

// applyOpenCodeUpstreamHeaders 为 opencode 账号写入出站必需头：会话头 + UA 兜底。
// 非 opencode 账号原样不动（同一请求构造代码被多平台共用）。
//
// 注意与账号级 header 覆写的顺序：x-opencode-session 在 account_header_override
// 的不可覆写清单内，先注入再 ApplyHeaderOverrides 也能保住取值。
func applyOpenCodeUpstreamHeaders(header http.Header, account *Account, keyID int64, body []byte, userAgent string) {
	if header == nil || account == nil || !account.IsOpenCode() {
		return
	}
	header.Set(openCodeSessionHeader, openCodeSessionHeaderValue(account.ID, keyID, body))
	if strings.TrimSpace(userAgent) == "" {
		header.Set("user-agent", openCodeUpstreamUserAgent)
	}
}
