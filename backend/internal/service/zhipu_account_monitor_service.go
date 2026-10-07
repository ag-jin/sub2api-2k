package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bigmodel"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// 智谱登录态账号的管理面探针服务（design M4）。
//
// R0：仅观测，永不使用。本服务对重置卡只做只读读取（reset/status → 快照字段），
// 不封装使用端点、不暴露写操作；任何「使用重置卡」的改动都是对 R0 的推翻，
// 需先经用户显式决策（校验：scripts/check_r0_invariant.sh，票 13）。
//
// 独立于 CNProviderQuotaService：credit-usage 走 bigmodel access_token
// （Authorization 直传，无 Bearer），与 api_key 探针的鉴权组合不同，
// 因此自成一个数据源，不做 cn_quota 分支。
//
// 已知上游特性（research FINAL-REPORT §六）：数据有 4–11 分钟结算延时，
// 调用方不得假设实时性；响应 code 为 200（不是 bigmodel 其它端点的 0）。
const (
	// zhipuCreditUsageDetailURL 是逐模型积分明细端点（协议资产 C / design M4）。
	zhipuCreditUsageDetailURL = "https://open.bigmodel.cn/api/monitor/credit-usage/usage-detail"

	zhipuCreditUsageTimeout = 15 * time.Second
	zhipuCreditUsageMaxBody = 512 * 1024
	zhipuCreditUsageCodeOK  = 200
	// zhipuCreditUsageMessageMaxBytes 是上游错误摘要进入快照/日志前的截断长度。
	zhipuCreditUsageMessageMaxBytes = 240

	// zhipuDefaultCreditUsageCacheMinutes 是 credit-usage 快照的内存缓存 TTL 兜底值，
	// 与重置卡状态查询共用 gateway.zhipu.reset_status_cache_minutes（票 08 规定口径）。
	zhipuDefaultCreditUsageCacheMinutes = 10
)

// zhipuCreditUsageWindow 把调用方给的时刻归一成上游的当日窗口：
// start → 该时刻所在 +8 自然日的 00:00:00，end → 同日的 23:59:59。
// 时区取 zhipuChinaZone（zhipu_cost_model.go 的单一事实源）。
func zhipuCreditUsageWindow(start, end time.Time) (string, string) {
	return start.In(zhipuChinaZone).Format("2006-01-02") + " 00:00:00",
		end.In(zhipuChinaZone).Format("2006-01-02") + " 23:59:59"
}

// ErrZhipuCreditUsageNotConfigured 表示服务缺少必要依赖（未接线）。
var ErrZhipuCreditUsageNotConfigured = errors.New("zhipu credit usage service is not configured")

// ErrZhipuCreditUsageNoCredential 表示账号不是登录托管智谱账号或缺少 access_token；
// 此时不发起任何上游请求。
var ErrZhipuCreditUsageNoCredential = errors.New("zhipu credit usage: account has no login access token")

// ErrZhipuCreditUsageUnauthorized 表示上游 401/403：登录态凭据失效或无权访问。
// keeper（票 09）用 errors.Is 区分这一分支并标记 needs_relogin。
var ErrZhipuCreditUsageUnauthorized = errors.New("zhipu credit usage: upstream rejected credentials")

// ErrZhipuCreditUsageUpstream 表示传输层失败或 HTTP 非 2xx（可重试的瞬时故障）。
var ErrZhipuCreditUsageUpstream = errors.New("zhipu credit usage: upstream request failed")

// ErrZhipuCreditUsageResponse 表示 HTTP 2xx 但响应体不符合协议
// （非 JSON、code != 200、success=false、缺 modelDataList/xTime）。
var ErrZhipuCreditUsageResponse = errors.New("zhipu credit usage: unexpected response")

