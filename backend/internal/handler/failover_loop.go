package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// TempUnscheduler 用于 HandleFailoverError 中同账号重试耗尽后的临时封禁。
// GatewayService 隐式实现此接口。
type TempUnscheduler interface {
	TempUnscheduleRetryableError(ctx context.Context, accountID int64, failoverErr *service.UpstreamFailoverError)
}

// FailoverAction 表示 failover 错误处理后的下一步动作
type FailoverAction int

const (
	// FailoverContinue 继续循环（同账号重试或切换账号，调用方统一 continue）
	FailoverContinue FailoverAction = iota
	// FailoverExhausted 切换次数耗尽（调用方应返回错误响应）
	FailoverExhausted
	// FailoverCanceled context 已取消（调用方应直接 return）
	FailoverCanceled
)

const (
	// maxSameAccountRetries 同账号重试次数上限（针对 RetryableOnSameAccount 错误）
	maxSameAccountRetries = 3
	// postFirstTokenSameAccountRetries 首 token 后文本续写只做更短的同账号恢复。
	postFirstTokenSameAccountRetries = 2
	// sameAccountRetryDelay 同账号重试间隔
	sameAccountRetryDelay = 500 * time.Millisecond
	// singleAccountBackoffDelay 单账号分组 503 退避重试固定延时。
	// Service 层在 SingleAccountRetry 模式下已做充分原地重试（最多 3 次、总等待 30s），
	// Handler 层只需短暂间隔后重新进入 Service 层即可。
	singleAccountBackoffDelay    = 2 * time.Second
	preFirstTokenRecoveryBudget  = 2 * time.Second
	postFirstTokenRecoveryBudget = 10 * time.Second
)

// FailoverState 跨循环迭代共享的 failover 状态
type FailoverState struct {
	SwitchCount                     int
	MaxSwitches                     int
	FailedAccountIDs                map[int64]struct{}
	SameAccountRetryCount           map[int64]int
	LastFailoverErr                 *service.UpstreamFailoverError
	ForceCacheBilling               bool
	hasBoundSession                 bool
	preFirstTokenRecoveryStartedAt  time.Time
	postFirstTokenRecoveryStartedAt time.Time
	continuationMetrics             ContinuationMetrics
}

type ContinuationMetrics struct {
	PreFirstTokenRecoverTotal             int64
	PostFirstTokenSameAccountRecoverTotal int64
	CrossAccountContinuationAttemptTotal  int64
	CrossAccountContinuationSuccessTotal  int64
	CrossAccountContinuationFailTotal     int64
	StrictSafeConservativeFailTotal       int64
	PreFirstTokenDurationMs               int64
	ContinuationDurationMs                int64
	LastEvent                             string
	LastAccountID                         int64
	LastStatusCode                        int
}

// NewFailoverState 创建 failover 状态
func NewFailoverState(maxSwitches int, hasBoundSession bool) *FailoverState {
	return &FailoverState{
		MaxSwitches:           maxSwitches,
		FailedAccountIDs:      make(map[int64]struct{}),
		SameAccountRetryCount: make(map[int64]int),
		hasBoundSession:       hasBoundSession,
	}
}

func (s *FailoverState) PreFirstTokenRecoveryBudget() time.Duration {
	return preFirstTokenRecoveryBudget
}

func (s *FailoverState) CanRecoverBeforeFirstToken() bool {
	if s == nil {
		return false
	}
	if s.preFirstTokenRecoveryStartedAt.IsZero() {
		s.preFirstTokenRecoveryStartedAt = time.Now()
		s.continuationMetrics.PreFirstTokenRecoverTotal++
		s.continuationMetrics.LastEvent = "pre_first_token_recover_started"
		return true
	}
	ok := time.Since(s.preFirstTokenRecoveryStartedAt) <= preFirstTokenRecoveryBudget
	if ok {
		s.continuationMetrics.PreFirstTokenRecoverTotal++
		s.continuationMetrics.LastEvent = "pre_first_token_recover"
	}
	return ok
}

func (s *FailoverState) SleepBeforeFirstTokenRetry(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return s.CanRecoverBeforeFirstToken()
	}
	if !s.CanRecoverBeforeFirstToken() {
		return false
	}
	deadline := s.preFirstTokenRecoveryStartedAt.Add(preFirstTokenRecoveryBudget)
	remaining := time.Until(deadline)
	if remaining <= 0 || d > remaining {
		return false
	}
	return sleepWithContext(ctx, d)
}

func (s *FailoverState) PostFirstTokenRecoveryBudget() time.Duration {
	return postFirstTokenRecoveryBudget
}

