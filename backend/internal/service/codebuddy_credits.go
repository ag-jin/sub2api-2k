package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// 腾讯 CodeBuddy 计费（积分余额 / 签到）请求形态常量（A6）。
// 独立常量段：计费 base 恒按账号 realm 取（不跟账号 base_url，base_url 仅 chat）——
// 见 codebuddy_realm_endpoints.go；TENCENT_BILLING_HEADERS 为计费/管理接口的固定
// UA 头全集（不含 Bearer，也不注入任何身份头——X-User-Id 等身份头集仅用于
// chat/refresh 通道）。

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

// codeBuddyBillingTimeLayout 计费接口的时间串格式（墙钟，无时区后缀）。
//
// ⚠️ 请求体与响应体共用同一 layout，但**承载的字段名不同，勿混**：
//   - 请求体发的是 `PackageEndTimeRangeBegin/End`（查询时间窗过滤条件）；
//   - 响应里带的是 `CycleEndTime`（每个套餐真实的到期时刻，按 UTC+8 解释）。
//
// 见 REF-C `client.go:1509-1510,1563` 与 REF-B `tencent.py` 的 _PACKAGE_END_LAYOUT。
const codeBuddyBillingTimeLayout = "2006-01-02 15:04:05"

// codeBuddyBillingLoc 上游时间串的时区：UTC+8 墙钟（与官网展示时区一致）。
// 必须显式固定，**不能按本机时区解析**——否则 UTC/西半球容器上算出的到期时刻
// 会整体偏移数小时，倒计时跟着错（REF-B `tencent.py:_package_expiry` 原话）。
var codeBuddyBillingLoc = time.FixedZone("UTC+8", 8*60*60)

// CodeBuddyBillingRequestBody 计费查询请求体固定形态（absorb-verify.md V1
// 实证）：时间窗 = now … now+365*101 天，ProductCode=p_tcaca，Status=[0,3]。
func codeBuddyBillingRequestBody(now time.Time) map[string]any {
	return map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(codeBuddyBillingTimeLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(codeBuddyBillingTimeLayout),
	}
}

