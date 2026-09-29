package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	logredact "github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

// CodeBuddy **收益流水**：记录各功能每次实际领到的积分。
//
// ## 为什么需要它（这不是"锦上添花"）
//
// 2026-09-29 排查一次"点了按钮还是 0 积分"时发现的真实缺口：
// 我们只能靠**直连上游账单**反推"到底领到没"，因为本地没有收益记录。
// 那次排查中 `travel_run` 实际领到 5 分，但它的回执里 `reward_credit` 是 0
// ——**领了却没记**，用户自然会认为功能坏了。
//
// 有了本模块：
//   - 界面上直接看「今日 +X / 累计 +Y」，不必翻上游；
//   - "领了没记"会立刻暴露（流水空但上游有入账）；
//   - 排查时不必再手工打上游接口。
//
// ## 与既有积分流水（codebuddy_credits_ledger）的分工
//
// 那个是**余额比对式**流水：轮询余额时发现"涨了"就记一条，delta 是**推出来的**。
// 它的盲区正是本次踩到的：如果两次轮询之间既领又花、恰好打平，
// 或者领了但余额被别处消耗掉，**这条流水根本不产生**。
//
// 本模块是**动作式**流水：每个发分动作当场记下"我领到了多少"，来源明确、
// 不依赖余额变化。两者互补——前者反映净变化，后者反映动作事实。
//
// ## 存储
//
// 落 `Account.Extra`，与既有留痕同通道（不新建表，沿用批 3.4 的硬约束）：
//   - `codebuddy_earnings`：环形流水（最新在前，上限 20 条）；
//   - `codebuddy_earnings_total`：累计领取总额（不随环形淘汰而减少）；
//   - `codebuddy_earnings_daily`：{日期 → 当日合计}（保留最近 30 天）。
//
// 累计值**单独存**而不是从环形求和：环形只留 20 条，求和会随淘汰而变小
// （"累计越领越少"这种反直觉现象）。

const (
	codeBuddyEarningsKey         = "codebuddy_earnings"
	codeBuddyEarningsTotalKey    = "codebuddy_earnings_total"
	codeBuddyEarningsDailyKey    = "codebuddy_earnings_daily"
	codeBuddyEarningsMaxEntries  = 20
	codeBuddyEarningsDailyMaxDay = 30
)

// codeBuddyEarningSource 收益来源标识（用于流水展示与分类汇总）。
type codeBuddyEarningSource string

const (
	// codeBuddyEarningCheckin 每日签到。
	codeBuddyEarningCheckin codeBuddyEarningSource = "checkin"
	// codeBuddyEarningAdopt 领养（first_buddy）。
	codeBuddyEarningAdopt codeBuddyEarningSource = "adopt"
	// codeBuddyEarningTravel 猫猫旅行领奖。
	codeBuddyEarningTravel codeBuddyEarningSource = "travel"
	// codeBuddyEarningRedeem 连登档位兑换。
	codeBuddyEarningRedeem codeBuddyEarningSource = "redeem"
	// codeBuddyEarningGift 礼包。
	codeBuddyEarningGift codeBuddyEarningSource = "gift"
	// codeBuddyEarningCompensation 补偿。
	codeBuddyEarningCompensation codeBuddyEarningSource = "compensation"
	// codeBuddyEarningSchool 开学季任务领奖。
	codeBuddyEarningSchool codeBuddyEarningSource = "school"
	// codeBuddyEarningTrial 国际版 trial。
	codeBuddyEarningTrial codeBuddyEarningSource = "trial"
)

// codeBuddyEarningsLocks 进程内 per-account 互斥。
//
// 为什么需要：读-改-写（读 Extra → 追加 → 写回）非原子，两个功能并发发分
// （例如 run-all 里 streak 与 travel 同时领到）会互相覆盖，导致**丢流水**。
// 调度器之间虽串行，但手动端点与自动趟可能同时跑，所以必须加锁。
var codeBuddyEarningsLocks sync.Map // accountID -> *sync.Mutex

