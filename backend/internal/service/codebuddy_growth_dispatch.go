package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// errCodeBuddyGrowthRepoUnavailable 仓储未注入（构造期问题，非运行期故障）。
var errCodeBuddyGrowthRepoUnavailable = infraerrors.InternalServer(
	"CODEBUDDY_GROWTH_REPO_UNAVAILABLE", "codebuddy account repository not configured")

// 成长链的**通道分发**（A6 批 P5）：把"跑哪些通道"翻译成具体动作调用。
//
// ## 分级在这里的落点
//
// 本函数**只按传入的 channelKeys 执行**，不自己做分级判断——
// 过滤发生在调用方：自动排程用
// `codebuddy.CodeBuddyGrowthAutoSchedulableChannelKeys()`（已排除 full），
// 手动端点则显式点名要跑哪个通道。
//
// 但此处仍有一道**兜底防线**：逐个 key 校验它是否真的已注册
// （`CodeBuddyGrowthChannelSpecByKey`）——未注册的 key 直接跳过并告警。
// 这道防线防的是"调用方拼错通道名/传了未注册的键"，那种情况下静默不执行
// 比报错安全（不会误跑一个没声明过分级的动作）。

// CodeBuddyGrowthChannelRunResult 单通道的执行结果（供汇总与端点回执）。
type CodeBuddyGrowthChannelRunResult struct {
	// Key 通道键。
	Key string `json:"key"`
	// Tier 该通道的合规级别（回执里带上，便于运维确认跑的是什么级别）。
	Tier string `json:"tier"`
	// Detail 通道特定的结果（各通道结构不同，用 any 承载）。
	Detail any `json:"detail,omitempty"`
	// Error 该通道的顶层失败（空 = 没失败）。
	Error string `json:"error,omitempty"`
}

// CodeBuddyGrowthAccountSummary 一个账号跑完指定通道的汇总。
type CodeBuddyGrowthAccountSummary struct {
	AccountID int64                             `json:"account_id"`
	Results   []CodeBuddyGrowthChannelRunResult `json:"results,omitempty"`
	Error     string                            `json:"error,omitempty"`
}

// RunCodeBuddyGrowthChannels 依次执行指定通道。
//
// **单通道失败不中断后续通道**：它们是彼此独立的领奖动作，一个失败
// （比如连登端点抽风）不该让旅行也跑不了。
// 账号级的 Error 只在"没有可执行的通道"等全局情况下置位。
func (s *CodeBuddyAdminService) RunCodeBuddyGrowthChannels(
	ctx context.Context,
	account *Account,
	localDay string,
	channelKeys []string,
) CodeBuddyGrowthAccountSummary {
	summary := CodeBuddyGrowthAccountSummary{}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		summary.Error = "not a codebuddy account"
		return summary
	}
	summary.AccountID = account.ID

	executable := 0
	for _, key := range channelKeys {
		if err := ctx.Err(); err != nil {
			summary.Error = "cancelled"
			break
		}
		spec, ok := codebuddy.CodeBuddyGrowthChannelSpecByKey(key)
		if !ok {
			// 未注册的通道键：拼错了或忘了声明。跳过并告警，不静默执行。
			slog.Warn("codebuddy_growth.unknown_channel", "account_id", account.ID, "channel", key)
			continue
		}
		// full 级通道**不进这个函数**：它只能走手动单通道入口
		// （`RunCodeBuddyGrowthChannelNow`）。这里判在**执行之前**而不是放进 switch 的
		// default 分支，是为了让它同时参与下方"有没有可执行通道"的计数——
		// 否则传入 pure-full 的 keys 会被算成"有东西可跑"，
		// 从而把"其实什么都没跑"静默报成成功。
		if !spec.Tier.AutoSchedulable() {
			slog.Warn("codebuddy_growth.channel_not_auto_runnable",
				"account_id", account.ID, "channel", spec.Key, "tier", spec.Tier.String())
			continue
		}
		executable++

		result := CodeBuddyGrowthChannelRunResult{Key: spec.Key, Tier: spec.Tier.String()}
		switch spec.Key {
		case codebuddy.CodeBuddyGrowthChannelStreak:
			detail := s.runCodeBuddyGrowthStreak(ctx, account, localDay)
			result.Detail = detail
			if detail.Error != "" {
				result.Error = detail.Error
			}
		case codebuddy.CodeBuddyGrowthChannelTravelRun:
			detail := s.runCodeBuddyGrowthTravel(ctx, account, localDay)
			result.Detail = detail
			if detail.Error != "" {
				result.Error = detail.Error
			}
		case codebuddy.CodeBuddyGrowthChannelTravelStatus:
			// 只读：查一次状态，不写上游。
			state, err := s.fetchCodeBuddyTravelState(ctx, account)
			if err != nil {
				result.Error = "fetch travel state failed"
			} else {
				result.Detail = state
			}
		case codebuddy.CodeBuddyGrowthChannelTrial:
			detail := s.runCodeBuddyGrowthTrial(ctx, account, localDay)
			result.Detail = detail
			if detail.Error != "" {
				result.Error = detail.Error
			}
		default:
			// 未知的 claim/preview 级通道（注册了但这里没接分支）：
			// 属编程错误，告警并跳过。full 级已在循环开头被拦掉，到不了这里。
			slog.Warn("codebuddy_growth.channel_not_implemented",
				"account_id", account.ID, "channel", spec.Key, "tier", spec.Tier.String())
			continue
		}
		summary.Results = append(summary.Results, result)
	}

	if executable == 0 {
		summary.Error = "no runnable growth channel"
	}
	return summary
}

