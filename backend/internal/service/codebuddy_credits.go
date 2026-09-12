package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// 腾讯 CodeBuddy 计费（积分余额 / 签到）请求形态常量（A6）。
// 独立常量段：计费恒用 TENCENT_BILLING_BASE（不跟账号 base_url，base_url 仅 chat）；
// TENCENT_BILLING_HEADERS 为计费/管理接口的固定 UA 头全集（不含 Bearer，
// 也不注入任何身份头——X-User-Id 等身份头集仅用于 chat/refresh 通道）。
const (
	// TENCENT_BILLING_BASE CodeBuddy 计费域名（www.codebuddy.cn）。
	TENCENT_BILLING_BASE = "https://www.codebuddy.cn"
	// codeBuddyBillingGetUserResourcePath 实时积分查询端点。
	codeBuddyBillingGetUserResourcePath = "/v2/billing/meter/get-user-resource"
	// CodeBuddyDailyCheckinPath 每日签到端点。
	CodeBuddyDailyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// TENCENT_CODEBUDDY_USER_AGENT 计费/管理接口固定 UA（实证 absorb-verify.md V1）。
const TENCENT_CODEBUDDY_USER_AGENT = "CLI/2.63.2 CodeBuddy/2.63.2"

// TENCENT_BILLING_HEADERS 计费/管理接口固定请求头全集（照抄参考实现
// config.py TENCENT_HEADERS，见 absorb-verify.md V1；Bearer 由调用方追加）。
var TENCENT_BILLING_HEADERS = map[string]string{
	"Content-Type":     "application/json",
	"Accept":           "application/json, text/plain, */*",
	"X-Requested-With": "XMLHttpRequest",
	"User-Agent":       TENCENT_CODEBUDDY_USER_AGENT,
	"Origin":           "https://www.codebuddy.cn",
	"Referer":          "https://www.codebuddy.cn/",
}

// CodeBuddyBillingRequestBody 计费查询请求体固定形态（absorb-verify.md V1
// 实证）：时间窗 = now … now+365*101 天，ProductCode=p_tcaca，Status=[0,3]。
func codeBuddyBillingRequestBody(now time.Time) map[string]any {
	return map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
}

// CodeBuddyCreditsFetchOptions 只携带请求期取值；凭据不与任何 provider 包共享。
type CodeBuddyCreditsFetchOptions struct {
	AccessToken string
	ProxyURL    string
	AccountID   int64
	// BaseURL 计费 base 覆写。仅测试注入（httptest）；生产恒空 → TENCENT_BILLING_BASE。
	BaseURL string
}

// CodeBuddyCreditsFetcher 查询 CodeBuddy 账号实时积分余额（A2）。
// -------------------- 使用 UpstreamBalanceUsage 冻结契约 --------------------
// 返回复用既有 UpstreamBalanceUsage struct（json 键以该 struct 现状为准，此处
// 冻结：balance/remaining/unit/mode/plan_name/today/total/status/stale/error），
// codebuddy 余额填 balance + unit=credits + status=ok；降级走
// status=error/stale 与 lastSuccess 值通道，不改账号任何状态。
type CodeBuddyCreditsFetcher struct {
	httpUpstream HTTPUpstream
	// testBaseURL 仅供测试注入 httptest 端点；生产恒为空。
	testBaseURL string
}

func NewCodeBuddyCreditsFetcher(httpUpstream HTTPUpstream) *CodeBuddyCreditsFetcher {
	return &CodeBuddyCreditsFetcher{httpUpstream: httpUpstream}
}

// WithTestBaseURL 注入测试计费 base（httptest）。链式返回自身。
func (f *CodeBuddyCreditsFetcher) WithTestBaseURL(baseURL string) *CodeBuddyCreditsFetcher {
	f.testBaseURL = strings.TrimRight(baseURL, "/")
	return f
}

// FetchCredits 调 POST {billing base}/v2/billing/meter/get-user-resource 并
// 解析 data.Response.Data.Accounts[]：每套餐取 CycleCapacityRemainPrecise
// （字符串精确小数）求和；Precise 缺失时退化整数字段（CycleCapacityRemain，
// Cycle 全零退 CapacityRemain）。失败以 *CodeBuddyCreditsError 返回。
func (f *CodeBuddyCreditsFetcher) FetchCredits(ctx context.Context, opts *CodeBuddyCreditsFetchOptions) (*UpstreamBalanceUsage, error) {
	if opts == nil || strings.TrimSpace(opts.AccessToken) == "" {
		return nil, &CodeBuddyCreditsError{Code: "unauthenticated"}
	}
	baseURL := TENCENT_BILLING_BASE
	if strings.TrimSpace(opts.BaseURL) != "" {
		baseURL = strings.TrimRight(opts.BaseURL, "/")
	} else if f != nil && f.testBaseURL != "" {
		baseURL = f.testBaseURL
	}
	endpoint := baseURL + codeBuddyBillingGetUserResourcePath

	body, err := json.Marshal(codeBuddyBillingRequestBody(time.Now()))
	if err != nil {
		return nil, &CodeBuddyCreditsError{Code: "internal_error"}
	}
	reqCtx, cancel := context.WithTimeout(ctx, codeBuddyBillingRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &CodeBuddyCreditsError{Code: "internal_error"}
	}
	for k, v := range TENCENT_BILLING_HEADERS {
		req.Header.Set(k, v)
	}
	// 仅注入 Bearer；不注入 X-User-Id 等身份头（计费接口按实证不含身份头）。
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(opts.AccessToken))

	// httppool + 账号级代理 + 15s 超时（出站统一池）。
	upstream := f.httpUpstream
	if upstream == nil {
		client, buildErr := httppool.GetClient(httppool.Options{
			ProxyURL: opts.ProxyURL,
			Timeout:  codeBuddyBillingRequestTimeout,
		})
		if buildErr != nil {
			return nil, &CodeBuddyCreditsError{Code: "internal_error"}
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			return nil, &CodeBuddyCreditsError{Code: "network_error"}
		}
		return f.readCreditsResponse(resp)
	}
	resp, doErr := upstream.Do(req, opts.ProxyURL, opts.AccountID, 1)
	if doErr != nil {
		return nil, &CodeBuddyCreditsError{Code: "network_error"}
	}
	return f.readCreditsResponse(resp)
}

