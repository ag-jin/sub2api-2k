package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	logredact "github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
)

// CodeBuddy 对话活跃上报（A5 批 5）。
//
// 通道：`POST {billingBase}/v2/report`（**billing 域**，不是 chat 域）。
// 一条上报同时点亮 growth 连登 + 解锁 `first_buddy` 任务（领养前置）。
//
// 三个实测踩出来的约束（参考实现 workbuddy2api `internal/upstream/report.go` 原文）：
//  1. **事件必须带 userId**（= 账号 uid），缺失时上游 **200 但静默丢弃**——
//     看起来成功、日志全绿、实际一次都没报。所以上报后**必须回读 streak 自检**。
//  2. 照抄官方 `chat_request_send` 的**完整字段形状**（见 platform/codebuddy
//     activity_report.go），勿用最小字段集，防上游后续加严。
//  3. 刷 `chat_5` 前置的正确姿势是**同一 conversationId 内 N 条**（N=5），
//     requestId 各条独立，条间留间隔避免秒发风控。
//
// 风控口径：**每号每天 1 次**即可，不做多时点高频上报。

const (
	// codeBuddyActivityAccountDelay 账号之间的间隔。
	// 参考实现是 800ms；这里同口径（上游对同 IP 高频敏感）。
	codeBuddyActivityAccountDelay = 800 * time.Millisecond

	// codeBuddyActivityReportGap 同一账号内 5 条之间的间隔（1.5s）。
	// 参考实现 activityReportGap = 1500ms：避免秒发触发风控。
	codeBuddyActivityReportGap = 1500 * time.Millisecond

	// codeBuddyActivityRequestTimeout 单条上报请求超时。
	codeBuddyActivityRequestTimeout = 15 * time.Second

	// codeBuddyActivityStreakTimeout 自检回读超时。
	codeBuddyActivityStreakTimeout = 10 * time.Second
)

// 上报前置校验的哨兵错误。三种"根本不该发"的情形分开命名：调度器要按它们
// 区分 skipped 与 failed（凭据缺失是跳过，不是失败）。
var (
	errCodeBuddyActivityInvalidAccount = errors.New("codebuddy activity: account is not a codebuddy account")
	errCodeBuddyActivityMissingUID     = errors.New("codebuddy activity: account uid missing (upstream would silently drop the event)")
	errCodeBuddyActivityMissingToken   = errors.New("codebuddy activity: account has no access token")
)

// gjsonGetInt 从上游响应里取整数字段（缺失返回 0）。
//
// ⚠️ 只在**响应**解析上用 gjson .Int()：这里的 0 是"没有值"的自然表示。
// **不要**把这个口径带到业务码解析上——那边字符串形态的码会被吞成 0，
// 与真实的 code=0（成功）不可区分（L3）。
func gjsonGetInt(raw []byte, path string) int64 {
	return gjson.GetBytes(raw, path).Int()
}

// CodeBuddyActivityStreakPath 连登状态回读端点（别名，字面量在 platform 包）。
//
// ⚠️ 注意域不同：本条走 **chat 域**（`{chatBase}/activity/growth/streak`），
// 而上报本身走 **billing 域**。参考实现里 `GrowthStreak` 用 chatBase + BillingHeaders，
// `ReportChatActivity` 用 billingBase——两者不是同一个 base，别混用。
//
// 保留别名（而不是让本包各处改用 `codebuddy.` 前缀）是为了不动既有调用点与测试；
// 字面量只有一份，在 platform 包——A6 成长链的 `CodeBuddyGrowthStreakPath`
// 必须与它共用同一个值（platform 包不能反向 import service，定义只能在那边）。
const CodeBuddyActivityStreakPath = codebuddy.CodeBuddyActivityStreakPath

// CodeBuddyActivityReportResult 单账号上报结果。
type CodeBuddyActivityReportResult struct {
	AccountID int64
	// Reported 成功发出的条数（失败即停，故可能 < 期望条数）。
	Reported int
	// Expected 期望条数（N）。
	Expected int
	// CreditIssued / StreakDays 自检回读结果（未自检时为零值）。
	StreakDays int
	// Verified 是否完成自检；SelfCheckFailed 表示上报成功但自检异常/失败。
	Verified        bool
	SelfCheckFailed bool
	Err             error
}