// codeBuddyGrowthManualChannels 手动通道（full 级）：只能由人显式点名执行。
//
// 从注册表按分级**推导**而不是硬编码列表：新增 full 级通道时自动纳入，
// 不会出现"代码里写死三个、注册表多了一个却没人能手动跑"。
func codeBuddyGrowthManualChannels() []codebuddy.CodeBuddyGrowthChannelSpec {
	manual := make([]codebuddy.CodeBuddyGrowthChannelSpec, 0)
	for _, spec := range codebuddy.CodeBuddyGrowthChannelSpecs {
		if !spec.Tier.AutoSchedulable() {
			manual = append(manual, spec)
		}
	}
	return manual
}

// codeBuddyGrowthChannelKeyIsManual 报告该通道是否属"仅手动"。
func codeBuddyGrowthChannelKeyIsManual(key string) bool {
	spec, ok := codebuddy.CodeBuddyGrowthChannelSpecByKey(key)
	return ok && !spec.Tier.AutoSchedulable()
}

// codeBuddyGrowthNormalizeChannelKeys 规范化手动端点传来的通道键列表：
// 去空白、去空项、保序去重。
func codeBuddyGrowthNormalizeChannelKeys(keys []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// codeBuddyGrowthTodayLocalDay 取当前 UTC+8 当地日期（手动端点用）。
func codeBuddyGrowthTodayLocalDay() string {
	return PlatformFeatureLocalDate(time.Now(), codeBuddyTimeZone).Format(time.DateOnly)
}

// ListCodeBuddyGrowthCandidates 列出成长链候选账号（含跳过标记）。
//
// 与签到/活跃上报候选同形（复用 `AccountRepository.ListByPlatform`，不新增仓储方法）。
// 跳过判据：已停调 / 临时停调冷却中 / 缺 access token / 缺 uid。
// 最后一条是 A5 的坑 1 教训：**缺 uid 的账号发上报必被静默丢弃**，
// 而成长链里夜猫子与领养都要发上报，所以在这里就标出来。
func (s *CodeBuddyAdminService) ListCodeBuddyGrowthCandidates(
	ctx context.Context,
	limit int,
) ([]codeBuddyGrowthCandidate, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errCodeBuddyGrowthRepoUnavailable
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformCodeBuddy)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > codeBuddyCheckinBatchMaxAccounts {
		limit = codeBuddyCheckinBatchMaxAccounts
	}

	now := time.Now()
	candidates := make([]codeBuddyGrowthCandidate, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !account.IsCodeBuddy() {
			continue
		}
		candidate := codeBuddyGrowthCandidate{AccountID: account.ID, Name: account.Name}
		switch {
		case !account.IsSchedulable():
			candidate.SkipReason = "账号已停调"
		case account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil):
			candidate.SkipReason = "账号处于临时停调冷却期"
		case strings.TrimSpace(account.GetCodeBuddyAccessToken()) == "":
			candidate.SkipReason = "账号缺少 access token"
		case strings.TrimSpace(account.GetCredential("uid")) == "":
			candidate.SkipReason = "账号缺少 uid（需上报的通道会被上游静默丢弃）"
		}
		candidates = append(candidates, candidate)
		if len(candidates) >= limit {
			break
		}
	}
	return candidates, nil
}
