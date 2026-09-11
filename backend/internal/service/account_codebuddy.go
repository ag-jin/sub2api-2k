package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CodeBuddy 平台账号的凭据与端点（腾讯 Copilot）。
// 上游协议事实见 .scratch/codebuddy-platform/design.md 与
// proto-run/evidence.md（2026-09-11 原型实证）：
//   - 对话端点 POST {base}/v2/chat/completions（原生 OpenAI Chat 协议、仅流式）
//   - 刷新端点 POST {base}/v2/plugin/auth/token/refresh
//   - auth JSON 字段 auth.accessToken/refreshToken/expiresAt(毫秒)/domain +
//     account.uid/enterpriseId（个人账号 enterpriseId 为 null → 归一为空串）
//   - 刷新响应只有 expiresIn / refreshExpiresIn 秒数，expiresAt 需客户端换算
//
// CodeBuddyChatCompletionsPath CodeBuddy 的 Chat Completions 端点路径（v2，非 v1）。
const CodeBuddyChatCompletionsPath = "/v2/chat/completions"

// codeBuddyTokenRefreshPath CodeBuddy 的 token 刷新端点路径。
const codeBuddyTokenRefreshPath = "/v2/plugin/auth/token/refresh"

// CodeBuddyAutoModel 上游"自动路由"模型：服务端解析为具体模型（原型实测
// auto → deepseek-v4.1-flash），响应中的 model 为回显实值。
const CodeBuddyAutoModel = "auto"

// codeBuddyPreservableConfigKeys 非 token 类、允许保留的管理员配置键。
var codeBuddyPreservableConfigKeys = []string{
	"base_url", "model_mapping",
	"header_override_enabled", "header_overrides",
	"expires_in", "refresh_expires_at", "last_refresh_time",
}

// NormalizeCodeBuddyCredentials 把用户粘贴的 CodeBuddy auth JSON 归一化为
// 白名单字段式 credentials map。
//
// 接受三种输入形态（都容忍缺键）：
//  1. 完整 auth 文件结构：{auth:{accessToken,refreshToken,expiresAt,domain},account:{uid,enterpriseId}}
//  2. 扁平化后的凭据 map（如刷新回写产物）：{access_token,refresh_token,expires_at,domain,uid,enterprise_id}
//  3. 两者混合（编辑场景增量提交）
//
// 处理规则：
//   - enterpriseId 为 null（个人账号）时归一为空串，不当必填；
//   - expiresAt 既接受毫秒数字/字符串（存储换算为 RFC3339，供 GetCredentialAsTime 读取），
//     也在输入含 expiresIn（秒）时按 now+expiresIn*1000 计算；
//   - 丢弃 auth 外的易变键（accounts / allAccounts 等）与未知键；
//   - 保留下列管理员配置键不丢。
func NormalizeCodeBuddyCredentials(raw map[string]any) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]any{}
	pick := func(dst, key string, src map[string]any) {
		v, ok := src[key]
		if !ok || v == nil {
			return
		}
		if s, ok := v.(string); ok && s == "" {
			return
		}
		out[dst] = v
	}
	// 1. 嵌套 auth / account 结构优先。
	if authMap, ok := raw["auth"].(map[string]any); ok {
		pick("access_token", "accessToken", authMap)
		pick("refresh_token", "refreshToken", authMap)
		pick("domain", "domain", authMap)
		pick("expires_in", "expiresIn", authMap)
		pick("refresh_expires_in", "refreshExpiresIn", authMap)
		if v, ok := authMap["expiresAt"]; ok && v != nil {
			if formatted := codeBuddyExpiresAtRFC3339(v, authMap["expiresIn"]); formatted != "" {
				out["expires_at"] = formatted
			}
		}
	}
	if accountMap, ok := raw["account"].(map[string]any); ok {
		// enterpriseId 可为 null（个人账号）——显式归一为空串。
		if v, ok := accountMap["enterpriseId"]; ok {
			if v == nil {
				out["enterprise_id"] = ""
			} else if s, isStr := v.(string); isStr {
				out["enterprise_id"] = s
			}
		}
		pick("uid", "uid", accountMap)
	}
	// 2. 扁平键（编辑/刷新回写直接给扁平 map）。
	flatExpires := raw["expires_at"]
	if _, exists := out["access_token"]; !exists {
		pick("access_token", "access_token", raw)
	}
	if _, exists := out["refresh_token"]; !exists {
		pick("refresh_token", "refresh_token", raw)
	}
	if _, exists := out["domain"]; !exists {
		pick("domain", "domain", raw)
	}
	if _, exists := out["expires_at"]; !exists {
		// 扁平 expires_at：毫秒值归一为 RFC3339；已是 RFC3339 字符串则原样保留。
		if formatted := codeBuddyExpiresAtRFC3339(flatExpires, raw["expires_in"]); formatted != "" {
			out["expires_at"] = formatted
		} else {
			pick("expires_at", "expires_at", raw)
		}
	}
	if _, exists := out["uid"]; !exists {
		pick("uid", "uid", raw)
	}
	if _, exists := out["enterprise_id"]; !exists {
		pick("enterprise_id", "enterprise_id", raw)
	}
	// 3. 管理员可保留键原样继承。
	for _, k := range codeBuddyPreservableConfigKeys {
		pick(k, k, raw)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// codeBuddyExpiresAtRFC3339 把 CodeBuddy 的毫秒 expiresAt（或 expiresIn 秒兜底）
// 归一化为 RFC3339 UTC 字符串。expiresIn 边界（60s / 0 / 负值）全部如实换算：
// 60s → now+60s；0/负 → now / 过去时间（ NeedsRefresh 会立即判定需要刷新）。
func codeBuddyExpiresAtRFC3339(expiresAt, expiresIn any) string {
	if ms, ok := codeBuddyMillis(expiresAt); ok && ms > 0 {
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	if sec, ok := codeBuddySeconds(expiresIn); ok {
		return time.Now().Add(time.Duration(sec) * time.Second).UTC().Format(time.RFC3339)
	}
	return ""
}

// codeBuddyMillis 解析毫秒时间戳（int/float/string）。
func codeBuddyMillis(v any) (int64, bool) {
	switch value := v.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		return int64(value), true
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return n, true
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func codeBuddySeconds(v any) (int64, bool) {
	ms, ok := codeBuddyMillis(v)
	return ms, ok
}

// codeBuddyStr returns trimmed string for any (nil-safe).
func codeBuddyStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v)) // json.Number 等
}