func (s *FailoverState) PostFirstTokenSameAccountRetryLimit() int {
	return postFirstTokenSameAccountRetries
}

func (s *FailoverState) CanRecoverAfterFirstToken() bool {
	if s == nil {
		return false
	}
	if s.postFirstTokenRecoveryStartedAt.IsZero() {
		s.postFirstTokenRecoveryStartedAt = time.Now()
		return true
	}
	return time.Since(s.postFirstTokenRecoveryStartedAt) <= postFirstTokenRecoveryBudget
}

func (s *FailoverState) SleepAfterFirstTokenRetry(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return s.CanRecoverAfterFirstToken()
	}
	if !s.CanRecoverAfterFirstToken() {
		return false
	}
	deadline := s.postFirstTokenRecoveryStartedAt.Add(postFirstTokenRecoveryBudget)
	remaining := time.Until(deadline)
	if remaining <= 0 || d > remaining {
		return false
	}
	return sleepWithContext(ctx, d)
}

// HandlePostFirstTokenFailover handles stream failures after client-visible
// output has started. Only text_aggressive may cross accounts; strict_safe
// terminates to avoid corrupting tools/JSON/image contracts.
func (s *FailoverState) HandlePostFirstTokenFailover(
	ctx context.Context,
	mode service.OpenAIStreamContinuationMode,
	accountID int64,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	if s == nil {
		return FailoverExhausted
	}
	s.LastFailoverErr = failoverErr
	s.continuationMetrics.LastAccountID = accountID
	if failoverErr != nil {
		s.continuationMetrics.LastStatusCode = failoverErr.StatusCode
	}
	if mode != service.OpenAIStreamContinuationTextAggressive {
		s.continuationMetrics.StrictSafeConservativeFailTotal++
		s.continuationMetrics.LastEvent = "strict_safe_conservative_fail"
		logger.FromContext(ctx).Warn("gateway.post_token_continuation_strict_safe_fail",
			zap.Int64("account_id", accountID),
			zap.String("stream_continuation_mode", string(mode)),
			zap.Int("upstream_status", s.continuationMetrics.LastStatusCode),
		)
		return FailoverExhausted
	}
	if !s.CanRecoverAfterFirstToken() {
		s.continuationMetrics.CrossAccountContinuationFailTotal++
		s.continuationMetrics.LastEvent = "post_first_token_budget_exhausted"
		return FailoverExhausted
	}
	if failoverErr != nil && failoverErr.RetryableOnSameAccount && s.SameAccountRetryCount[accountID] < postFirstTokenSameAccountRetries {
		s.SameAccountRetryCount[accountID]++
		s.continuationMetrics.PostFirstTokenSameAccountRecoverTotal++
		s.continuationMetrics.LastEvent = "post_first_token_same_account_recover"
		logger.FromContext(ctx).Warn("gateway.post_token_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", postFirstTokenSameAccountRetries),
			zap.Int64("continuation_duration_ms", s.ContinuationMetricsSnapshot().ContinuationDurationMs),
		)
		if !s.SleepAfterFirstTokenRetry(ctx, sameAccountRetryDelay) {
			s.continuationMetrics.CrossAccountContinuationFailTotal++
			s.continuationMetrics.LastEvent = "post_first_token_same_account_sleep_canceled"
			return FailoverExhausted
		}
		return FailoverContinue
	}

	s.FailedAccountIDs[accountID] = struct{}{}
	if s.SwitchCount >= s.MaxSwitches {
		s.continuationMetrics.CrossAccountContinuationFailTotal++
		s.continuationMetrics.LastEvent = "cross_account_continuation_switch_exhausted"
		return FailoverExhausted
	}
	s.SwitchCount++
	s.continuationMetrics.CrossAccountContinuationAttemptTotal++
	s.continuationMetrics.LastEvent = "cross_account_continuation_attempt"
	logger.FromContext(ctx).Warn("gateway.post_token_continuation_switch_account",
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status", func() int {
			if failoverErr == nil {
				return 0
			}
			return failoverErr.StatusCode
		}()),
		zap.Int("switch_count", s.SwitchCount),
		zap.Int("max_switches", s.MaxSwitches),
		zap.Int64("continuation_duration_ms", s.ContinuationMetricsSnapshot().ContinuationDurationMs),
	)
	return FailoverContinue
}

func (s *FailoverState) RecordCrossAccountContinuationResult(success bool) {
	if s == nil {
		return
	}
	if success {
		s.continuationMetrics.CrossAccountContinuationSuccessTotal++
		s.continuationMetrics.LastEvent = "cross_account_continuation_success"
		return
	}
	s.continuationMetrics.CrossAccountContinuationFailTotal++
	s.continuationMetrics.LastEvent = "cross_account_continuation_fail"
}