// ZhipuAccountMonitorService 采集登录托管智谱账号的积分明细与登录态凭据健康。
//
// 本文件包含 credit-usage 探针（票 08）、重置卡只读读取（票 12）与 L2 费率对账的
// 周期挂载 / 快照合并（票 27）。
//
// 缓存语义：以「账号 + 上游当日窗口」为键的单条正缓存（TTL 见 creditUsageCacheTTL），
// 并发与 TTL 内的连续调用由 singleflight 合并为至多一次上游请求；失败结果不写缓存。
type ZhipuAccountMonitorService struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config

	// resetStatus 是重置卡只读读取器（design M4 / 票 12）：装配期在构造函数里
	// 经共享 RiskClient 建好，运行期只读；无 HTTP 上游（未接线/回滚）时为 nil，
	// 调用方静默跳过整条链。
	resetStatus zhipuResetStatusReader

	flight singleflight.Group

	creditUsageMu    sync.Mutex
	creditUsageCache map[string]zhipuCreditUsageCacheEntry

	// signReconcile 是 L2 费率对账器（票 27）：装配期经 SetSignReconciler 注入一次，
	// 运行期只读；为 nil 时周期入口与快照合并都是空操作（零行为变化）。
	signReconcile *ZhipuSignReconciler

	now func() time.Time
}

// zhipuResetStatusReader 是重置卡只读状态读取器的接缝（*bigmodel.ResetStatusReader
// 天然满足；单元测试注入桩）。
//
// R0：本接口只有只读 Fetch —— 重置卡是用户资产，本系统仅观测，永不使用；
// 不存在任何使用/兑换/消耗方法的接缝，也不得新增（校验：scripts/check_r0_invariant.sh）。
type zhipuResetStatusReader interface {
	Fetch(ctx context.Context, zcodeJWTToken, accessToken string) (*bigmodel.ResetStatus, error)
}

// zhipuCreditUsageCacheEntry 是键内已含账号与窗口的单条快照缓存。
type zhipuCreditUsageCacheEntry struct {
	credits   []domain.MonitorQuotaModelCredit
	expiresAt time.Time
}

// NewZhipuAccountMonitorService 构造探针服务。accountRepo 供后续管理面查询复用。
//
// 重置卡只读读取器在同一构造点装配（票 12）：TTL 与 credit-usage 快照共用
// gateway.zhipu.reset_status_cache_minutes（票 08 规定的口径，默认 10 分钟）；
// httpUpstream 缺失时读取器保持 nil（整链静默跳过，等价回滚）。
func NewZhipuAccountMonitorService(accountRepo AccountRepository, httpUpstream HTTPUpstream, cfg *config.Config) *ZhipuAccountMonitorService {
	svc := &ZhipuAccountMonitorService{
		accountRepo:      accountRepo,
		httpUpstream:     httpUpstream,
		cfg:              cfg,
		creditUsageCache: make(map[string]zhipuCreditUsageCacheEntry),
		now:              time.Now,
	}
	svc.resetStatus = newZhipuResetStatusReader(httpUpstream, cfg, svc.creditUsageCacheTTL())
	return svc
}

// newZhipuResetStatusReader 装配重置卡只读读取器（design M4 / 票 12）：zcode.z.ai 是
// 风控域名，读取必须经共享 RiskClient（最小调用间隔 + 3012/3001/429 负缓存退避 +
// singleflight），绝不直连；httpUpstream 缺失时返回 nil，调用方静默跳过。
//
// 全服务共用**一个**读取器（而非按账号各建）：RiskClient 的节流/退避按端点全局生效，
// 多账号并发读同一端点时才算「一个上游 IP 一个闸」；读取器内部按凭据哈希缓存 +
// 串行化，账号之间不会串号（见 reset_status.go 的 callMu）。
func newZhipuResetStatusReader(httpUpstream HTTPUpstream, cfg *config.Config, cacheTTL time.Duration) zhipuResetStatusReader {
	if httpUpstream == nil {
		return nil
	}
	return bigmodel.NewResetStatusReader(
		bigmodel.NewRiskClient(zhipuResetStatusHTTPDoer{upstream: httpUpstream}, zhipuZCodeMinCallInterval(cfg)),
		cacheTTL,
	)
}

// zhipuResetStatusHTTPDoer 把 HTTPUpstream 适配成 bigmodel 需要的单参数 doer。
// 不绑定账号（accountID=0，与登录链路 ZhipuOAuthService.doerFor 同形）：该层只做
// 连接池隔离，zcode.z.ai 的调用闸由上面的共享 RiskClient 承担。
type zhipuResetStatusHTTPDoer struct {
	upstream HTTPUpstream
}

func (d zhipuResetStatusHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	return d.upstream.Do(req, "", 0, 0)
}

