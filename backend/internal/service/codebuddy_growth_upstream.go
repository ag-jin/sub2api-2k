package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/tidwall/gjson"
)

// 成长任务链的上游调用（批 6，A6）。
//
// 这一层只做三件事：端点解析（域 + 路径候选，见 codebuddy_growth_endpoints.go）、
// 出站请求、信封解析。**不做业务判断**——"要不要发"在通道层（codebuddy_growth_*.go），
// 免得把风控与业务语义混进 HTTP 层。

const (
	// codeBuddyGrowthRequestTimeout 单次成长链上游请求超时。
	codeBuddyGrowthRequestTimeout = 15 * time.Second

	// codeBuddyGrowthAccountDelay 账号之间的间隔。
	// 与签到/活跃上报同口径（参考实现 travelAccountDelay = 800ms）。
	codeBuddyGrowthAccountDelay = 800 * time.Millisecond

	// codeBuddyGrowthEventGap 同一账号内多条活跃上报之间的间隔。
	// 对齐 A5 的 codeBuddyActivityReportGap（1.5s，避免秒发风控）。
	codeBuddyGrowthEventGap = codeBuddyActivityReportGap
)

// codeBuddyGrowthUpstreamError 上游返回的错误（含业务码原值，供幂等/下线判定）。
//
// 为什么不用 infraerrors：上层需要**按业务码分类**（幂等码 14051 算成功、
// 41000 算活动下线静默跳过），而 infraerrors 把码压进文案里，上层再靠文本
// 反解析就回到了 L2 的坑（文本读取不可信）。这里保留结构化字段。
type codeBuddyGrowthUpstreamError struct {
	// Path 出站路径（诊断用，不含 token）。
	Path string
	// StatusCode 上游 HTTP 状态码（0 = 传输层失败）。
	StatusCode int
	// BizCode 业务码原值（可能是字符串形态如 "11-128"；见 L3）。
	BizCode any
	// NumericCode / HasNumericCode 业务码的数值形态（解不出时为 0/false）。
	NumericCode    int
	HasNumericCode bool
	// Message 上游 msg（已脱敏）。
	Message string
}

func (e *codeBuddyGrowthUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{"codebuddy growth upstream"}
	if e.Path != "" {
		parts = append(parts, e.Path)
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("http=%d", e.StatusCode))
	}
	// 业务码按**原值**展示：字符串形态的 "11-128" 用 %v 不会被吞成 0。
	parts = append(parts, fmt.Sprintf("code=%v", e.BizCode))
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, " ")
}

// isCodeBuddyGrowthTrialAlreadyClaimed 报告该错误是否为 trial 的"已领过"幂等态。
//
// 两条判据都覆盖（参考实现 `internal/upstream/trial.go:18-21` 给的两种指纹）：
//   - 数值码 == 14051（正常 JSON 信封路径）；
//   - 文案里含 "14051"（上游把幂等码塞进 msg / 原始 body 的路径）。
//
// 这里对**文案**做匹配是安全的：它匹配的是一个我们自己也认识的常量码，
// 而不是靠文案去推断语义类别（后者才是 L2 警告的用法）。
func isCodeBuddyGrowthTrialAlreadyClaimed(err error) bool {
	var upstreamErr *codeBuddyGrowthUpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if upstreamErr.HasNumericCode && upstreamErr.NumericCode == codebuddy.CodeBuddyTrialAlreadyClaimedBizCode {
		return true
	}
	return strings.Contains(upstreamErr.Message, codebuddy.CodeBuddyTrialAlreadyClaimedMarker)
}

// codeBuddyGrowthQuietReason 报告该错误是否应当**静默跳过**（不重试、不记账号故障）。
//
// 两类：
//  1. 活动下线（41000/40901）——F 通道 §6.5 的硬要求；
//  2. 需人工完成的环节（83400/50300）——重试一万次也不会变成做过。
//
// 返回 (原因, true) 表示应当静默。原因文案区分两类，便于运维分辨"活动没了"
// 与"要真人认证"——把后者说成前者会让人直接放弃排查。
func codeBuddyGrowthQuietReason(err error) (string, bool) {
	var upstreamErr *codeBuddyGrowthUpstreamError
	if !errors.As(err, &upstreamErr) || !upstreamErr.HasNumericCode {
		return "", false
	}
	if reason := codebuddy.CodeBuddyGrowthOfflineReason(upstreamErr.NumericCode); reason != "" {
		return reason, true
	}
	if reason := codebuddy.CodeBuddyGrowthManualOnlyReason(upstreamErr.NumericCode); reason != "" {
		return reason, true
	}
	return "", false
}

// codeBuddyGrowthCallResult 一次上游调用的结果（已解析信封 data 段）。
type codeBuddyGrowthCallResult struct {
	// Path 实际成功的路径（回落时可能不是首选）。
	Path string
	// Raw 完整响应体（调用方按需再取字段）。
	Raw []byte
	// Code 业务码原值；NumericCode/HasNumericCode 为其数值形态。
	Code           any
	NumericCode    int
	HasNumericCode bool
}