func (s *FailoverState) ContinuationMetricsSnapshot() ContinuationMetrics {
	if s == nil {
		return ContinuationMetrics{}
	}
	snapshot := s.continuationMetrics
	if !s.preFirstTokenRecoveryStartedAt.IsZero() {
		snapshot.PreFirstTokenDurationMs = time.Since(s.preFirstTokenRecoveryStartedAt).Milliseconds()
		if snapshot.PreFirstTokenDurationMs < 0 {
			snapshot.PreFirstTokenDurationMs = 0
		}
	}
	if !s.postFirstTokenRecoveryStartedAt.IsZero() {
		snapshot.ContinuationDurationMs = time.Since(s.postFirstTokenRecoveryStartedAt).Milliseconds()
		if snapshot.ContinuationDurationMs < 0 {
			snapshot.ContinuationDurationMs = 0
		}
	}
	return snapshot
}

// HandleFailoverError 处理 UpstreamFailoverError，返回下一步动作。
// 包含：缓存计费判断、同账号重试、临时封禁、切换计数、Antigravity 延时。
func (s *FailoverState) HandleFailoverError(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	s.LastFailoverErr = failoverErr

	// 缓存计费判断
	if needForceCacheBilling(s.hasBoundSession, failoverErr) {
		s.ForceCacheBilling = true
	}

	// 同账号重试：对 RetryableOnSameAccount 的临时性错误，先在同一账号上重试
	if failoverErr.RetryableOnSameAccount && s.SameAccountRetryCount[accountID] < maxSameAccountRetries {
		s.SameAccountRetryCount[accountID]++
		logger.FromContext(ctx).Warn("gateway.failover_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", maxSameAccountRetries),
		)
		if !sleepWithContext(ctx, sameAccountRetryDelay) {
			return FailoverCanceled
		}
		return FailoverContinue
	}

	// 同账号重试用尽，执行临时封禁
	if failoverErr.RetryableOnSameAccount {
		gatewayService.TempUnscheduleRetryableError(ctx, accountID, failoverErr)
	}

	// 加入失败列表
	s.FailedAccountIDs[accountID] = struct{}{}

	// 检查是否耗尽
	if s.SwitchCount >= s.MaxSwitches {
		return FailoverExhausted
	}

	// 递增切换计数
	s.SwitchCount++
	logger.FromContext(ctx).Warn("gateway.failover_switch_account",
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("switch_count", s.SwitchCount),
		zap.Int("max_switches", s.MaxSwitches),
	)

	// Antigravity 平台换号线性递增延时
	if platform == service.PlatformAntigravity {
		delay := time.Duration(s.SwitchCount-1) * time.Second
		if !sleepWithContext(ctx, delay) {
			return FailoverCanceled
		}
	}

	return FailoverContinue
}

// HandleSelectionExhausted 处理选号失败（所有候选账号都在排除列表中）时的退避重试决策。
// 针对 Antigravity 单账号分组的 503 (MODEL_CAPACITY_EXHAUSTED) 场景：
// 清除排除列表、等待退避后重新选号。
//
// 返回 FailoverContinue 时，调用方应设置 SingleAccountRetry context 并 continue。
// 返回 FailoverExhausted 时，调用方应返回错误响应。
// 返回 FailoverCanceled 时，调用方应直接 return。
func (s *FailoverState) HandleSelectionExhausted(ctx context.Context) FailoverAction {
	if s.LastFailoverErr != nil &&
		s.LastFailoverErr.StatusCode == http.StatusServiceUnavailable &&
		s.SwitchCount <= s.MaxSwitches {

		logger.FromContext(ctx).Warn("gateway.failover_single_account_backoff",
			zap.Duration("backoff_delay", singleAccountBackoffDelay),
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		if !sleepWithContext(ctx, singleAccountBackoffDelay) {
			return FailoverCanceled
		}
		logger.FromContext(ctx).Warn("gateway.failover_single_account_retry",
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		s.FailedAccountIDs = make(map[int64]struct{})
		return FailoverContinue
	}
	return FailoverExhausted
}

// needForceCacheBilling 判断 failover 时是否需要强制缓存计费。
// 粘性会话切换账号、或上游明确标记时，将 input_tokens 转为 cache_read 计费。
func needForceCacheBilling(hasBoundSession bool, failoverErr *service.UpstreamFailoverError) bool {
	return hasBoundSession || (failoverErr != nil && failoverErr.ForceCacheBilling)
}

// sleepWithContext 等待指定时长，返回 false 表示 context 已取消。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