// zhipuZCodeMinCallInterval 取 zcode.z.ai 风控端点的全局最小调用间隔
// （gateway.zhipu.zcode_min_call_interval_seconds，0 = 禁用节流）。未注入配置时按
// design M1 的默认值兜底（单一事实源：bigmodel 的同名导出常量）；登录链路读同一个键。
func zhipuZCodeMinCallInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return bigmodel.DefaultZCodeMinCallInterval
	}
	seconds := cfg.Gateway.Zhipu.ZCodeMinCallIntervalSeconds
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// FetchUsageDetailForAccount 拉取 [start, end] 窗口内的逐模型逐日积分明细。
//
// 返回的切片按上游 modelDataList 顺序展开（每个模型 × 每个自然日一行）。
// start/end 先归一到 +8 自然日（见 zhipuCreditUsageWindow），因此调用方
// 传 time.Now() 即得「当日窗口」。同一账号同一窗口在 TTL 内的并发与连续
// 调用只打一次上游（keeper 09 与监控 11 共用）。
//
// 鉴权失败（401/403）返回 ErrZhipuCreditUsageUnauthorized，调用方（keeper）
// 据此判定登录态失效；其余失败返回可包装错误，绝不返回半截数据。
func (s *ZhipuAccountMonitorService) FetchUsageDetailForAccount(ctx context.Context, account *Account, start, end time.Time) ([]domain.MonitorQuotaModelCredit, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, ErrZhipuCreditUsageNotConfigured
	}
	accessToken := strings.TrimSpace(account.GetZhipuAccessToken())
	if accessToken == "" {
		return nil, ErrZhipuCreditUsageNoCredential
	}

	startTime, endTime := zhipuCreditUsageWindow(start, end)
	key := zhipuCreditUsageCacheKey(account.ID, startTime, endTime)
	if credits, ok := s.loadCachedCreditUsage(key); ok {
		return credits, nil
	}

	resultCh := s.flight.DoChan(key, func() (any, error) {
		// 进入 flight 后复查缓存：TTL 内已有结果时不再探测（singleflight 之外
		// 的唯一写者是本函数，因此不会读到半成品）。
		if credits, ok := s.loadCachedCreditUsage(key); ok {
			return credits, nil
		}
		// 上游请求不绑定单个调用方 ctx：并发调用合并为一次探测；调用方取消
		// 由外层 select 处理（与 cn_provider_quota_service 的骨架同口径）。
		probeCtx, cancel := context.WithTimeout(context.Background(), zhipuCreditUsageTimeout+5*time.Second)
		defer cancel()
		credits, err := s.fetchUsageDetail(probeCtx, account, accessToken, startTime, endTime)
		if err != nil {
			// 失败不写缓存：下一次调用必须重新探测（避免把 401 缓存成长期结论）。
			return nil, err
		}
		s.storeCreditUsage(key, credits)
		return credits, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return nil, result.Err
		}
		credits, ok := result.Val.([]domain.MonitorQuotaModelCredit)
		if !ok {
			return nil, fmt.Errorf("%w: unexpected singleflight payload", ErrZhipuCreditUsageResponse)
		}
		return zhipuCloneCreditRows(credits), nil
	}
}

// fetchUsageDetail 执行单次上游探测（探测上下文与窗口字符串都由调用方定好）。
func (s *ZhipuAccountMonitorService) fetchUsageDetail(ctx context.Context, account *Account, accessToken, startTime, endTime string) ([]domain.MonitorQuotaModelCredit, error) {
	query := url.Values{}
	query.Set("type", "1")
	query.Set("usageType", "MODEL")
	query.Set("startTime", startTime)
	query.Set("endTime", endTime)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zhipuCreditUsageDetailURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("zhipu credit usage: build request: %w", err)
	}
	req.Header.Set("Authorization", accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpUpstream.Do(req, "", account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		// 保留原始错误链（含 context.DeadlineExceeded 等）供上层区分取消与瞬时故障。
		return nil, fmt.Errorf("%w: %w", ErrZhipuCreditUsageUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, zhipuCreditUsageMaxBody))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 登录态凭据失效：keeper 据此标记 needs_relogin，不落任何半截数据。
		return nil, fmt.Errorf("%w: HTTP %d", ErrZhipuCreditUsageUnauthorized, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrZhipuCreditUsageUpstream, resp.StatusCode,
			truncate(strings.TrimSpace(string(body)), zhipuCreditUsageMessageMaxBytes))
	}

	root := gjson.ParseBytes(body)
	if success := root.Get("success"); success.Exists() && !success.Bool() {
		return nil, fmt.Errorf("%w: success=false: %s", ErrZhipuCreditUsageResponse, zhipuCreditUsageMessage(root))
	}
	return parseZhipuCreditUsageDetail(body)
}