func (f *CodeBuddyCreditsFetcher) readCreditsResponse(resp *http.Response) (*UpstreamBalanceUsage, error) {
	if resp == nil || resp.Body == nil {
		return nil, &CodeBuddyCreditsError{Code: "network_error"}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &CodeBuddyCreditsError{Code: "network_error", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &CodeBuddyCreditsError{Code: "unauthenticated", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &CodeBuddyCreditsError{Code: "upstream_error", HTTPStatus: resp.StatusCode}
	}
	return parseCodeBuddyCreditsResponse(raw)
}

// parseCodeBuddyCreditsResponse 解析信封 {code,msg,data} 与
// data.Response.Data.Accounts[] 的 Precise/整数字段，余额求和。
func parseCodeBuddyCreditsResponse(raw []byte) (*UpstreamBalanceUsage, error) {
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
	}
	if envelope.Code != 0 {
		return nil, &CodeBuddyCreditsError{Code: "upstream_error", BizCode: envelope.Code, BizMsg: envelope.Msg}
	}
	accounts, err := codeBuddyExtractResourceAccounts(envelope.Data)
	if err != nil {
		return nil, err
	}
	total := 0.0
	for _, item := range accounts {
		total += codeBuddyPackageRemain(item)
	}
	if total < 0 {
		total = 0
	}
	usage := &UpstreamBalanceUsage{Unit: "credits", Status: "ok"}
	usage.Balance = &total
	return usage, nil
}

// codeBuddyExtractResourceAccounts 下钻 data → data.Response → data.Response.Data
// 取 Accounts[]，结构无法识别返回 invalid_response。
func codeBuddyExtractResourceAccounts(data json.RawMessage) ([]map[string]any, error) {
	cur := json.RawMessage(data)
	for depth := 0; depth < 5; depth++ {
		var probe map[string]any
		if err := json.Unmarshal(cur, &probe); err != nil || probe == nil {
			return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
		}
		if accountsList, ok := probe["Accounts"].([]any); ok {
			return codeBuddyAccountsAsMaps(accountsList), nil
		}
		next, ok := probe["Response"]
		if !ok {
			return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
		}
		if nextMap, ok := next.(map[string]any); ok {
			if inner, isMap := nextMap["Data"].(map[string]any); isMap {
				raw, err := json.Marshal(inner)
				if err != nil {
					return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
				}
				cur = json.RawMessage(raw)
				continue
			}
			raw, err := json.Marshal(nextMap)
			if err != nil {
				return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
			}
			cur = json.RawMessage(raw)
			continue
		}
		return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
	}
	return nil, &CodeBuddyCreditsError{Code: "invalid_response"}
}

func codeBuddyAccountsAsMaps(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// codeBuddyPackageRemain 单套餐余额：优先 CycleCapacityRemainPrecise（字符串
// 精确小数）；Precise 缺失/无法解析时退整数字段 CycleCapacityRemain；Cycle 全零
// （字段全缺）退化 CapacityRemain（照抄参考实现 _package_remain 退化口径）。
func codeBuddyPackageRemain(item map[string]any) float64 {
	if v, ok := item["CycleCapacityRemainPrecise"]; ok && v != nil {
		switch value := v.(type) {
		case string:
			if floatValue, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				return floatValue
			}
		case int:
			return float64(value)
		case float64:
			return value
		}
	}
	cycleSize := codeBuddyFloatAny(item["CycleCapacitySize"])
	cycleRemain := codeBuddyFloatAny(item["CycleCapacityRemain"])
	cycleUsed := codeBuddyFloatAny(item["CycleCapacityUsed"])
	if cycleSize > 0 || cycleRemain > 0 || cycleUsed > 0 {
		return cycleRemain
	}
	return codeBuddyFloatAny(item["CapacityRemain"])
}

// codeBuddyFloatAny 整数字段退化解析（int/float/string/array 均容忍，非数值→0）。
func codeBuddyFloatAny(v any) float64 {
	switch value := v.(type) {
	case int:
		return float64(value)
	case float64:
		return value
	case json.Number:
		floatValue, err := value.Float64()
		if err != nil {
			return 0
		}
		return floatValue
	case string:
		floatValue, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0
		}
		return floatValue
	default:
		return 0
	}
}

// CodeBuddyCreditsError 是积分查询失败的安全分类（凭据安全、无响应体回显）。
type CodeBuddyCreditsError struct {
	Code       string // unauthenticated / forbidden / upstream_error / network_error / invalid_response / internal_error
	HTTPStatus int
	BizCode    int
	BizMsg     string
}

func (e *CodeBuddyCreditsError) Error() string {
	base := "codebuddy credits error"
	if e == nil || e.Code == "" {
		return base
	}
	msg := fmt.Sprintf("%s: %s", base, e.Code)
	if e.BizCode != 0 {
		msg += fmt.Sprintf(" (biz code %d %s)", e.BizCode, e.BizMsg)
	}
	return msg
}

// codeBuddyBillingRequestTimeout 出站请求超时（15s，A2）。
const codeBuddyBillingRequestTimeout = 15 * time.Second
