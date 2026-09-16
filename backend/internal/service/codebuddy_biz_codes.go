package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// CodeBuddy（腾讯 Copilot）上游业务信封 {code,msg} 的管理面可读码表。
// 仅用于管理面（账号 ErrorMessage 拼接与刷新/管理日志），网关客户端 message
// 保持透传不改（精确匹配资产）。来源：workbuddy-manager 参考实现
// _CODE_HINTS 与本机实证（absorb-verify.md V4：11217 = 未扫码/登录进行中）。
var codeBuddyBizCodeHints = map[int]string{
	0:     "成功",
	10001: "今日已签到",
	11101: "上游不接受非流式请求",
	11128: "上游安全策略拦截（第三方客户端提示词指纹，或 developer 角色未归一化）",
	11217: "登录进行中（未扫码）",
	12153: "会话已失效，需重新登录",
}

// CodeBuddyBizCodeHint 返回业务码的可读说明；未知码返回空串。
func CodeBuddyBizCodeHint(code int) string {
	return codeBuddyBizCodeHints[code]
}

// CodeBuddyBizCodeMessage 拼接管理面可读消息：已知码在原文后追加 hint 说明。
// 未知码原样返回 msg。
func CodeBuddyBizCodeMessage(code int, msg string) string {
	hint := CodeBuddyBizCodeHint(code)
	if hint == "" {
		return msg
	}
	if msg == "" {
		return hint
	}
	return msg + "（" + hint + "）"
}

// codeBuddyUpstreamEnvelopeMessage 从 CodeBuddy 上游错误信封 {code,msg,...} 里取
// 可读文案（形如 "code 11128: Illegal API invocation from an unapproved channel"）；
// 非该形态（无顶层 msg）返回空串。
//
// 上游拒因只存在于这个信封里，而通用提取器 extractUpstreamErrorMessage
// 不认顶层 msg，客户端因此只看到 "Upstream error: 400"（2026-09-14 遗留项）。
// 该函数用于把这类不可诊断文案补全。
func codeBuddyUpstreamEnvelopeMessage(body []byte) string {
	msg := strings.TrimSpace(gjson.GetBytes(body, "msg").String())
	if msg == "" {
		return ""
	}
	if code := gjson.GetBytes(body, "code").Int(); code != 0 {
		return fmt.Sprintf("code %d: %s", code, msg)
	}
	return msg
}