// creditUsageCacheTTL 取配置的缓存 TTL，未配置或缺省时回落 10 分钟。
func (s *ZhipuAccountMonitorService) creditUsageCacheTTL() time.Duration {
	minutes := zhipuDefaultCreditUsageCacheMinutes
	if s != nil && s.cfg != nil && s.cfg.Gateway.Zhipu.ResetStatusCacheMinutes > 0 {
		minutes = s.cfg.Gateway.Zhipu.ResetStatusCacheMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// loadCachedCreditUsage 读取未过期的快照；命中时返回副本，调用方改动不影响缓存。
func (s *ZhipuAccountMonitorService) loadCachedCreditUsage(key string) ([]domain.MonitorQuotaModelCredit, bool) {
	now := s.nowTime()
	s.creditUsageMu.Lock()
	defer s.creditUsageMu.Unlock()
	entry, ok := s.creditUsageCache[key]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.expiresAt) {
		delete(s.creditUsageCache, key)
		return nil, false
	}
	return zhipuCloneCreditRows(entry.credits), true
}

// storeCreditUsage 写入正缓存并顺手清理过期条目（键的数量受账号数约束）。
func (s *ZhipuAccountMonitorService) storeCreditUsage(key string, credits []domain.MonitorQuotaModelCredit) {
	now := s.nowTime()
	expiresAt := now.Add(s.creditUsageCacheTTL())
	s.creditUsageMu.Lock()
	defer s.creditUsageMu.Unlock()
	if s.creditUsageCache == nil {
		s.creditUsageCache = make(map[string]zhipuCreditUsageCacheEntry)
	}
	for existingKey, entry := range s.creditUsageCache {
		if !now.Before(entry.expiresAt) {
			delete(s.creditUsageCache, existingKey)
		}
	}
	s.creditUsageCache[key] = zhipuCreditUsageCacheEntry{
		credits:   zhipuCloneCreditRows(credits),
		expiresAt: expiresAt,
	}
}

func (s *ZhipuAccountMonitorService) nowTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

// ---------------------------------------------------------------------------
// 重置卡只读读取（design M4 / 票 12；R0：仅观测，永不使用）
// ---------------------------------------------------------------------------

// FetchResetStatusForAccount 只读读取登录托管智谱账号的可用重置卡。
//
// R0：本方法只经 zhipuResetStatusReader 发一个 GET（reset/status），不消耗重置卡，
// 也不存在任何使用/兑换入口；返回的是纯展示字段（类型 + 到期）。
//
// 静默语义（与 appendZhipuLoginFields 的积分明细同口径）：非登录托管账号、缺
// zcodejwttoken 或 access_token、读取器未接线（无 HTTP 上游）一律返回 (nil, nil)，
// 既不报错也不打上游；只有已发起读取后的上游失败才返回错误，由调用方降级为
// 「只缺字段」（绝不改写快照 Success/Error）。
func (s *ZhipuAccountMonitorService) FetchResetStatusForAccount(ctx context.Context, account *Account) ([]domain.MonitorResetCard, error) {
	if s == nil || s.resetStatus == nil || !account.IsZhipuLoginManaged() {
		return nil, nil
	}
	zcodeJWTToken := strings.TrimSpace(account.GetZhipuZCodeJWTToken())
	accessToken := strings.TrimSpace(account.GetZhipuAccessToken())
	if zcodeJWTToken == "" || accessToken == "" {
		// 两个头缺一上游即拒绝，缺凭据时一个请求都不发（快速路径，不打风控端点的闸）。
		return nil, nil
	}
	status, err := s.resetStatus.Fetch(ctx, zcodeJWTToken, accessToken)
	if err != nil {
		return nil, err
	}
	return zhipuResetCardsFromStatus(status), nil
}

// zhipuResetCardsFromStatus 把读取器的两池卡归并为快照字段：five_hour 在前、week 在后
// （与 MonitorQuotaView 的展示顺序一致）。两池都空（或无状态）返回 nil，让快照 JSON
// 省略字段——「无卡」与「未采集」对下游同形，前端不虚构 0 张卡片。
func zhipuResetCardsFromStatus(status *bigmodel.ResetStatus) []domain.MonitorResetCard {
	if status == nil {
		return nil
	}
	cards := make([]domain.MonitorResetCard, 0, len(status.FiveHourCards)+len(status.WeekCards))
	for _, card := range status.FiveHourCards {
		cards = append(cards, zhipuResetCardToSnapshot(card))
	}
	for _, card := range status.WeekCards {
		cards = append(cards, zhipuResetCardToSnapshot(card))
	}
	if len(cards) == 0 {
		return nil
	}
	return cards
}

// zhipuResetCardToSnapshot 把读取器的一张卡映射为只读展示字段：Type 原样透出
// （"five_hour" / "week"），ExpireAt 归一为 RFC3339 UTC（上游未给 → 空串，前端按
// 「到期时间未知」展示）。除展示字段外不带任何执行字段（R0：无卡 id）。
//
// 不修改读取器交回的 status（缓存命中时同一值会交给多个调用方）。
func zhipuResetCardToSnapshot(card bigmodel.ResetCard) domain.MonitorResetCard {
	snapshot := domain.MonitorResetCard{Type: string(card.Type)}
	if !card.ExpireAt.IsZero() {
		snapshot.ExpireAt = card.ExpireAt.UTC().Format(time.RFC3339)
	}
	return snapshot
}

// ---------------------------------------------------------------------------
// L2 费率对账的周期挂载与快照输出（design M5 费率对账段 / 票 27）
// ---------------------------------------------------------------------------

// SetSignReconciler 注入 L2 费率对账器（票 27）。装配期调用一次，运行期只读；
// 传 nil 等于未接线（周期入口与快照合并都成为空操作）。
func (s *ZhipuAccountMonitorService) SetSignReconciler(reconciler *ZhipuSignReconciler) {
	if s == nil {
		return
	}
	s.signReconcile = reconciler
}

// RunDueSignReconcile 是对账的**周期入口**：挂在 M4 既有周期任务（keeper loop 每轮）
// 内，每轮调一次——本服务不新建 goroutine/ticker（design M5：不新建 loop）。是否到期
// 由 ZhipuSignReconciler 的窗口口径决定；未接线时是空操作。
//
// 装配已就位（service/wire.go）：keeper.SetSignReconcileHook 挂载本方法，
// 对账器与生效配置面经 SetSignReconciler / SetConfigSource 注入。
func (s *ZhipuAccountMonitorService) RunDueSignReconcile(ctx context.Context) {
	if s == nil || s.signReconcile == nil {
		return
	}
	s.signReconcile.RunIfDue(ctx)
}

// SignReconcileResult 返回最近一次可展示的对账结果（ok=false 表示从未成功对账）。
func (s *ZhipuAccountMonitorService) SignReconcileResult() (ZhipuSignReconcileResult, bool) {
	if s == nil || s.signReconcile == nil {
		return ZhipuSignReconcileResult{}, false
	}
	return s.signReconcile.LastResult()
}

// ApplySignReconcileSnapshot 把最近一次 L2 对账结果合入配额快照（票 11 的快照字段）。
// 未接线或从未成功对账时保持快照原样——老历史行与既有消费方零变化。
//
// 抓取侧经 monitorSignReconcileSnapshotSink 动态下探调用
// （channel_monitor_quota_fetcher.go 的 appendZhipuLoginFields）。
func (s *ZhipuAccountMonitorService) ApplySignReconcileSnapshot(snapshot *domain.MonitorQuotaSnapshot) {
	if s == nil || snapshot == nil || s.signReconcile == nil {
		return
	}
	result, ok := s.signReconcile.LastResult()
	if !ok {
		return
	}
	snapshot.SignEffectiveRate = result.EffectiveRate
	snapshot.SignPeakFactor = result.PeakFactor
	snapshot.SignReconcileStale = result.Stale
	snapshot.SignReconcileDeviation = result.Deviation
	if !result.WindowEnd.IsZero() {
		windowEnd := result.WindowEnd
		snapshot.SignReconciledAt = &windowEnd
	}
}

// zhipuCreditUsageCacheKey 以账号 + 上游窗口字符串为键：不同自然日窗口不互相串用，
// 同一账号同一窗口的 keeper 与监控调用共享同一条目。
func zhipuCreditUsageCacheKey(accountID int64, startTime, endTime string) string {
	return "zhipu_credit_usage:" + strconv.FormatInt(accountID, 10) + ":" + startTime + ":" + endTime
}

func zhipuCloneCreditRows(credits []domain.MonitorQuotaModelCredit) []domain.MonitorQuotaModelCredit {
	if credits == nil {
		return nil
	}
	cloned := make([]domain.MonitorQuotaModelCredit, len(credits))
	copy(cloned, credits)
	return cloned
}

// parseZhipuCreditUsageDetail 解析 usage-detail 响应体为逐日逐模型明细。
//
// 结构（实测快照）：data.modelUsage.xTime 是自然日轴，modelDataList[].*Usage
// 是与 xTime 等长的逐日序列，token 为数字、credits 为数字字符串。
func parseZhipuCreditUsageDetail(body []byte) ([]domain.MonitorQuotaModelCredit, error) {
	root := gjson.ParseBytes(body)
	if code := root.Get("code"); !code.Exists() || code.Int() != zhipuCreditUsageCodeOK {
		return nil, fmt.Errorf("%w: upstream code %d: %s", ErrZhipuCreditUsageResponse, code.Int(), zhipuCreditUsageMessage(root))
	}
	modelUsage := root.Get("data.modelUsage")
	dataList := modelUsage.Get("modelDataList")
	if !dataList.Exists() || !dataList.IsArray() {
		return nil, fmt.Errorf("%w: response missing data.modelUsage.modelDataList", ErrZhipuCreditUsageResponse)
	}
	rows := dataList.Array()
	if len(rows) == 0 {
		return []domain.MonitorQuotaModelCredit{}, nil
	}
	dates := modelUsage.Get("xTime").Array()
	if len(dates) == 0 {
		return nil, fmt.Errorf("%w: response missing data.modelUsage.xTime", ErrZhipuCreditUsageResponse)
	}

	credits := make([]domain.MonitorQuotaModelCredit, 0, len(rows)*len(dates))
	for _, row := range rows {
		model := strings.TrimSpace(row.Get("modelCode").String())
		if model == "" {
			model = strings.TrimSpace(row.Get("modelName").String())
		}
		if model == "" {
			// 无模型标识的行无法归属（上游偶发返回纯拆分桶），跳过而非编造名字。
			continue
		}
		totalCredits := row.Get("totalCreditsUsage")
		uncachedTokens := row.Get("uncachedInputTokensUsage")
		cachedTokens := row.Get("cachedInputTokensUsage")
		outputTokens := row.Get("outputTokensUsage")
		for i := range dates {
			dayCredits := zhipuCreditUsageSeriesValue(totalCredits, i)
			if !totalCredits.IsArray() {
				// 无总计列时退化为三项之和（与官方客户端 mapper 同口径）。
				dayCredits = zhipuCreditUsageSeriesValue(row.Get("uncachedInputCreditsUsage"), i) +
					zhipuCreditUsageSeriesValue(row.Get("cachedInputCreditsUsage"), i) +
					zhipuCreditUsageSeriesValue(row.Get("outputCreditsUsage"), i)
			}
			credits = append(credits, domain.MonitorQuotaModelCredit{
				Model:        model,
				Date:         dates[i].String(),
				InputTokens:  zhipuCreditUsageSeriesValue(uncachedTokens, i),
				CachedTokens: zhipuCreditUsageSeriesValue(cachedTokens, i),
				OutputTokens: zhipuCreditUsageSeriesValue(outputTokens, i),
				Credits:      dayCredits,
			})
		}
	}
	return credits, nil
}

// zhipuCreditUsageMessage 提取上游错误摘要，缺失时给出稳定占位文案。
func zhipuCreditUsageMessage(root gjson.Result) string {
	message := strings.TrimSpace(root.Get("msg").String())
	if message == "" {
		message = "unknown zhipu credit usage error"
	}
	return truncate(message, zhipuCreditUsageMessageMaxBytes)
}

// zhipuCreditUsageSeriesValue 取日序列第 index 项：数字与数字字符串都接受，
// 缺项 / null / 非数组 / 非数字一律视为 0（上游对零值会给 "0.0000"）。
func zhipuCreditUsageSeriesValue(series gjson.Result, index int) float64 {
	if !series.IsArray() {
		return 0
	}
	items := series.Array()
	if index < 0 || index >= len(items) {
		return 0
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(items[index].String()), 64)
	if err != nil {
		return 0
	}
	return value
}
