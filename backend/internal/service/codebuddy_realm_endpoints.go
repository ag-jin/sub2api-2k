package service

// CodeBuddy 出站端点按品牌域（realm）分发（P0-3）。
//
// 背景：CN（国内版）与 global（国际版）不只是域名不同，**billing/meter 族的
// 路径与回落方向也不同**——global 以**无 `/v2` 前缀**的 `/billing/meter/*` 优先、
// 404 才回落带 `/v2` 的形式；CN 只有带 `/v2` 一种形式。**两边顺序相反**，
// 照上游实现，不是笔误。
//
// 参照实现（只读核对，均为参考实现的源码事实，非猜测）：
//   - workbuddy2api `internal/upstream/client.go:862-895`
//     （`billingBase()` / `billingMeterPaths()` / `checkinMeterPaths()`：注释原文
//     "global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）"）
//   - workbuddy-manager `server/services/realm.py:256-283`
//     （`billing_base()` / `billing_paths()`：注释原文 "**注意两边顺序相反**"）
//
// ⚠️ 未经真机验证：global 分支按上述源码事实实现，但**本轮没有 global 测试账号**，
// 未对上游 `www.workbuddy.ai` 发过真实请求。CN 分支为现状逐字（零回归）。
// 落地后首次接入 global 账号时，必须实测 `get-user-resource` / `daily-checkin`
// 的路径回落是否按预期工作（404 换下一候选）。
const (
	// CodeBuddyBillingBaseCN 国内版计费域（余额查询 / 签到）。
	CodeBuddyBillingBaseCN = "https://www.codebuddy.cn"
	// CodeBuddyBillingBaseGlobal 国际版计费域【未经真机验证，见文件头】。
	CodeBuddyBillingBaseGlobal = "https://www.workbuddy.ai"

	// CodeBuddyChatBaseCN 国内版对话与登录流程域。
	CodeBuddyChatBaseCN = "https://copilot.tencent.com"
	// CodeBuddyChatBaseGlobal 国际版对话与登录流程域【未经真机验证，见文件头】。
	//
	// ⚠️ 此处**不带** `/v2` 前缀：版本段由路径常量
	// `CodeBuddyChatCompletionsPath`（`/v2/chat/completions`）承担，与 CN 同构。
	// 三份参考实现一致：workbuddy2api `client.go:729,743,1029`（ChatBaseCN =
	// copilot.tencent.com、defaultGlobalBase = www.workbuddy.ai、chatCompletionsPath
	// = /v2/chat/completions，两域共用同一路径常量）、workbuddy-manager
	// `realm.py:248-253,276`（global chat base = www.workbuddy.ai，chat paths 恒
	// /v2/chat/completions）、codebuddy2api。全三仓 grep `workbuddy.ai/v2` 零命中。
	CodeBuddyChatBaseGlobal = "https://www.workbuddy.ai"
)

// billing/meter 族路径候选：两域的**顺序相反**（见文件头注释）。
const (
	// CodeBuddyUserResourcePath 实时积分查询端点（无版本段形式，global 首选）。
	CodeBuddyUserResourcePath = "/billing/meter/get-user-resource"
	// CodeBuddyUserResourcePathV2 实时积分查询端点（CN 现状逐字；global 404 回落项）。
	CodeBuddyUserResourcePathV2 = "/v2/billing/meter/get-user-resource"
	// CodeBuddyDailyCheckinPath 每日签到端点（无版本段形式，global 首选）。
	CodeBuddyDailyCheckinPath = "/billing/meter/daily-checkin"
	// CodeBuddyDailyCheckinPathV2 每日签到端点（CN 现状逐字；global 404 回落项）。
	CodeBuddyDailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// CodeBuddyBillingBase 按账号 realm 返回计费域 base。
func CodeBuddyBillingBase(a *Account) string {
	if codeBuddyAccountRealm(a) == codeBuddyRealmKindGlobal {
		return CodeBuddyBillingBaseGlobal
	}
	return CodeBuddyBillingBaseCN
}

// CodeBuddyChatBase 按账号 realm 返回对话/登录基址 base。
// 注意：计费域与 chat 域在 CN 下是**两个不同主机**（www.codebuddy.cn 与
// copilot.tencent.com），不要混用。
func CodeBuddyChatBase(a *Account) string {
	if codeBuddyAccountRealm(a) == codeBuddyRealmKindGlobal {
		return CodeBuddyChatBaseGlobal
	}
	return CodeBuddyChatBaseCN
}

// CodeBuddyUserResourcePaths 按 realm 返回积分查询的路径候选序列（按尝试顺序）：
// global → [无 /v2, 有 /v2]（404 回落）；cn → [有 /v2]（单元素，现状逐字）。
func CodeBuddyUserResourcePaths(a *Account) []string {
	if codeBuddyAccountRealm(a) == codeBuddyRealmKindGlobal {
		return []string{CodeBuddyUserResourcePath, CodeBuddyUserResourcePathV2}
	}
	return []string{CodeBuddyUserResourcePathV2}
}

// CodeBuddyDailyCheckinPaths 同 CodeBuddyUserResourcePaths，针对签到端点。
func CodeBuddyDailyCheckinPaths(a *Account) []string {
	if codeBuddyAccountRealm(a) == codeBuddyRealmKindGlobal {
		return []string{CodeBuddyDailyCheckinPath, CodeBuddyDailyCheckinPathV2}
	}
	return []string{CodeBuddyDailyCheckinPathV2}
}