func codeBuddyEarningsLock(accountID int64) *sync.Mutex {
	actual, _ := codeBuddyEarningsLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock, _ := actual.(*sync.Mutex)
	if lock == nil {
		return &sync.Mutex{}
	}
	return lock
}

// RecordCodeBuddyEarning 记一笔收益（credit <= 0 时静默跳过）。
//
// 调用点：**每个发分动作成功之后**。detail 是给人看的补充说明
// （如兑换档位 "7d"、礼包名），可为空。
//
// 任何失败只告警、不影响主流程——留痕是旁路（与既有台账同口径）。
func RecordCodeBuddyEarning(
	ctx context.Context,
	repo AccountRepository,
	account *Account,
	source codeBuddyEarningSource,
	credit float64,
	detail string,
) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	// credit <= 0 不记：那些是"没领到"（跳过/幂等/失败），不是收益。
	// 记 0 会让流水被噪音淹没，反而看不清真实收益。
	if credit <= 0 {
		return
	}

	lock := codeBuddyEarningsLock(account.ID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	entry := map[string]any{
		"at":     now.UTC().Format(time.RFC3339),
		"source": string(source),
		"credit": credit,
	}
	if detail != "" {
		entry["detail"] = detail
	}

	// 读现有流水（DB 读回是 []any，同进程刚写是 []map[string]any）。
	entries := make([]map[string]any, 0, codeBuddyEarningsMaxEntries+1)
	switch raw := account.Extra[codeBuddyEarningsKey].(type) {
	case []any:
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	case []map[string]any:
		entries = append(entries, raw...)
	}
	entries = append([]map[string]any{entry}, entries...)
	if len(entries) > codeBuddyEarningsMaxEntries {
		entries = entries[:codeBuddyEarningsMaxEntries]
	}

	// 累计：读现有值 + 本笔。
	total := codeBuddyFloatAny(account.Extra[codeBuddyEarningsTotalKey]) + credit

	// 按当地日期（UTC+8，与积分口径一致）汇总当日。
	localDay := codeBuddyGrowthLocalDay(now)
	daily := codeBuddyEarningsDaily(account.Extra[codeBuddyEarningsDailyKey])
	daily[localDay] = daily[localDay] + credit
	daily = codeBuddyTrimEarningsDaily(daily, localDay)

	updates := map[string]any{
		codeBuddyEarningsKey:      entries,
		codeBuddyEarningsTotalKey: total,
		codeBuddyEarningsDailyKey: daily,
	}
	// 就地更新内存，让同一轮里后续读取（如回执汇总）能立刻看到。
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[codeBuddyEarningsKey] = entries
	account.Extra[codeBuddyEarningsTotalKey] = total
	account.Extra[codeBuddyEarningsDailyKey] = daily

	if err := repo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("codebuddy earnings write failed",
			slog.Int64("account_id", account.ID),
			slog.String("source", string(source)),
			slog.String("error", logredact.RedactText(err.Error())))
		return
	}
	slog.Info("codebuddy_earning_recorded",
		"account_id", account.ID,
		"source", string(source),
		"credit", credit,
		"total", total)
}

// codeBuddyEarningsDaily 解析当日汇总 map（容错各种 DB 读回形态）。
//
// 值可能是 float64 / int / json.Number / 字符串数字——统一走 codeBuddyFloatAny，
// 保证不会因为类型差异静默读成 0（那会让"今日收益"永远显示 0）。
func codeBuddyEarningsDaily(raw any) map[string]float64 {
	out := map[string]float64{}
	switch m := raw.(type) {
	case map[string]any:
		for day, v := range m {
			out[day] = codeBuddyFloatAny(v)
		}
	case map[string]float64:
		for day, v := range m {
			out[day] = v
		}
	}
	return out
}

