package service

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// 签名状态读取面（票 28 / design M6「渠道签名 V4 配置 UI」第一条）。
//
// 只读投影，服务于管理端 29：全局签名开关与 fail 策略的生效值，以及每个「启用签名」
// 账号的握手私钥缓存状态。响应刻意不含任何凭据或签名材料——没有 apiKey、没有私钥、
// 没有 privateCipher 密文，账号只以 id + 名称标识。

// zhipuSignAccountLister 是状态投影需要的账号来源（*AccountRepository 即满足）。
// 收窄成单方法接缝，便于测试与后续替换实现，不影响既有仓库接口。
type zhipuSignAccountLister interface {
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
}

// zhipuSignCircuitBreakReader 是账号级熔断状态的只读来源（票 24 的 *ZhipuSignAlerts
// 即满足）。未接线（nil）时状态投影按 runtime_state_available=false 渲染为「未知」，
// 绝不让前端把「没接线」误读成「未熔断」。
type zhipuSignCircuitBreakReader interface {
	CircuitBreakState(accountID int64) ZhipuSignCircuitBreakState
}

// ZhipuSignAccountStatus 是单个账号的握手私钥状态。
type ZhipuSignAccountStatus struct {
	AccountID int64 `json:"account_id"`
	// AccountName 是账号显示名，不含任何凭据。
	AccountName string `json:"account_name"`
	// KeyCached 表示本进程当前是否持有可用（未过期、未作废）的握手私钥。
	KeyCached bool `json:"key_cached"`
	// LastHandshakeAt 是本进程上次成功握手的时刻；从未握手为 null（前端显示「未缓存」）。
	LastHandshakeAt *time.Time `json:"last_handshake_at"`
	// KeyExpiresAt 是缓存私钥的失效时刻；没有缓存私钥为 null。
	KeyExpiresAt *time.Time `json:"key_expires_at"`
	// ConsecutiveFailures 是自上次成功握手以来的连续握手失败次数。
	ConsecutiveFailures int `json:"consecutive_failures"`

	// CircuitBreakTripped / CircuitBreakReason 是账号级熔断运行时状态（票 24）：
	// 5 分钟窗口内验签失效超阈值时，签名生效位被内存摘除（credentials 未被修改）。
	// CircuitBreakReason 是稳定原因码（如 verify_invalid_over_threshold），不含自然语言。
	// 该状态不进 DB：进程重启即恢复，窗口滚动后自动恢复，管理员确认版本后可显式恢复。
	CircuitBreakTripped bool   `json:"circuit_break_tripped"`
	CircuitBreakReason  string `json:"circuit_break_reason"`
	// RuntimeStateAvailable 表示上面两个字段是否来自真实的运行时状态源：
	// false（未接线）时前端必须渲染为「未知」，不得显示「未熔断」。
	RuntimeStateAvailable bool `json:"runtime_state_available"`
}

// ZhipuSignStatus 是状态读取接口的响应体。
type ZhipuSignStatus struct {
	// SignV4Enabled / SignFailPolicy 是当前生效值（运行层覆盖 > 部署层）。
	SignV4Enabled  bool   `json:"sign_v4_enabled"`
	SignFailPolicy string `json:"sign_fail_policy"`
	// Accounts 只包含「启用签名」的智谱账号（登录托管 + 账号级 zcode_client_sign=v4），
	// 按 account_id 升序，未握手账号显示 KeyCached=false / LastHandshakeAt=null。
	Accounts []ZhipuSignAccountStatus `json:"accounts"`
}

// Status 汇总状态读取面。accountRepo 为 nil（未接线/测试）时返回空账号列表；
// 账号查询失败时返回错误而不是静默空列表——「状态未知」与「没有启用账号」必须可区分。
func (s *ZhipuSignConfigService) Status(ctx context.Context) (*ZhipuSignStatus, error) {
	if s == nil {
		return &ZhipuSignStatus{Accounts: []ZhipuSignAccountStatus{}}, nil
	}
	effective := s.Effective(ctx)
	status := &ZhipuSignStatus{
		SignV4Enabled:  effective.SignV4Enabled,
		SignFailPolicy: effective.SignFailPolicy,
		Accounts:       []ZhipuSignAccountStatus{},
	}
	if s.accountRepo == nil {
		return status, nil
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformZhipu)
	if err != nil {
		return nil, fmt.Errorf("list zhipu accounts for sign status: %w", err)
	}
	for i := range accounts {
		account := &accounts[i]
		if !zhipuSignAccountEnabled(account) {
			continue
		}
		status.Accounts = append(status.Accounts, s.accountSignStatus(account))
	}
	sort.Slice(status.Accounts, func(i, j int) bool {
		return status.Accounts[i].AccountID < status.Accounts[j].AccountID
	})
	return status, nil
}

// accountSignStatus 读取一个账号的私钥缓存状态与熔断运行时状态（对应 runtime 未接线
// 时分别退化为「未缓存」与「未知」）。
func (s *ZhipuSignConfigService) accountSignStatus(account *Account) ZhipuSignAccountStatus {
	entry := ZhipuSignAccountStatus{
		AccountID:   account.ID,
		AccountName: account.Name,
	}
	// 熔断状态：只有接入了票 24 的运行时状态源才声称「已知」。
	if s.circuitBreak != nil {
		state := s.circuitBreak.CircuitBreakState(account.ID)
		entry.CircuitBreakTripped = state.Tripped
		entry.CircuitBreakReason = state.Reason
		entry.RuntimeStateAvailable = true
	}
	if s.runtime == nil {
		return entry
	}
	keyStatus := s.runtime.KeyStatus(zhipuSignAPIKeyID(account.GetOpenAIProtocolAPIKey()))
	entry.KeyCached = keyStatus.Cached
	entry.ConsecutiveFailures = keyStatus.ConsecutiveFailures
	if !keyStatus.LastHandshakeAt.IsZero() {
		lastHandshake := keyStatus.LastHandshakeAt
		entry.LastHandshakeAt = &lastHandshake
	}
	if !keyStatus.ExpiresAt.IsZero() {
		expiresAt := keyStatus.ExpiresAt
		entry.KeyExpiresAt = &expiresAt
	}
	return entry
}

// zhipuSignAccountEnabled 判定账号是否「启用签名」：智谱登录托管账号 ∧ 账号级灰度标记
// （签名的双开关 AND 里账号那一半；全局开关单独上报，方便运营对照）。
func zhipuSignAccountEnabled(account *Account) bool {
	return zhipuSignLoginManagedAccount(account) &&
		account.GetCredential(zhipuSignCredentialKey) == zhipuSignCredentialV4
}