// ReportCodeBuddyActivity 对单个账号上报 N 条对话活跃。
//
// 语义（逐条对齐参考实现）：
//   - N 条**共用同一 conversationId**（同会话），requestId 各条独立；
//   - 条间 sleep `codeBuddyActivityReportGap`；
//   - **任一条失败即停止该号后续条数**（不再续发，自检无意义）；
//   - N 条发满才做 streak 自检（不满则自检无意义）。
func (s *CodeBuddyAdminService) ReportCodeBuddyActivity(
	ctx context.Context,
	account *Account,
	count int,
) CodeBuddyActivityReportResult {
	result := CodeBuddyActivityReportResult{Expected: count}
	if s == nil || account == nil || !account.IsCodeBuddy() {
		result.Err = errCodeBuddyActivityInvalidAccount
		return result
	}
	result.AccountID = account.ID
	if count <= 0 {
		count = codebuddy.CodeBuddyActivityReportCount
		result.Expected = count
	}

	userID := strings.TrimSpace(account.GetCredential("uid"))
	if userID == "" {
		// uid 缺失 = 事件必然被上游静默丢弃。这里直接判失败，
		// 不去发一个"看起来成功"的请求（坑 1 的直接对策）。
		result.Err = errCodeBuddyActivityMissingUID
		return result
	}
	accessToken := strings.TrimSpace(account.GetCodeBuddyAccessToken())
	if accessToken == "" {
		result.Err = errCodeBuddyActivityMissingToken
		return result
	}

	now := time.Now()
	conversationID := codebuddy.CodeBuddyActivityConversationID(now)

	for i := 1; i <= count; i++ {
		if err := ctx.Err(); err != nil {
			result.Err = err
			return result
		}
		requestID := codebuddy.CodeBuddyActivityRequestID(conversationID, i)
		event := codebuddy.NewCodeBuddyActivityEvent(userID, conversationID, requestID, time.Now())
		if !codebuddy.ValidateCodeBuddyActivityEvent(event) {
			result.Err = errCodeBuddyActivityMissingUID
			return result
		}
		if err := s.sendCodeBuddyActivity(ctx, account, event, accessToken); err != nil {
			result.Err = err
			return result // 该号停止，不再续发
		}
		result.Reported++
		if i < count {
			if err := codeBuddyActivitySleep(ctx, codeBuddyActivityReportGap); err != nil {
				result.Err = err
				return result
			}
		}
	}

	// N 条发满 → 回读 streak 自检（坑 1：200 ≠ 真的点亮）。
	days, verified, failed := s.checkCodeBuddyActivityStreak(ctx, account, accessToken)
	result.StreakDays = days
	result.Verified = verified
	result.SelfCheckFailed = failed
	return result
}

// sendCodeBuddyActivity 发一条上报（body 是**数组**，参考实现即 `[]chatRequestEvent`）。
// codeBuddyActivityReportEndpoint 上报端点：**billing 域** + 固定路径 /v2/report。
//
// ⚠️ 单一路径，无 realm 回落：/v2/report **不在** billing/meter 回落族
// （参考实现 client.go:880 原文"report /v2/report 不参与"）。别照 L8 加候选。
func (s *CodeBuddyAdminService) codeBuddyActivityReportEndpoint(account *Account) string {
	return activityEndpointBase(s, account, CodeBuddyBillingBase) + codebuddy.CodeBuddyActivityReportPath
}

// codeBuddyActivityStreakEndpoint 连登回读端点：**chat 域** + /activity/growth/streak。
//
// ⚠️ 与上报**不同域**：上报走 billing（www.codebuddy.cn），连登走 chat
// （copilot.tencent.com）。参考实现里两者分别是 billingBase 与 chatBase——
// 混用会打到错的主机，且两端都用同一个 testBaseURL 时测试**看不出来**。
func (s *CodeBuddyAdminService) codeBuddyActivityStreakEndpoint(account *Account) string {
	return activityEndpointBase(s, account, CodeBuddyChatBase) + CodeBuddyActivityStreakPath
}

