package service

import (
	"fmt"
	"strings"
)

// 立即执行的**结果文案**（服务端生成，设置页直接展示）。
//
// ## 为什么文案由服务端生成而不是前端拼
//
// 设置页是平台无关的（`usePlatformFeatures` 不为任何平台写死），各功能回执
// 形状差异又很大（签到四态计数 / 成长链通道汇总 / 上报条数）。让前端按
// featureKey 分支去拼文案，等于把"谁是什么语义"的知识搬回前端——正是注册表
// 架构要避免的耦合。
//
// 所以各功能自己把回执压成一句话（本文件），设置页只负责显示 `Summary`。
//
// ## 文案口径
//
// 统一用「已完成 N / 跳过 N / 失败 N」的中性表述，**不谎报成功**：
// 只要 `failed > 0` 就必须在文案里出现，否则运维看到"成功"会以为没事。
// 中文文案直接写在这里（不进 i18n）：它是**服务端回执**，与各平台的日志/
// 审计文案同源；前端 i18n 管的是界面固定文案，两者职责不同。

// codeBuddyCheckinSummaryText 签到批量结果的一句话。
func codeBuddyCheckinSummaryText(summary *CodeBuddyCheckinBatchResponse) string {
	if summary == nil {
		return "签到：无结果"
	}
	parts := []string{
		fmt.Sprintf("成功 %d", summary.Succeeded),
	}
	if summary.AlreadyCheckedIn > 0 {
		parts = append(parts, fmt.Sprintf("已签 %d", summary.AlreadyCheckedIn))
	}
	if summary.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("跳过 %d", summary.Skipped))
	}
	if summary.Failed > 0 {
		// 失败必须出现，且放在最后（读者最容易看到的位置）。
		parts = append(parts, fmt.Sprintf("**失败 %d**", summary.Failed))
	}
	text := fmt.Sprintf("签到完成（共 %d 个账号）：%s", summary.Total, strings.Join(parts, " / "))
	if summary.CreditEarned > 0 {
		// CreditEarned 是 float64（积分可能有小数），用 %g 避免印出 100.000000。
		text += fmt.Sprintf("，获得 %g 积分", summary.CreditEarned)
	}
	return text
}

// codeBuddyActivitySummaryText 活跃上报结果的一句话。
func codeBuddyActivitySummaryText(summary CodeBuddyActivityRunSummary) string {
	if summary.Error != "" {
		return "活跃上报失败：" + summary.Error
	}
	text := fmt.Sprintf("活跃上报完成（共 %d 个账号）：已报 %d / 跳过 %d",
		summary.Attempted, summary.Reported, summary.Skipped)
	if summary.Failed > 0 {
		text += fmt.Sprintf(" / **失败 %d**", summary.Failed)
	}
	// 自检异常是"发出去了但可能被上游静默丢弃"的信号（坑 1），必须显式提示——
	// 只报 reported 会让人以为一定点亮了。
	if summary.SelfCheckSuspicious > 0 {
		text += fmt.Sprintf("；⚠️ %d 个账号自检异常（可能被上游静默丢弃）",
			summary.SelfCheckSuspicious)
	}
	return text
}

// codeBuddyGrowthSummaryText 成长链一轮结果的一句话。
func codeBuddyGrowthSummaryText(summary CodeBuddyGrowthRunSummary) string {
	if summary.Error != "" {
		return "成长任务失败：" + summary.Error
	}
	text := fmt.Sprintf("成长任务完成（共 %d 个账号）：成功 %d / 跳过 %d",
		summary.Attempted, summary.Succeeded, summary.Skipped)
	if summary.Failed > 0 {
		text += fmt.Sprintf(" / **失败 %d**", summary.Failed)
	}
	if len(summary.Channels) > 0 {
		text += fmt.Sprintf("；通道：%s", strings.Join(summary.Channels, "、"))
	}
	return text
}
