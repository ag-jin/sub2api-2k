package service

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// CodeBuddy 积分耗尽的**解冻闭环**与错误码冷却规则（A4 批 4.5/4.6）。
//
// 现状缺口：积分耗尽后账号被摘出池子，但**没有任何自动恢复路径** ——
// CN 供应商的余额周期检测（CNProviderBalanceCheckService）平台白名单只有
// kimi/deepseek，codebuddy 不在其中。于是"签到补回积分"这件事发生了，账号却
// 一直停在停调状态，必须管理员手动恢复。
//
// 闭环：签到成功 / 余额查询成功 → 余额恢复到阈值以上 → 清除**本模块写入的**
// 硬冷却。只清自己写的（reason 前缀匹配），不碰其他子系统设的停调 —— 参考实现
// ReenableIfCredits 里也有同样的边界（不复活 disabled 账号）。
//
// 阈值口径与参考实现一致：**remain > 0 即恢复**。积分是"能不能用"的开关，
// 不做二次百分比阈值 —— 只有 1 分也确实能发一次请求，继续停调是错的。

const (
	// codeBuddyCreditsExhaustedReasonPrefix 本模块写入的硬冷却理由前缀。
	// 用前缀而不是全等：理由里带当时的余额数值，全等匹配不上。
	codeBuddyCreditsExhaustedReasonPrefix = "CodeBuddy 积分耗尽"

	// codeBuddyCreditsExhaustedCooldown 积分耗尽后的冷却时长。
	// 取 6 小时而不是永久：积分可以通过签到/购买恢复，而永久停调需要人工解；
	// 6 小时也远长于普通限流冷却，避免耗尽期间被反复调度空转。
	codeBuddyCreditsExhaustedCooldown = 6 * time.Hour
)

// CodeBuddyCreditsExhaustedBizCode 上游"积分耗尽"业务码（A2 已录入码表）。
const CodeBuddyCreditsExhaustedBizCode = 14018

// CodeBuddyModelRateLimitBizCode 上游"该模型使用量超限"业务码。
// 语义是**模型级**：只该冷这个模型，不该把整个账号摘出去（同一账号换模型仍可用）。
const CodeBuddyModelRateLimitBizCode = 6004

// codeBuddyBizCodeAccountCooldown 账号级业务码 → 冷却时长。
// 不含 6004：它是模型级，走 model_rate_limit（见 ApplyCodeBuddyBizCodeCooldown）。
// 不含 11102（该账号后端无此模型）：同样是模型维度的问题，冷账号没道理。
var codeBuddyBizCodeAccountCooldown = map[int]time.Duration{
	// 积分耗尽：等签到/充值恢复，给一个足以跨过签到窗口的冷却。
	CodeBuddyCreditsExhaustedBizCode: codeBuddyCreditsExhaustedCooldown,
	// 授权封禁：需要管理员重新登录该账号，账号级冷却到人工处理为止。
	11140: 24 * time.Hour,
	// trial 未激活：补完注册流程可自愈，但短时间重试无意义。
	14017: 6 * time.Hour,
}

// CodeBuddyCooldownDecision 一次业务码冷却判定的结果。
type CodeBuddyCooldownDecision struct {
	// Scope 冷却范围：account（整号）或 model（仅该模型）。
	Scope string
	// Duration 冷却时长（model 范围下为模型级限流的重置时间跨度）。
	Duration time.Duration
	// Reason 写入停调理由的可读文案。
	Reason string
	// Handled 是否由本判定接管（false = 交给既有规则引擎，如 temp_unschedulable_rules）。
	Handled bool
}

const (
	codeBuddyCooldownScopeAccount = "account"
	codeBuddyCooldownScopeModel   = "model"
)

// CodeBuddyBizCodeCooldown 把上游业务码映射为冷却决策。
//
// 未被本表覆盖的码返回 Handled=false：交给既有 TempUnschedulableRule 引擎
// （按账号 credentials 配置的规则匹配）处理，本模块不抢管。
func CodeBuddyBizCodeCooldown(bizCode int) CodeBuddyCooldownDecision {
	// 6004 模型级：**只冷模型不冷账号**。若按账号级冷却，同一账号的其他模型
	// 会被一起摘出去——上游明确说了"仅影响该模型，切换其他模型可立即使用"。
	if bizCode == CodeBuddyModelRateLimitBizCode {
		return CodeBuddyCooldownDecision{
			Scope:    codeBuddyCooldownScopeModel,
			Duration: 30 * time.Minute,
			Reason:   "CodeBuddy 该模型使用量超限",
			Handled:  true,
		}
	}
	duration, ok := codeBuddyBizCodeAccountCooldown[bizCode]
	if !ok {
		return CodeBuddyCooldownDecision{}
	}
	return CodeBuddyCooldownDecision{
		Scope:    codeBuddyCooldownScopeAccount,
		Duration: duration,
		Reason:   codeBuddyCooldownReasonFor(bizCode),
		Handled:  true,
	}
}

