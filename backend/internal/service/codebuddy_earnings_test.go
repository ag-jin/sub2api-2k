package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 收益流水（codebuddy_earnings.go）的行为测试。
//
// ## 这个模块为什么存在（回归的由来）
//
// 2026-09-29 排查"点了按钮还是 0 积分"时发现：`travel_run` 实际领到了 5 分，
// 但回执里 `reward_credit` 是 0 —— **领了却没记**。用户据此认为功能坏了，
// 而排查只能靠直连上游账单反推。
//
// 所以本测试的核心不是"能不能记"，而是**每一个发分动作都真的记了**
// （漏一个就会出现"领了看不见"，而那正是本次的病灶）。

// codeBuddyEarningsTestRepo 只实现收益记录所需的 GetByID / UpdateExtra。
type codeBuddyEarningsTestRepo struct {
	AccountRepository
	accounts map[int64]*Account
	writes   []map[string]any
}

func (r *codeBuddyEarningsTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if acct, ok := r.accounts[id]; ok {
		return acct, nil
	}
	return nil, ErrAccountNotFound
}

func (r *codeBuddyEarningsTestRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.writes = append(r.writes, updates)
	return nil
}

func newCodeBuddyEarningsTestRepo(accounts ...*Account) *codeBuddyEarningsTestRepo {
	repo := &codeBuddyEarningsTestRepo{accounts: map[int64]*Account{}}
	for _, acct := range accounts {
		repo.accounts[acct.ID] = acct
	}
	return repo
}

func newEarningsTestAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformCodeBuddy,
		Extra:    map[string]any{},
	}
}

// --- 基础记录行为 ---

// Scenario：记一笔 → 流水、累计、当日汇总三者都更新。
func TestRecordCodeBuddyEarningUpdatesLedgerTotalAndDaily(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 100, "")

	summary := codeBuddyEarningsFromAccount(account)
	require.Equal(t, float64(100), summary.TotalCredit, "累计应 +100")
	require.Equal(t, float64(100), summary.TodayCredit, "今日应 +100")
	require.Len(t, summary.Entries, 1, "应有一条流水")
	require.Equal(t, "checkin", summary.Entries[0]["source"])
	require.Equal(t, float64(100), summary.Entries[0]["credit"])
	require.NotEmpty(t, repo.writes, "应经仓储落库（不只改内存）")
}

// Scenario：credit <= 0 **不记**（跳过/幂等/失败不是收益）。
//
// 这条很关键：签到幂等返回 `credit: 0`、通道跳过时也是 0。
// 若把 0 也记进流水，流水会被噪音淹没，"今日收益"也就失去意义。
func TestRecordCodeBuddyEarningSkipsNonPositive(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	for _, credit := range []float64{0, -1, -100} {
		RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, credit, "")
	}

	summary := codeBuddyEarningsFromAccount(account)
	require.Equal(t, float64(0), summary.TotalCredit, "非正数不应计入累计")
	require.Empty(t, summary.Entries, "非正数不应产生流水")
	require.Empty(t, repo.writes, "非正数不应触发落库")
}

// Scenario：多笔累加，且**累计不随环形淘汰而减少**。
//
// 若累计是从环形求和推出来的，超过上限后"累计越领越少"——反直觉且错误。
// 所以累计单独存键，本用例钉住这个设计。
func TestRecordCodeBuddyEarningTotalSurvivesRingEviction(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	// 写满并超出环形上限（20）。
	total := 0.0
	for i := 0; i < codeBuddyEarningsMaxEntries+10; i++ {
		RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 10, "")
		total += 10
	}

	summary := codeBuddyEarningsFromAccount(account)
	require.Len(t, summary.Entries, codeBuddyEarningsMaxEntries,
		"环形应封顶在 %d 条", codeBuddyEarningsMaxEntries)
	require.Equal(t, total, summary.TotalCredit,
		"累计必须是全部收益之和（%v），不能因环形淘汰而变小", total)
}