// CodeBuddyCreditsFetchOptions 只携带请求期取值；凭据不与任何 provider 包共享。
type CodeBuddyCreditsFetchOptions struct {
	AccessToken string
	ProxyURL    string
	AccountID   int64
	// Account 账号主体：**只用于 realm 判定**（域名与路径族分发），凭据仍走
	// AccessToken 字段，本字段不被读取为凭据来源。nil 按 CN 处理。
	Account *Account
	// BaseURL 计费 base 覆写。仅测试注入（httptest）；生产恒空 → 按账号 realm 取
	// CodeBuddyBillingBase 的 CN/global 常量。
	BaseURL string
	// Paths 计费端点路径候选覆写（按尝试顺序，404 换下一候选）。仅测试注入；
	// 生产恒空 → 按账号 realm 取 CodeBuddyUserResourcePaths。
	Paths []string
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

// FetchCredits 调 POST {billing base}{path}/get-user-resource 并解析
// data.Response.Data.Accounts[]：每套餐取 CycleCapacityRemainPrecise
// （字符串精确小数）**逐个钳位后**求和；Precise 缺失时退化整数字段
// （CycleCapacityRemain，Cycle 全零退 CapacityRemain）。失败以
// *CodeBuddyCreditsError 返回。
//
// base 与路径候选均**按账号 realm 分发**（P0-3）：CN → www.codebuddy.cn +
// /v2/billing/meter/get-user-resource；global → www.workbuddy.ai +
// /billing/meter/get-user-resource，404 回落 /v2/billing/meter/get-user-resource
// （两域顺序相反，见 codebuddy_realm_endpoints.go）。
// ⚠️ global 分支**未经真机验证**（本轮无 global 测试账号）。
func (f *CodeBuddyCreditsFetcher) FetchCredits(ctx context.Context, opts *CodeBuddyCreditsFetchOptions) (*UpstreamBalanceUsage, error) {
	if opts == nil || strings.TrimSpace(opts.AccessToken) == "" {
		return nil, &CodeBuddyCreditsError{Code: "unauthenticated"}
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	switch {
	case baseURL != "":
		// 测试注入优先。
	case f != nil && f.testBaseURL != "":
		baseURL = f.testBaseURL
	default:
		baseURL = CodeBuddyBillingBase(opts.Account)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	paths := opts.Paths
	if len(paths) == 0 {
		paths = CodeBuddyUserResourcePaths(opts.Account)
	}

	body, err := json.Marshal(codeBuddyBillingRequestBody(time.Now()))
	if err != nil {
		return nil, &CodeBuddyCreditsError{Code: "internal_error"}
	}
	// 超时预算覆盖**全部候选路径**（不是每条各一份）——回落不该成倍拉长最坏耗时。
	reqCtx, cancel := context.WithTimeout(ctx, codeBuddyBillingRequestTimeout)
	defer cancel()

	var lastErr error
	for i, path := range paths {
		usage, attemptErr := f.fetchCreditsAt(reqCtx, baseURL+path, body, opts)
		if attemptErr == nil {
			return usage, nil
		}
		lastErr = attemptErr
		// 仅 404 换下一候选（上游"该域没有无 /v2 形式"的确切信号）；其他错误立即返回。
		if i < len(paths)-1 && codeBuddyCreditsPathNotFound(attemptErr) {
			continue
		}
		return nil, attemptErr
	}
	return nil, lastErr
}

// fetchCreditsAt 单次计费请求（一条候选路径）并解析响应。
func (f *CodeBuddyCreditsFetcher) fetchCreditsAt(reqCtx context.Context, endpoint string, body []byte, opts *CodeBuddyCreditsFetchOptions) (*UpstreamBalanceUsage, error) {
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &CodeBuddyCreditsError{Code: "internal_error"}
	}
	// ⚠️ 已知缺口：计费头仍是 CN 形态（Origin/Referer = www.codebuddy.cn），
	// global 账号应发 workbuddy.ai 的品牌域（REF-C `BillingHeaders` 有按 realm 注入
	// Accept-Language，Origin/Referer 由 realm 派生）。该补全属另一任务范围，
	// 本轮不改，避免与本任务写集冲突。
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

// codeBuddyCreditsPathNotFound 报告错误是否为上游 404（路径不存在的回落信号）。
// 只看 HTTP 状态，不改错误分类（404 的 Code 仍是 upstream_error，对调用方口径不变）。
func codeBuddyCreditsPathNotFound(err error) bool {
	creditsErr, ok := err.(*CodeBuddyCreditsError)
	return ok && creditsErr != nil && creditsErr.HTTPStatus == http.StatusNotFound
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
// data.Response.Data.Accounts[] 的 Precise/整数字段：余额**逐套餐钳位后**求和，
// 同时收集仍有余额的套餐到期时刻（CycleEndTime）。
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
	expiries := make([]UpstreamBalanceExpiry, 0, len(accounts))
	for _, item := range accounts {
		// 逐条钳位**再**累加（不是求和后整体钳）：一个 -100 的坏套餐若先参与求和，
		// 会把合计里的其他正数吃掉，而我们只应把它当 0。顺序不能换（REF-B
		// `tencent.py` 原文："顺序不能换"）。
		remain := codeBuddyPackageRemain(item)
		if remain < 0 {
			remain = 0
		}
		total += remain
		// 到期时间只在**仍有余额**的套餐上有意义（已花光的套餐不参与倒计时，
		// 否则界面会显示一堆已作废的时间点）。
		if remain <= 0 {
			continue
		}
		if at, ok := codeBuddyPackageExpiry(item); ok {
			expiries = append(expiries, UpstreamBalanceExpiry{At: at, Amount: remain})
		}
	}
	if total < 0 {
		total = 0
	}
	usage := &UpstreamBalanceUsage{Unit: "credits", Status: "ok"}
	usage.Balance = &total
	if len(expiries) > 0 {
		sort.Slice(expiries, func(i, j int) bool { return expiries[i].At.Before(expiries[j].At) })
		usage.Expiries = expiries
	}
	return usage, nil
}

// codeBuddyPackageExpiry 单套餐到期时刻。字段缺失/空/解析失败 → false（到期时间是
// 展示字段，不能因为它让整次余额查询失败；上游对解析失败也是保守忽略）。
//
// ⚠️ 字段名是响应里的 `CycleEndTime`，**不是**请求体里的 `PackageEndTimeRange*`
// ——两者是不同字段（见 codeBuddyBillingTimeLayout 注释）。
// 时间串按 UTC+8 硬编码解释，与本机时区无关。
func codeBuddyPackageExpiry(item map[string]any) (time.Time, bool) {
	text := strings.TrimSpace(codeBuddyStr(item["CycleEndTime"]))
	if text == "" {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation(codeBuddyBillingTimeLayout, text, codeBuddyBillingLoc)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
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
// （字段全缺）退化 CapacityRemain。
//
// **钳位是本函数的职责**（此前只做退化、不做钳位，是"显示给用户的余额比上游多"
// 的静默错误数字缺陷）。口径参照 REF-B `tencent.py:_package_remain`（v1.0.52
// 专门重写）与 REF-C `packageRemainUsed`（两仓同一事实来源，注释明说"上游为此把
// 双份逻辑合并到了这一个函数"）：
//
//   - CycleCapacitySize > 0 时：remain = clamp(remain, 0, size)，再用
//     used = size - remain 与 CycleCapacityUsed 反修正；
//   - 无周期额度时退 CapacityRemain。
//
// 为什么必须钳上界不只是钳负值：腾讯偶发 `CycleCapacityRemain > CycleCapacitySize`
// 的脏数据，只钳负值会**高估**余额（用户看到比实际多的数字，且不报错、不明显）。
// 注意 Precise 字段是字符串精确小数，**本函数只钳不退化**——即 Precise 在场时
// 也必须参与钳位，否则精确小数路径仍然会漏掉脏数据。
func codeBuddyPackageRemain(item map[string]any) float64 {
	size := codeBuddyFloatAny(item["CycleCapacitySize"])
	remain, hasCycleRemain := codeBuddyPackagePreciseRemain(item)
	if !hasCycleRemain {
		remain = codeBuddyFloatAny(item["CycleCapacityRemain"])
	}
	if size <= 0 && remain <= 0 && codeBuddyFloatAny(item["CycleCapacityUsed"]) <= 0 {
		// Cycle 三元组全缺 → 退化 Capacity 域（照抄参考实现退化口径）。
		return codeBuddyFloatAny(item["CapacityRemain"])
	}
	if size <= 0 {
		// 有 Cycle 余额但没有总量：无从钳上界（上游同款语义），保持原值。
		return remain
	}
	if remain < 0 {
		remain = 0
	}
	if remain > size {
		remain = size
	}
	// used 反修正：上游把 used 记成"已消耗"，若它比 size-remain 更大，说明
	// remain 被高估，用 used 反推一个更小的 remain（仍然只往下修）。
	if used := codeBuddyFloatAny(item["CycleCapacityUsed"]); used > size-remain && size >= used {
		remain = size - used
	}
	return remain
}

// codeBuddyPackagePreciseRemain 取 CycleCapacityRemainPrecise（字符串精确小数形态）
// 的数值；字段缺失/无法解析时 hasPrecise=false，由调用方退化整数字段。
func codeBuddyPackagePreciseRemain(item map[string]any) (value float64, hasPrecise bool) {
	v, ok := item["CycleCapacityRemainPrecise"]
	if !ok || v == nil {
		return 0, false
	}
	switch typed := v.(type) {
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case int:
		return float64(typed), true
	case float64:
		return typed, true
	default:
		return 0, false
	}
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