func codeBuddyCooldownReasonFor(bizCode int) string {
	if bizCode == CodeBuddyCreditsExhaustedBizCode {
		return codeBuddyCreditsExhaustedReasonPrefix
	}
	return "CodeBuddy 上游拒绝（code " + codeBuddyIntToString(bizCode) + "）"
}

func codeBuddyIntToString(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := make([]byte, 0, 12)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// codeBuddyCooldownApplier 网关侧消费的窄接口（避免 gateway 硬依赖
// CodeBuddyAdminService 的具体类型，也便于测试注入桩）。
type codeBuddyCooldownApplier interface {
	ApplyCodeBuddyBizCodeCooldown(ctx context.Context, account *Account, bizCode int, modelName string) bool
}

// SetCodeBuddyCooldownApplier 注入业务码冷却的执行者（wire 里调一次）。
func (s *OpenAIGatewayService) SetCodeBuddyCooldownApplier(applier codeBuddyCooldownApplier) {
	if s == nil {
		return
	}
	s.codeBuddyCooldownApplier = applier
}

// ApplyCodeBuddyBizCodeCooldown 按业务码施加冷却。
//
// account 范围 → SetTempUnschedulable（整号临时停调）；
// model 范围 → SetModelRateLimit（只冷这个模型，账号继续服务其他模型）。
// modelName 为空时模型级冷却**放弃执行**而不是降级为账号级：降级会把上游明确
// 限定的模型故障扩大成整号故障，代价更大。调用方应尽量带上原始请求模型。
func (s *CodeBuddyAdminService) ApplyCodeBuddyBizCodeCooldown(
	ctx context.Context,
	account *Account,
	bizCode int,
	modelName string,
) bool {
	if s == nil || s.accountRepo == nil || account == nil || !account.IsCodeBuddy() {
		return false
	}
	decision := CodeBuddyBizCodeCooldown(bizCode)
	if !decision.Handled {
		return false
	}
	until := time.Now().Add(decision.Duration)

	if decision.Scope == codeBuddyCooldownScopeModel {
		model := strings.TrimSpace(modelName)
		if model == "" {
			slog.Warn("codebuddy_model_cooldown_skipped_no_model",
				"account_id", account.ID, "biz_code", bizCode)
			return false
		}
		if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, model, until, decision.Reason); err != nil {
			slog.Warn("codebuddy_model_cooldown_failed",
				"account_id", account.ID, "model", model, "error", err)
			return false
		}
		slog.Info("codebuddy_model_cooldown_applied",
			"account_id", account.ID, "model", model, "until", until)
		return true
	}

	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, decision.Reason); err != nil {
		slog.Warn("codebuddy_account_cooldown_failed",
			"account_id", account.ID, "biz_code", bizCode, "error", err)
		return false
	}
	slog.Info("codebuddy_account_cooldown_applied",
		"account_id", account.ID, "biz_code", bizCode, "until", until)
	return true
}

// ReenableCodeBuddyIfCreditsRecovered 余额恢复后的解冻判定。
//
// remain > 0 → 清除**本模块写入的**积分耗尽硬冷却（reason 前缀匹配）。
// 其余来源的停调一律不碰：那些停调有各自的恢复条件（凭据失效要重录、
// 风控要人工处理），被余额恢复顺手清掉会让它们静默失效。
//
// 返回是否真的清除了冷却（供调用方汇总/测试断言）。
func (s *CodeBuddyAdminService) ReenableCodeBuddyIfCreditsRecovered(
	ctx context.Context,
	account *Account,
	remain float64,
) bool {
	if s == nil || s.accountRepo == nil || account == nil || !account.IsCodeBuddy() {
		return false
	}
	if remain <= 0 {
		return false
	}
	if !account.HasCodeBuddyCreditsExhaustedCooldown() {
		return false
	}
	if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
		slog.Warn("codebuddy_unfreeze_failed", "account_id", account.ID, "error", err)
		return false
	}
	slog.Info("codebuddy_unfrozen_after_credits_recovered",
		"account_id", account.ID, "remain", remain)
	return true
}

// HasCodeBuddyCreditsExhaustedCooldown 账号是否正被"积分耗尽"冷却（本模块写入的）。
//
// 只看 reason 前缀 + 冷却未到期：不加"是否可调度"这类额外条件，
// 否则管理员手动置不可调度的账号会因为条件不满足而永远解不了冻。
func (a *Account) HasCodeBuddyCreditsExhaustedCooldown() bool {
	if a == nil || !a.IsCodeBuddy() {
		return false
	}
	if !strings.HasPrefix(strings.TrimSpace(a.TempUnschedulableReason), codeBuddyCreditsExhaustedReasonPrefix) {
		return false
	}
	if a.TempUnschedulableUntil == nil {
		return false
	}
	return time.Now().Before(*a.TempUnschedulableUntil)
}

// CodeBuddyCreditsRecoveredThreshold 判定"积分已恢复"的阈值。
// 与参考实现 ReenableIfCredits 同口径：> 0 即可。
const CodeBuddyCreditsRecoveredThreshold = 0
