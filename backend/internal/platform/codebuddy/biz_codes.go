// Package codebuddy 收拢 CodeBuddy（腾讯 Copilot）平台的实现。
//
// 本包是"按平台分包目录"的第一步：只承载**不依赖 service 包内部类型**的
// 纯逻辑件（业务码表、出站体规范化规则、指纹净化、流式帧归一化）。依赖
// service.Account 等类型的部分（账号、调度、网关装配）暂时仍留在 service
// 包内，待后续平台整改时再抽。
//
// 这样做的目的：先立住平台包的边界与命名，同时避免与 service 包形成
// 循环依赖（service → platform/codebuddy → service）。
package codebuddy

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// 业务码表。
//
// ⚠️ 上游信封的 code 字段**不保证是数字**：实测 "11-128" 就是字符串形态
// （JSON 里带引号）。因此码表必须支持字符串键，不能只按 int 查。
//
// 历史缺陷（2026-09-22 修复）：上一版把键写成 Go 常量表达式 `11-128:`
// ——Go 词法器会把 `-` 当作减号求值成 **-117**，于是这张表里那一项永远
// 匹配不上；而上游实际发的是字符串 "11-128"，gjson .Int() 对非数字串返回
// 0，导致该提示在真实错误上从未生效。同名坑在参考实现里出现过
// （Python 字典键未加引号被算成 -117，它们专门写了测试防回归）。
var codeBuddyBizCodeHints = map[string]string{
	"0":     "成功",
	"10001": "今日已签到",
	"11101": "上游不接受非流式请求",
	"11-128": "上游安全策略拦截（第三方客户端提示词指纹，或 developer 角色未归一化）",
	"11217": "登录进行中（未扫码）",
	"12153": "会话已失效，需重新登录",
	// 14018 上游自 2026-09-20 起新增：积分耗尽。是唯一的结构化"欠费"判据
	// （上游不靠"额度不足"这类跨计费/限流两界的文案猜）。
	"14018": "账号积分耗尽（等签到恢复或更换账号）",
	// 6004 只影响被调用的模型，不是账号级故障——冷却时应按模型维度处理。
	"6004": "该模型使用量超限（仅影响该模型，切换其他模型可立即使用）",
	// 11102 该后端无此模型（跨域/跨版本选错模型时会命中）。
	"11102": "该账号后端无此模型（可能选到了其他域/版本的模型）",
	// 11140 授权封禁，需重新登录。
	"11140": "请求被拒绝（授权异常，需重新登录该账号）",
	// 14017 trial 未激活（补完注册流程可自愈）。
	"14017": "试用未激活（需补完注册流程）",
}

// CodeBuddyBizCodeHint 返回业务码的可读说明；未知码返回空串。
// code 可以是数字或字符串形态（上游两种都出现过），内部统一成字符串查表。
func CodeBuddyBizCodeHint(code any) string {
	return codeBuddyBizCodeHints[codeBuddyNormalizeBizCode(code)]
}

// CodeBuddyBizCodeMessage 拼接管理面可读消息：已知码在原文后追加 hint 说明。
// 未知码原样返回 msg。
func CodeBuddyBizCodeMessage(code any, msg string) string {
	hint := CodeBuddyBizCodeHint(code)
	if hint == "" {
		return msg
	}
	if msg == "" {
		return hint
	}
	return msg + "（" + hint + "）"
}

// codeBuddyNormalizeBizCode 把上游 code 的任意形态归一成查表键：
//   - 数字 → 十进制字符串（"14018"）
//   - 字符串 → 原值去空白（"11-128" 保持原样，**不做数值转换**）
//   - 浮点（JSON 数字统一被解析成 float64）→ 去掉小数尾（10001.0 → "10001"）
func codeBuddyNormalizeBizCode(code any) string {
	switch v := code.(type) {
	case string:
		return strings.TrimSpace(v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		// JSON 数字经 gjson/encoding 往往落成 float64；整数形态去掉 ".0"
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return strings.TrimSpace(fmt.Sprintf("%g", v))
	default:
		return ""
	}
}

// CodeBuddyUpstreamEnvelopeMessage 从 CodeBuddy 上游错误信封 {code,msg,...} 里取
// 可读文案（形如 "code 11-128: Illegal API invocation from an unapproved channel"）；
// 非该形态（无顶层 msg）返回空串。
//
// 上游拒因只存在于这个信封里，而通用提取器 extractUpstreamErrorMessage
// 不认顶层 msg，客户端因此只看到 "Upstream error: 400"（2026-09-14 遗留项）。
// 该函数用于把这类不可诊断文案补全。
//
// ⚠️ code 必须按**原值**取（gjson .String()），不能用 .Int()：字符串形态的
// "11-128" 会被 .Int() 吞成 0，导致回显成 "code 0"。
func CodeBuddyUpstreamEnvelopeMessage(body []byte) string {
	msg := strings.TrimSpace(gjson.GetBytes(body, "msg").String())
	if msg == "" {
		return ""
	}
	raw := gjson.GetBytes(body, "code")
	if raw.Exists() && codeBuddyNormalizeBizCode(rawValue(raw)) != "0" {
		return fmt.Sprintf("code %s: %s", codeBuddyBizCodeDisplay(raw), msg)
	}
	return msg
}

// rawValue 把 gjson.Result 还原成用于查表的原生值（保留字符串/数字的区别）。
func rawValue(r gjson.Result) any {
	switch r.Type {
	case gjson.String:
		return r.String()
	case gjson.Number:
		return r.Num
	default:
		return r.String()
	}
}

// codeBuddyBizCodeDisplay 取用于展示的 code 原文（字符串形态保持原样，
// 数字形态去掉小数尾），避免 "11-128" 被显示成 0 或 11。
func codeBuddyBizCodeDisplay(r gjson.Result) string {
	return codeBuddyNormalizeBizCode(rawValue(r))
}