// Scenario：流水最新在前（面板直接按序渲染，不需要前端排序）。
func TestRecordCodeBuddyEarningNewestFirst(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 100, "first")
	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningTravel, 5, "second")

	summary := codeBuddyEarningsFromAccount(account)
	require.Len(t, summary.Entries, 2)
	require.Equal(t, "travel", summary.Entries[0]["source"], "最新的应在最前")
	require.Equal(t, "checkin", summary.Entries[1]["source"])
}

// Scenario：按来源归类（供界面分类展示）。
func TestRecordCodeBuddyEarningGroupsBySource(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 100, "")
	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 100, "")
	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningAdopt, 300, "")

	summary := codeBuddyEarningsFromAccount(account)
	require.Equal(t, float64(200), summary.BySource["checkin"])
	require.Equal(t, float64(300), summary.BySource["adopt"])
	require.Equal(t, float64(500), summary.TotalCredit)
}

// Scenario：DB 读回形态（[]any 而非 []map）也能正确解析。
//
// 这条防的是**类型断言漏一种形态**：Extra 经 JSON 往返后切片元素是 `any`，
// 而同进程刚写的是 `[]map[string]any`。只处理一种会让"重启后流水消失"。
func TestCodeBuddyEarningsParsesDBRoundTripShape(t *testing.T) {
	account := &Account{
		ID:       1,
		Platform: PlatformCodeBuddy,
		Extra: map[string]any{
			codeBuddyEarningsKey: []any{
				map[string]any{"source": "checkin", "credit": float64(100)},
				map[string]any{"source": "travel", "credit": float64(5)},
			},
			codeBuddyEarningsTotalKey: float64(105),
		},
	}

	summary := codeBuddyEarningsFromAccount(account)
	require.Len(t, summary.Entries, 2, "[]any 形态必须能解析（否则重启后流水全空）")
	require.Equal(t, float64(105), summary.TotalCredit)
	require.Equal(t, float64(100), summary.BySource["checkin"])
}

// Scenario：nil 账号 / nil 仓储 / 空 Extra 都不 panic。
func TestRecordCodeBuddyEarningHandlesNils(t *testing.T) {
	require.NotPanics(t, func() {
		RecordCodeBuddyEarning(context.Background(), nil, nil, codeBuddyEarningCheckin, 100, "")
		RecordCodeBuddyEarning(context.Background(), nil, newEarningsTestAccount(1), codeBuddyEarningCheckin, 100, "")
		RecordCodeBuddyEarning(context.Background(), newCodeBuddyEarningsTestRepo(), &Account{ID: 0}, codeBuddyEarningCheckin, 100, "")
	})
	require.Empty(t, codeBuddyEarningsFromAccount(nil).Entries)
	require.Equal(t, float64(0), codeBuddyEarningsFromAccount(&Account{ID: 1}).TotalCredit)
}

// Scenario：并发记账不丢流水（读-改-写有 per-account 互斥保护）。
//
// 真实场景：run-all 里 streak 与 travel 可能各自领到分，若不加锁会互相覆盖。
func TestRecordCodeBuddyEarningConcurrentDoesNotLoseEntries(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	const goroutines = 10
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 10, "")
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}

	summary := codeBuddyEarningsFromAccount(account)
	require.Equal(t, float64(goroutines*10), summary.TotalCredit,
		"并发 %d 笔应全部计入累计（丢说明互斥失效）", goroutines)
}

// Scenario：当日汇总按 UTC+8 日期，且不会无限增长。
func TestCodeBuddyEarningsDailyKeyedByLocalDay(t *testing.T) {
	account := newEarningsTestAccount(1)
	repo := newCodeBuddyEarningsTestRepo(account)

	RecordCodeBuddyEarning(context.Background(), repo, account, codeBuddyEarningCheckin, 100, "")

	day := codeBuddyGrowthLocalDay(time.Now())
	summary := codeBuddyEarningsFromAccount(account)
	require.Equal(t, float64(100), summary.Daily[day], "当日汇总应以 UTC+8 日期为键")
}