// callCodeBuddyGrowth 向成长链端点发一次请求。
//
// 逐层落实的既有教训：
//   - **业务码按原值取**（`codebuddyEnvelopeCodeRaw`），不用 gjson .Int()——
//     字符串形态 "11-128" 会被吞成 0，与真实成功码无法区分（L3）。
//   - **HTTP 4xx 也可能是业务信封**（上游惯用形态），所以先解信封；
//     只有"解不出信封且 HTTP ≥400"才当传输层错误。
//   - **404 触发路径回落**（仅白名单里多候选的端点；本批全是单候选，
//     所以这条实际不会走到，但保留是因为它对应 A2 已建立的契约）。
func (s *CodeBuddyAdminService) callCodeBuddyGrowth(
	ctx context.Context,
	account *Account,
	method string,
	path string,
	body any,
) (*codeBuddyGrowthCallResult, error) {
	candidates, ok := codeBuddyEndpointCandidates(path)
	if !ok {
		// 未声明的端点 = 编程错误。**不静默回落**成某个默认域发出去：
		// 打到错的主机在 CN 上可能"恰好能通"，直到接 global 账号才炸。
		return nil, fmt.Errorf("codebuddy growth: undeclared endpoint %q (add it to codeBuddyGrowthEndpoints)", path)
	}

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = encoded
	}

	var lastErr error
	for _, candidate := range candidates {
		result, err := s.callCodeBuddyGrowthOnce(ctx, account, method, candidate, payload)
		if err == nil {
			return result, nil
		}
		var upstreamErr *codeBuddyGrowthUpstreamError
		// 只有 404 是"路径不存在"的确定性信号 → 换下一个候选。
		if errors.As(err, &upstreamErr) && upstreamErr.StatusCode == http.StatusNotFound {
			lastErr = err
			continue
		}
		return nil, err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codebuddy growth: no endpoint candidate for %q", path)
	}
	return nil, lastErr
}

func (s *CodeBuddyAdminService) callCodeBuddyGrowthOnce(
	ctx context.Context,
	account *Account,
	method string,
	candidate codeBuddyEndpointCandidate,
	payload []byte,
) (*codeBuddyGrowthCallResult, error) {
	base := strings.TrimRight(codeBuddyGrowthDomainBase(candidate.Domain, account), "/")
	if base == "" {
		return nil, fmt.Errorf("codebuddy growth: no base for domain %d", candidate.Domain)
	}
	endpoint := base + candidate.Path

	var reader io.Reader
	if payload != nil {
		reader = strings.NewReader(string(payload))
	}
	reqCtx, cancel := context.WithTimeout(ctx, codeBuddyGrowthRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header.Set(k, v)
	}
	if token := strings.TrimSpace(account.GetCodeBuddyAccessToken()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// 小程序门户域需要 X-Client-Platform 头（参考实现 task_runner.py 任务表注释：
	// "小程序成长任务（growth 域小程序限定）：列表/accept/claim 均需
	// X-Client-Platform: miniprogram"）。只在 school 域加——给别的域加这个头会
	// 让请求看起来像小程序发出的，属无谓的指纹污染。
	if candidate.Domain == codeBuddyDomainSchoolMiniApp {
		req.Header.Set("X-Client-Platform", codebuddy.CodeBuddySchoolClientPlatform)
	}

	resp, doErr := s.client.Do(req)
	if doErr != nil {
		return nil, &codeBuddyGrowthUpstreamError{
			Path: candidate.Path, StatusCode: 0, Message: "request failed: " + doErr.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, &codeBuddyGrowthUpstreamError{
			Path: candidate.Path, StatusCode: resp.StatusCode, Message: "read response failed: " + readErr.Error(),
		}
	}

	result := &codeBuddyGrowthCallResult{Path: candidate.Path, Raw: raw}
	result.Code = codebuddyEnvelopeCodeRaw(raw)
	result.NumericCode, result.HasNumericCode = codebuddy.CodeBuddyNumericBizCode(raw)

	// 成功：业务码 0。
	if result.HasNumericCode && result.NumericCode == 0 {
		return result, nil
	}
	// 有 HTTP 错误但解不出业务码 → 传输层/网关错误。
	if !result.HasNumericCode && resp.StatusCode >= http.StatusBadRequest {
		return nil, &codeBuddyGrowthUpstreamError{
			Path: candidate.Path, StatusCode: resp.StatusCode,
			BizCode: result.Code, Message: codebuddyEnvelopeMsg(raw),
		}
	}
	// 其余（业务码非 0、或解不出码但 HTTP 2xx）一律按上游拒绝处理。
	return nil, &codeBuddyGrowthUpstreamError{
		Path:           candidate.Path,
		StatusCode:     resp.StatusCode,
		BizCode:        result.Code,
		NumericCode:    result.NumericCode,
		HasNumericCode: result.HasNumericCode,
		Message:        codebuddyEnvelopeMsg(raw),
	}
}

// codeBuddyGrowthData 从调用结果的 data 段解出目标结构（缺 data 时零值不报错）。
//
// 为什么缺 data 不当失败：上游对幂等态/空态经常回 `{"code":0}` 不带 data
// （例如 claim-gift 已领过、旅行 status 无在途记录）。把"没有 data"当错误
// 会让这些正常态全变成失败告警。
func codeBuddyGrowthData(result *codeBuddyGrowthCallResult, target any) error {
	if result == nil || len(result.Raw) == 0 || target == nil {
		return nil
	}
	data := gjson.GetBytes(result.Raw, "data")
	if !data.Exists() || data.Type == gjson.Null {
		return nil
	}
	return json.Unmarshal([]byte(data.Raw), target)
}