// BuildCodeBuddyRefreshedCredentials 把刷新端点响应 data 落成 credentials 增量。
// expiresIn / refreshExpiresIn 为秒，需换算毫秒（60s/0/负值边界沿用真实语义）；
// lastRefreshTime 为当前毫秒时间戳；缺失 accessToken 的响应视为失败。
func BuildCodeBuddyRefreshedCredentials(nowMs int64, domain string, data map[string]any) (map[string]any, error) {
	accessToken := codeBuddyStr(data["accessToken"])
	if accessToken == "" {
		return nil, fmt.Errorf("%w: codebuddy refresh response missing accessToken", errCodeBuddyRefreshRejected)
	}
	out := map[string]any{
		"access_token":      accessToken,
		"last_refresh_time": nowMs,
	}
	if rt := codeBuddyStr(data["refreshToken"]); rt != "" {
		out["refresh_token"] = rt // 上游轮换 refreshToken，必须写回新值
	}
	if d := codeBuddyStr(data["domain"]); d != "" {
		out["domain"] = d
	} else if codeBuddyStr(domain) != "" {
		out["domain"] = codeBuddyStr(domain) // domain 继承刷新前值
	}
	if sec, ok := codeBuddySeconds(data["expiresIn"]); ok {
		out["expires_at"] = time.UnixMilli(nowMs + sec*1000).UTC().Format(time.RFC3339)
	}
	if sec, ok := codeBuddySeconds(data["refreshExpiresIn"]); ok {
		out["refresh_expires_at"] = time.UnixMilli(nowMs + sec*1000).UTC().Format(time.RFC3339)
	}
	return out, nil
}

// --- CodeBuddy credential accessors ---

// GetCodeBuddyAccessToken 返回刷新后的访问令牌（credentials.access_token）。
func (a *Account) GetCodeBuddyAccessToken() string {
	if a == nil || !a.IsCodeBuddy() {
		return ""
	}
	return strings.TrimSpace(a.GetCredential("access_token"))
}

// GetCodeBuddyRefreshToken 返回刷新令牌。
func (a *Account) GetCodeBuddyRefreshToken() string {
	if a == nil || !a.IsCodeBuddy() {
		return ""
	}
	return strings.TrimSpace(a.GetCredential("refresh_token"))
}

// GetCodeBuddyUID 返回账号 uid（仅用于 X-User-Id 头注入，不落日志）。
func (a *Account) GetCodeBuddyUID() string {
	if a == nil || !a.IsCodeBuddy() {
		return ""
	}
	return strings.TrimSpace(a.GetCredential("uid"))
}

// GetCodeBuddyEnterpriseID 返回 enterprise id；空串（个人账号）也按原样返回，
// 头注入时发空串（原型实证：上游接受空串）。
func (a *Account) GetCodeBuddyEnterpriseID() string {
	if a == nil || !a.IsCodeBuddy() {
		return ""
	}
	return strings.TrimSpace(a.GetCredential("enterprise_id"))
}

// GetCodeBuddyDomain 返回 X-Domain 头的取值（auth.domain 原值，不是端点主机名）。
func (a *Account) GetCodeBuddyDomain() string {
	if a == nil || !a.IsCodeBuddy() {
		return ""
	}
	return strings.TrimSpace(a.GetCredential("domain"))
}

// BuildCodeBuddyUpstreamHeaders 生成 CodeBuddy 上游请求的专用身份头集合。
// enterpriseId 空串时按空串头注入（原型实证被接受）。
func BuildCodeBuddyUpstreamHeaders(account *Account) map[string]string {
	if account == nil || !account.IsCodeBuddy() {
		return nil
	}
	return map[string]string{
		"X-User-Id":       account.GetCodeBuddyUID(),
		"X-Enterprise-Id": account.GetCodeBuddyEnterpriseID(),
		"X-Tenant-Id":     account.GetCodeBuddyEnterpriseID(),
		"X-Domain":        account.GetCodeBuddyDomain(),
	}
}