// --- 接入完整性（防"某个功能忘了记账"）---

// Scenario：**每个发分动作都接了记账**（源码级断言）。
//
// 这是本模块存在的理由的直接守门：漏掉任一处就会出现"领了看不见"。
// 用源码扫描而不是逐功能跑集成测试：后者要为每个功能造出"能领到分"的上游
// stub（领养要过 chat_5、旅行要有 record_id、兑换要够档位…），成本高且脆弱；
// 而"该记分的地方有没有调用 RecordCodeBuddyEarning"是个**静态可查**的事实。
func TestEveryCreditGrantPointRecordsEarning(t *testing.T) {
	cases := []struct {
		file    string
		why     string
		markers []string
	}{
		{
			file:    "codebuddy_admin_service.go",
			why:     "签到（每日主要来源）",
			markers: []string{"codeBuddyEarningCheckin"},
		},
		{
			file:    "codebuddy_growth_channel_travel.go",
			why:     "领养（300 分）+ 旅行领奖（本次漏记的那笔）",
			markers: []string{"codeBuddyEarningAdopt", "codeBuddyEarningTravel"},
		},
		{
			file:    "codebuddy_growth_channel_streak.go",
			why:     "兑换 / 礼包 / 补偿",
			markers: []string{"codeBuddyEarningRedeem", "codeBuddyEarningGift", "codeBuddyEarningCompensation"},
		},
		{
			file:    "codebuddy_growth_school_run.go",
			why:     "开学季领奖",
			markers: []string{"codeBuddyEarningSchool"},
		},
		{
			file:    "codebuddy_growth_channel_school_trial.go",
			why:     "国际版 trial",
			markers: []string{"codeBuddyEarningTrial"},
		},
	}
	for _, tc := range cases {
		source := readServiceFile(t, tc.file)
		// ⚠️ 用 if + t.Errorf 而不是 require.Contains：后者在失败时会把
		// **整个文件内容**打进错误信息（实测输出上千行，完全没法看）。
		// 这里只要一句"哪个文件的哪个标记缺失"。
		if !strings.Contains(source, "RecordCodeBuddyEarning") {
			t.Errorf("%s：%s 的发分点未接入收益流水（找不到 RecordCodeBuddyEarning 调用）",
				tc.file, tc.why)
		}
		for _, marker := range tc.markers {
			if !strings.Contains(source, marker) {
				t.Errorf("%s：缺少 %s 的记账调用（%s）——会出现「领了看不见」",
					tc.file, marker, tc.why)
			}
		}
	}
}

// Scenario：文案在无收益时给出"尚无记录"而不是"今日 +0 / 累计 +0"。
func TestCodeBuddyEarningsText(t *testing.T) {
	require.Contains(t, CodeBuddyEarningsText(codeBuddyEarningsSummary{}), "尚无")
	text := CodeBuddyEarningsText(codeBuddyEarningsSummary{TodayCredit: 100, TotalCredit: 1295})
	require.Contains(t, text, "100")
	require.Contains(t, text, "1295")
}

// Scenario：小数金额格式化为两位（积分可能有小数，如 4.35）。
func TestCodeBuddyFormatCredit(t *testing.T) {
	require.Equal(t, "100", codeBuddyFormatCredit(100))
	require.Equal(t, "0", codeBuddyFormatCredit(0))
	require.Equal(t, "4.35", codeBuddyFormatCredit(4.35))
	require.Equal(t, "5", codeBuddyFormatCredit(5.0))
}

// readServiceFile 读取 internal/service 下的源码（源码级接线断言用）。
//
// 用 os.ReadFile 而不是 go/parser：这里只做"字符串是否出现"的检查，
// 解析失败时要能明确报"读不到"（而不是静默空串让 Contains 恒假）。
func readServiceFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	require.NoError(t, err, "读不到 %s（测试失效，不是通过）", name)
	return string(raw)
}