// codeBuddyTrimEarningsDaily 只保留最近 maxDay 天（按日期键排序，字典序即时间序）。
func codeBuddyTrimEarningsDaily(daily map[string]float64, keepDay string) map[string]float64 {
	if len(daily) <= codeBuddyEarningsDailyMaxDay {
		return daily
	}
	days := make([]string, 0, len(daily))
	for day := range daily {
		days = append(days, day)
	}
	// 日期串是 YYYY-MM-DD，字典序排序即时间序。
	for i := 0; i < len(days); i++ {
		for j := i + 1; j < len(days); j++ {
			if days[j] < days[i] {
				days[i], days[j] = days[j], days[i]
			}
		}
	}
	keep := map[string]float64{}
	for i := len(days) - codeBuddyEarningsDailyMaxDay; i < len(days); i++ {
		if i >= 0 {
			keep[days[i]] = daily[days[i]]
		}
	}
	if _, ok := keep[keepDay]; !ok {
		keep[keepDay] = daily[keepDay]
	}
	return keep
}

// codeBuddyEarningsSummary 一次收益汇总（读取侧 DTO）。
type codeBuddyEarningsSummary struct {
	// TodayCredit 今日（UTC+8）合计。
	TodayCredit float64 `json:"today_credit"`
	// TotalCredit 累计合计（不随环形淘汰减少）。
	TotalCredit float64 `json:"total_credit"`
	// Entries 最近流水（最新在前）。
	Entries []map[string]any `json:"entries,omitempty"`
	// Daily 近 30 天按日合计。
	Daily map[string]float64 `json:"daily,omitempty"`
	// BySource 按来源累计（从流水现状推导，供界面分类展示）。
	BySource map[string]float64 `json:"by_source,omitempty"`
}

// codeBuddyEarningsFromAccount 从账号 Extra 读收益汇总（只读，不改状态）。
func codeBuddyEarningsFromAccount(account *Account) codeBuddyEarningsSummary {
	summary := codeBuddyEarningsSummary{BySource: map[string]float64{}}
	if account == nil || account.Extra == nil {
		return summary
	}
	summary.TotalCredit = codeBuddyFloatAny(account.Extra[codeBuddyEarningsTotalKey])
	summary.Daily = codeBuddyEarningsDaily(account.Extra[codeBuddyEarningsDailyKey])
	summary.TodayCredit = summary.Daily[codeBuddyGrowthLocalDay(time.Now())]

	switch raw := account.Extra[codeBuddyEarningsKey].(type) {
	case []any:
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				summary.Entries = append(summary.Entries, m)
			}
		}
	case []map[string]any:
		summary.Entries = append(summary.Entries, raw...)
	}
	// 按来源归类：从**流水**推导（只覆盖最近 20 条）。
	//
	// ⚠️ 这是"最近收益的构成"，不是"历史每类各领了多少"——后者需要每类单独
	// 累计。当前不单独维护：来源分类是辅助展示，累计总额才是关键数字，
	// 而为每类加一个持久化计数器会让 Extra 键数量随通道增加而膨胀。
	// 若日后要精确的分类累计，再加 `codebuddy_earnings_by_source` 键。
	for _, entry := range summary.Entries {
		source, _ := entry["source"].(string)
		if source == "" {
			continue
		}
		summary.BySource[source] += codeBuddyFloatAny(entry["credit"])
	}
	if len(summary.Entries) == 0 {
		summary.Entries = nil
	}
	if len(summary.BySource) == 0 {
		summary.BySource = nil
	}
	return summary
}

// CodeBuddyEarningsText 一句话收益文案（供回执/界面展示）。
func CodeBuddyEarningsText(summary codeBuddyEarningsSummary) string {
	if summary.TotalCredit <= 0 && summary.TodayCredit <= 0 {
		return "尚无收益记录"
	}
	return fmt.Sprintf("今日 +%s / 累计 +%s",
		codeBuddyFormatCredit(summary.TodayCredit),
		codeBuddyFormatCredit(summary.TotalCredit))
}

// codeBuddyFormatCredit 积分格式化：整数不带小数点，小数最多两位。
func codeBuddyFormatCredit(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}