// activityEndpointBase 取端点 base：测试注入优先，否则按 realm 走传入的解析器。
// 抽出来是为了让"哪个域"这件事可单测——两个 endpoint 函数的**差异只在解析器参数**，
// 混淆两者是本模块最容易犯且最难发现的错（testBaseURL 会同时覆盖两边）。
func activityEndpointBase(
	s *CodeBuddyAdminService,
	account *Account,
	resolve func(*Account) string,
) string {
	if s != nil && s.testBaseURL != "" {
		return strings.TrimRight(s.testBaseURL, "/")
	}
	return strings.TrimRight(resolve(account), "/")
}

func (s *CodeBuddyAdminService) sendCodeBuddyActivity(
	ctx context.Context,
	account *Account,
	event codebuddy.CodeBuddyActivityEvent,
	accessToken string,
) error {
	body, err := json.Marshal([]codebuddy.CodeBuddyActivityEvent{event})
	if err != nil {
		return err
	}
	endpoint := s.codeBuddyActivityReportEndpoint(account)

	reqCtx, cancel := context.WithTimeout(ctx, codeBuddyActivityRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header[k] = []string{v}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, doErr := s.client.Do(req)
	if doErr != nil {
		return doErr
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
	}
	// 上游也可能用 HTTP 4xx 承载业务信封（与既有口径一致）：先看信封 code。
	//
	// ⚠️ 用 platform 包的 CodeBuddyNumericBizCode（严格解析 + ok 标志），
	// **不要**用 codebuddyEnvelopeCode：后者是 `gjson .Int()`，对字符串形态的码
	// 返回 0，于是"上游拒绝"被读成"成功"（L3 的原始坑）。
	// 解不出数字时按失败处理——本通道的失败可能完全静默（HTTP 200 也可能被丢弃），
	// 假阳性成功的代价远高于假阴性。
	if code, ok := codebuddy.CodeBuddyNumericBizCode(raw); !ok || code != 0 {
		return codebuddyUpstreamBizError(raw)
	}
	return nil
}

// checkCodeBuddyActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
//
// 返回 (days, verified, failed)：
//   - verified=true 表示自检成功执行（不代表天数好看）；
//   - failed=true 表示「上报 OK 但 streak 可疑」——回读失败，或 days==0
//     （后者正是"缺 userId 被静默丢弃"的典型迹象）。
//
// 回读失败**不算上报失败**：上报本身已成功且按天幂等，不做重试；
// 但必须留可诊断日志，不能静默当成功。
func (s *CodeBuddyAdminService) checkCodeBuddyActivityStreak(
	ctx context.Context,
	account *Account,
	accessToken string,
) (days int, verified bool, failed bool) {
	endpoint := s.codeBuddyActivityStreakEndpoint(account)

	reqCtx, cancel := context.WithTimeout(ctx, codeBuddyActivityStreakTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, false, true
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header[k] = []string{v}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, doErr := s.client.Do(req)
	if doErr != nil {
		slog.Warn("codebuddy activity streak check failed (report OK)",
			slog.Int64("account_id", account.ID),
			slog.String("error", logredact.RedactText(doErr.Error())))
		return 0, false, true
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		slog.Warn("codebuddy activity streak read failed (report OK)",
			slog.Int64("account_id", account.ID),
			slog.String("error", logredact.RedactText(readErr.Error())))
		return 0, false, true
	}
	if code, ok := codebuddy.CodeBuddyNumericBizCode(raw); !ok || code != 0 {
		slog.Warn("codebuddy activity streak rejected (report OK)",
			slog.Int64("account_id", account.ID),
			slog.Any("biz_code", codebuddyEnvelopeCodeRaw(raw)))
		return 0, false, true
	}
	days = int(gjsonGetInt(raw, "data.streak.days"))
	if days == 0 {
		// 上报 200 但 days=0 → 极可能是静默丢弃（缺 userId 的典型症状）。
		slog.Warn("codebuddy activity report OK but streak.days=0 (silent drop?)",
			slog.Int64("account_id", account.ID))
		return 0, true, true
	}
	slog.Info("codebuddy activity streak verified",
		slog.Int64("account_id", account.ID), slog.Int("days", days))
	return days, true, false
}

// codeBuddyActivitySleep 可取消的等待；ctx 取消立即返回错误。
// 抽成包级 var 便于测试替换（否则单账号 5 条要真等 6 秒）。
var codeBuddyActivitySleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
