package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	logredact "github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

// CodeBuddy 积分变动留痕（批 3.4）。
//
// 目标：每次**真实查询成功后**比对上次余额，**增加**即写一条流水；需要去重键；
// **首见账号只建基线、不记流水**（否则会把"首次查询"误记成"刚获得一大笔"）。
// 参照 REF-B `workbuddy-manager/server/services/credits.py:record_balance`。
//
// 为什么用 Account.Extra 而不是审计日志（`AuditLogService.Record`）：
//   - 审计通道是**入站管理请求**的留痕（有 actor / method / path / request_id 语义），
//     积分流水来自**出站轮询**，没有 actor 可写，硬塞会让审计语义污染；
//   - `Account.Extra` 已是有账号上下文的键值留痕通道（签到留痕
//     `last_checkin_at` / `streak_days` 同通道，`UpdateExtra` 原子合并写）；
//   - 本批**不新建表**（任务书硬约束）。
//
// 具体形状：
//   - `codebuddy_credits_ledger`：单条流水 {at, delta, balance, prev}（**只留最近一条**，
//     避免 Extra 无限膨胀；环形历史留给后续批次按需扩展）；
//   - `codebuddy_credits_ledger_dedup`：去重键 `balance` 的字符串化——同一余额值只留一条；
//   - `codebuddy_credits_ledger_balance` / `..._at`：上次余额基线（首见只写这两个）。
//
// ⚠️ 基线并发：Extra 的写入是"合并"而不是"比较并交换"，两个并发查询可能都读到
// 旧基线 → 各写一条。故这里用进程内 per-account 互斥 + 短窗口节流兜底（同一账号
// 在 minInterval 内不重复落基线），跨实例仍有极小重复概率，可接受（流水是观测
// 语义，不是账务语义）。
const (
	codeBuddyCreditsLedgerExtraKey      = "codebuddy_credits_ledger"
	codeBuddyCreditsLedgerDedupKey      = "codebuddy_credits_ledger_dedup"
	codeBuddyCreditsLedgerBalanceKey    = "codebuddy_credits_ledger_balance"
	codeBuddyCreditsLedgerBaselineAtKey = "codebuddy_credits_ledger_baseline_at"

	// codeBuddyCreditsMaxDelta 单次"增加"的记录上限：超过视为异常值/重置后混算，
	// 不记流水（REF-B `_MAX_DELTA = 1_000_000` 同款防御）。
	codeBuddyCreditsMaxDelta = 1_000_000
	// codeBuddyCreditsBaselineMinInterval 同一账号两次基线写入的最小间隔。real query
	// 本身已被 3min 缓存与单飞收敛，这层是给"强制刷新"场景兜底。
	codeBuddyCreditsBaselineMinInterval = 30 * time.Second
)

// codeBuddyCreditsLedgerLocks 进程内 per-account 基线互斥（见文件头并发说明）。
var codeBuddyCreditsLedgerLocks sync.Map // accountID -> *sync.Mutex

func codeBuddyCreditsLedgerLock(accountID int64) *sync.Mutex {
	actual, _ := codeBuddyCreditsLedgerLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock, _ := actual.(*sync.Mutex)
	if lock == nil {
		return &sync.Mutex{}
	}
	return lock
}

// recordCodeBuddyCreditsChange 比对上次余额并在**增加**时写一条流水。
// 任何写失败只告警、不影响余额查询结果（留痕是旁路）。
func (s *AccountUsageService) recordCodeBuddyCreditsChange(ctx context.Context, account *Account, snapshot *UpstreamBalanceUsage, now time.Time) {
	if s == nil || s.accountRepo == nil || account == nil || snapshot == nil || snapshot.Balance == nil {
		return
	}
	current := *snapshot.Balance
	lock := codeBuddyCreditsLedgerLock(account.ID)
	lock.Lock()
	defer lock.Unlock()

	updates := map[string]any{}
	if previous, ok := codeBuddyCreditsLedgerBaseline(account); ok {
		// 节流：距上次基线写入过近时只更新当前值、不判流水（并发/强制刷新兜底）。
		if now.Sub(previous.at) >= codeBuddyCreditsBaselineMinInterval {
			delta := current - previous.balance
			if delta > 0 && delta <= codeBuddyCreditsMaxDelta {
				updates[codeBuddyCreditsLedgerExtraKey] = map[string]any{
					"at":      now.UTC().Format(time.RFC3339),
					"delta":   delta,
					"balance": current,
					"prev":    previous.balance,
				}
				// 去重键用余额值：同一余额只会留下一条流水。
				updates[codeBuddyCreditsLedgerDedupKey] = codeBuddyCreditsDedupKey(current)
			}
		}
	}
	updates[codeBuddyCreditsLedgerBalanceKey] = current
	updates[codeBuddyCreditsLedgerBaselineAtKey] = now.UTC().Format(time.RFC3339)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("codebuddy credits ledger write failed",
			slog.Int64("account_id", account.ID),
			slog.String("error", logredact.RedactText(err.Error())))
	}
}

type codeBuddyCreditsBaseline struct {
	balance float64
	at      time.Time
}

// codeBuddyCreditsLedgerBaseline 从 Account.Extra 读上次余额基线。
// 首见账号（无基线键）→ false，调用方只建基线、不记流水。
func codeBuddyCreditsLedgerBaseline(account *Account) (codeBuddyCreditsBaseline, bool) {
	if account == nil || account.Extra == nil {
		return codeBuddyCreditsBaseline{}, false
	}
	raw, exists := account.Extra[codeBuddyCreditsLedgerBalanceKey]
	if !exists {
		return codeBuddyCreditsBaseline{}, false
	}
	balance := codeBuddyFloatAny(raw)
	// 基线时间缺失时按零值处理：不是"刚写过"，因此允许判定流水。
	at, err := time.Parse(time.RFC3339, codeBuddyStr(account.Extra[codeBuddyCreditsLedgerBaselineAtKey]))
	if err != nil {
		return codeBuddyCreditsBaseline{balance: balance}, true
	}
	return codeBuddyCreditsBaseline{balance: balance, at: at}, true
}

// codeBuddyCreditsDedupKey 去重键：同一余额值只留一条流水。
func codeBuddyCreditsDedupKey(balance float64) string {
	return "codebuddy-credits|" + codeBuddyStr(balance)
}
