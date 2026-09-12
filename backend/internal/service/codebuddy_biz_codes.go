package service

// CodeBuddy（腾讯 Copilot）上游业务信封 {code,msg} 的管理面可读码表。
// 仅用于管理面（账号 ErrorMessage 拼接与刷新/管理日志），网关客户端 message
// 保持透传不改（精确匹配资产）。来源：workbuddy-manager 参考实现
// _CODE_HINTS 与本机实证（absorb-verify.md V4：11217 = 未扫码/登录进行中）。
var codeBuddyBizCodeHints = map[int]string{
	0:     "成功",
	10001: "今日已签到",
	11101: "上游不接受非流式请求",
	11128: "developer 角色需归一化为 system（上游拒绝）",
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
