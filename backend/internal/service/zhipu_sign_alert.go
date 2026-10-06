package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"go.uber.org/zap"
)

// 智谱签名 L1 实时指标、fail 策略与账号级熔断（design M3.1(b)(c)(d) / 票 24）。
//
// 本文件是「0.67 必须稳定、失效必须告警」的实时数据源与降级裁决点：
//
//   - 计数：签名热路径只做 O(1) 计数（Redis 5 分钟桶计数器，无 Redis 时退化为
//     进程内存并只告警一次 —— 对齐 OpsAlertEvaluatorService 的降级先例）；
//   - 策略：签不出来时按生效的 sign_fail_policy 裁决（open = 发无签名请求并计数，
//     closed = 交回可 failover 错误）；
//   - 熔断：单账号 5 分钟窗口内 verify_invalid 超阈值 → 内存摘除签名生效位
//     （绝不修改 credentials），窗口滚动或管理员显式恢复后自动复位。
//
// 通知本体（规则评估 / 事件落库 / 邮件）属票 25：本文件只产生指标与 L1 事件。
//
// 计数器键规范（冻结契约，票 25 的指标源与告警文案按此拼键）：
//
//	zhipu:sign:{kind}:{bucket}            全局维度
//	zhipu:sign:{kind}:{bucket}:acct:{id}  账号维度（告警文案定位到账号）
//
// kind ∈ {verify_invalid, handshake_fail, failopen, replay_ok, replay_fail}；
// bucket = Unix 秒 / 300（5 分钟桶，按 Unix 对齐；UTC+8 与本地 5 分钟刻度同一刻）。

// L1 指标种类（design M3.1(b) 的三类告警项 + 自愈重放结果）。字符串即计数器键
// 与事件里的 kind，是跨票契约，不得改名。
const (
	// ZhipuSignMetricVerifyInvalid 上游对已签名请求返回 VERIFY_* 的次数（含自愈检测点）。
	ZhipuSignMetricVerifyInvalid = "verify_invalid"
	// ZhipuSignMetricHandshakeFail 签名/握手失败次数（退避窗口内的拒绝不计，见 RecordSignFailure）。
	ZhipuSignMetricHandshakeFail = "handshake_fail"
	// ZhipuSignMetricFailOpen 降级为无签名发送的次数（降级即费率 ×1.5，必须可见）。
	ZhipuSignMetricFailOpen = "failopen"
	// ZhipuSignMetricReplayOK 自愈重放成功次数。
	ZhipuSignMetricReplayOK = "replay_ok"
	// ZhipuSignMetricReplayFail 自愈重放仍失败次数（传输失败或仍 >=400）。
	ZhipuSignMetricReplayFail = "replay_fail"
)

const (
	// zhipuSignMetricBucket 是 L1 计数器窗口粒度（design M3.1(b)：5 分钟桶）。
	zhipuSignMetricBucket = 5 * time.Minute
	// zhipuSignCounterKeyTTL 是桶键的存活时间：覆盖「当前桶 + 上一桶」，让跨越桶边界的
	// 读取（评估周期与请求同刻发生）仍能看到刚滚过去的窗口。
	zhipuSignCounterKeyTTL = 2 * zhipuSignMetricBucket
	// zhipuSignCounterKeyPrefix 是全部 L1 计数器键的公共前缀。
	zhipuSignCounterKeyPrefix = "zhipu:sign:"
	// zhipuSignCounterAccountInfix 是账号维度的键中缀（design M3.1(b) 的 :acct:{id}）。
	zhipuSignCounterAccountInfix = ":acct:"
)

// ZhipuSignCircuitBreakReasonVerifyInvalid 是账号级熔断的唯一原因码（窗口内验签失效
// 超阈值）。前端 29/30 按原因码做本地化，不解析任何自然语言。
const ZhipuSignCircuitBreakReasonVerifyInvalid = "verify_invalid_over_threshold"

// ZhipuSignCounterCache 是 L1 桶计数器的存储接缝。生产实现是 internal/repository 的
// Redis 实现（service 包不得直接依赖 redis，depguard）；为 nil 或出错时退化为进程内存
// 并只告警一次，绝不阻塞请求、绝不 panic（design M3.1(b) 的风险条款）。
type ZhipuSignCounterCache interface {
	// IncrZhipuSignCounter 原子递增一个桶键并返回递增后的值，ttl 只在该键首次创建时生效。
	IncrZhipuSignCounter(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// GetZhipuSignCounter 读取一个桶键的当前值；键不存在返回 0 且 err 为 nil。
	GetZhipuSignCounter(ctx context.Context, key string) (int64, error)
}

// ZhipuSignL1Event 是一条 L1 实时事件（状态变化类，如账号熔断触发）。
//
// 窗口计数走拉取（ZhipuSignAlerts.CounterSnapshot），状态变化走推送（本事件）：
// 票 25 的评估器两者都用 —— 拉取用于「5 分钟窗口计数 > 阈值」的规则，
// 推送用于「熔断刚发生」这种一次性事实。
type ZhipuSignL1Event struct {
	// Kind 是事件对应的指标种类（熔断事件为 ZhipuSignMetricVerifyInvalid）。
	Kind string `json:"kind"`
	// AccountID 是定位到的账号；0 表示全局事件。
	AccountID int64 `json:"account_id"`
	// Bucket 是 5 分钟桶编号（Unix 秒 / 300）。
	Bucket int64 `json:"bucket"`
	// Count 是触发时刻该窗口的计数。
	Count int64 `json:"count"`
	// Threshold 是触发时生效的阈值。
	Threshold int64 `json:"threshold"`
	// Reason 是稳定原因码（如 ZhipuSignCircuitBreakReasonVerifyInvalid），不含自然语言。
	Reason string `json:"reason"`
	// At 是事件时刻（引擎时钟）。
	At time.Time `json:"at"`
}

// ZhipuSignEventSink 是 L1 事件的消费接缝：票 25 在装配期用 SetEventSink 接过事件，
// 映射到 OpsAlertRule/事件落库/邮件。未注入时事件只落一条 Warn 日志。
type ZhipuSignEventSink func(event ZhipuSignL1Event)

// ZhipuSignCounterSnapshot 是某类指标在当前窗口的只读计数快照（票 25 的指标源）。
type ZhipuSignCounterSnapshot struct {
	Kind string `json:"kind"`
	// Bucket 是 5 分钟桶编号。
	Bucket int64 `json:"bucket"`
	// Global 是全局维度计数：Redis 可用时取 Redis（跨实例聚合），否则取本进程内存。
	Global int64 `json:"global"`
	// Accounts 是账号维度计数（本进程内存镜像；用于告警文案定位）。没有账号维度计数
	// 时为已初始化的空 map（前端与 JSON 序列化都按空集合渲染）。
	Accounts map[int64]int64 `json:"accounts"`
}

// ZhipuSignCircuitBreakState 是单个账号的熔断只读状态（票 28 的状态投影 / 票 25 的
// 事件文案都读它）。零值 = 未熔断。
type ZhipuSignCircuitBreakState struct {
	Tripped bool `json:"tripped"`
	// Reason 是稳定原因码；未熔断为空串。
	Reason string `json:"reason"`
	// Since 是触发时刻；未熔断为零值时间。
	Since time.Time `json:"since"`
	// WindowCount 是触发时该 5 分钟窗口的 verify_invalid 计数。
	WindowCount int64 `json:"window_count"`
	// Threshold 是触发时生效的阈值。
	Threshold int64 `json:"threshold"`
	// Bucket 是触发时所在窗口编号。
	Bucket int64 `json:"bucket"`
}

// zhipuSignCircuitBreak 是内存熔断标记：只在触发窗口内有效（不进 DB、不进 Redis，
// 重启即恢复 —— design M3.1(d) 的兜底条款）。窗口滚动后由状态读取惰性清除。
type zhipuSignCircuitBreak struct {
	Reason      string
	Since       time.Time
	WindowCount int64
	Threshold   int64
	Bucket      int64
}

// zhipuSignCounterKey 是内存镜像与熔断判定的内部键：accountID=0 表示全局维度。
type zhipuSignCounterKey struct {
	kind      string
	bucket    int64
	accountID int64
}

// zhipuSignConfigSource 是 fail 策略与熔断阈值的生效值来源（票 28 的
// *ZhipuSignConfigService 即满足：部署层 + system_settings 运行层覆盖，读进程内缓存，
// 因此「改配置热生效」在本文件里就是每次裁决都读一次生效值）。
type zhipuSignConfigSource interface {
	Effective(ctx context.Context) ZhipuSignConfig
}

// ZhipuSignAlerts 是 L1 指标 / fail 策略 / 账号级熔断的引擎。全部方法都对 nil 接收者
// 安全（未接线时零行为变化、零副作用）。
type ZhipuSignAlerts struct {
	cfg    *config.Config
	cache  ZhipuSignCounterCache
	source zhipuSignConfigSource
	now    func() time.Time

	// counterDegrade 是「计数器退化到进程内存」的一次性告警（warnOnce 语义）。
	// 生产默认写一条 Warn 日志；测试可替换以观测次数。
	counterDegrade func(err error)

	mu         sync.Mutex
	counts     map[zhipuSignCounterKey]int64
	prunedAt   int64
	breakers   map[int64]zhipuSignCircuitBreak
	sink       ZhipuSignEventSink
	degraded   bool
	degradeOne sync.Once
}

// NewZhipuSignAlerts 构造 L1 引擎。cache 为 nil（无 Redis / 构造失败）时计数器退化为
// 进程内存；source 为 nil 时策略与阈值取部署层配置（cfg 为 nil 再用协议默认）；
// now 为 nil 时用 time.Now。
func NewZhipuSignAlerts(
	cfg *config.Config,
	cache ZhipuSignCounterCache,
	source zhipuSignConfigSource,
	now func() time.Time,
) *ZhipuSignAlerts {
	if now == nil {
		now = time.Now
	}
	return &ZhipuSignAlerts{
		cfg:            cfg,
		cache:          cache,
		source:         source,
		now:            now,
		counterDegrade: zhipuSignLogCounterDegrade,
		counts:         make(map[zhipuSignCounterKey]int64),
		breakers:       make(map[int64]zhipuSignCircuitBreak),
	}
}

// SetEventSink 注入 L1 事件消费者（票 25 的装配点）。装配期调用，运行期只读。
func (a *ZhipuSignAlerts) SetEventSink(sink ZhipuSignEventSink) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sink = sink
}

// ZhipuSignCounterKey 返回某类指标某个 5 分钟桶的全局维度键（冻结契约）。
func ZhipuSignCounterKey(kind string, bucket int64) string {
	return fmt.Sprintf("%s%s:%d", zhipuSignCounterKeyPrefix, kind, bucket)
}

// ZhipuSignAccountCounterKey 返回某类指标某个 5 分钟桶的账号维度键（冻结契约）。
func ZhipuSignAccountCounterKey(kind string, bucket int64, accountID int64) string {
	return fmt.Sprintf("%s%s%d", ZhipuSignCounterKey(kind, bucket), zhipuSignCounterAccountInfix, accountID)
}

// zhipuSignMetricBucketID 把时刻映射到 5 分钟桶编号（Unix 秒 / 300）。
// 边界语义：桶区间为 [t0, t0+5min)，t0 是 Unix 纪元起 300 秒的整数倍。
func zhipuSignMetricBucketID(at time.Time) int64 {
	return at.Unix() / int64(zhipuSignMetricBucket/time.Second)
}

// FailPolicy 返回当前生效的 fail 策略（默认 open；未知值一律按 open 处理，
// 因为「不接受降级流量」必须是显式配置的运营决策，不能由坏数据触发）。
func (a *ZhipuSignAlerts) FailPolicy(ctx context.Context) string {
	if a == nil {
		return ZhipuSignFailPolicyOpen
	}
	if a.source != nil {
		return zhipuSignNormalizeFailPolicy(a.source.Effective(ctx).SignFailPolicy)
	}
	return zhipuSignFailPolicyFromConfig(a.cfg)
}

// CircuitBreakThreshold 返回当前生效的账号级熔断阈值（默认 10，越界回落默认值）。
func (a *ZhipuSignAlerts) CircuitBreakThreshold(ctx context.Context) int {
	if a == nil {
		return zhipuSignProtocolDefaults().SignAccountCircuitBreakThreshold
	}
	if a.source != nil {
		return zhipuSignSanitizeCircuitBreakThreshold(a.source.Effective(ctx).SignAccountCircuitBreakThreshold)
	}
	if a.cfg == nil {
		return zhipuSignProtocolDefaults().SignAccountCircuitBreakThreshold
	}
	return zhipuSignSanitizeCircuitBreakThreshold(a.cfg.Gateway.Zhipu.SignAccountCircuitBreakThreshold)
}

// CounterDegraded 表示计数器是否已退化到进程内存（Redis 缺失或出错，只告警一次）。
// 只读，供票 25/31 判断「指标是否可信」。
func (a *ZhipuSignAlerts) CounterDegraded() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.degraded
}

// RecordSignFailure 记录一次「签不出来」。命中每 key 退避窗口的错误不是新的握手失败
// （与 zcodesign.KeyStatus.ConsecutiveFailures 的口径一致）；其余失败计 handshake_fail
// ——签名热路径上唯一的 I/O 就是握手，故这是保守且可解释的分类。
func (a *ZhipuSignAlerts) RecordSignFailure(ctx context.Context, accountID int64, err error) {
	if a == nil {
		return
	}
	if err == nil || !errors.Is(err, zcodesign.ErrHandshakeBackoff) {
		a.record(ctx, ZhipuSignMetricHandshakeFail, accountID)
	}
}

// RecordFailOpen 记录一次降级为无签名发送（费率 ×1.5 的可见化）。
func (a *ZhipuSignAlerts) RecordFailOpen(ctx context.Context, accountID int64) {
	if a == nil {
		return
	}
	a.record(ctx, ZhipuSignMetricFailOpen, accountID)
}

// RecordVerifyInvalid 记录一次「已签名请求被 VERIFY_* 拒绝」，并在同一窗口计数达到
// 阈值时触发账号级熔断（摘除签名生效位 + 产生 L1 事件）。
func (a *ZhipuSignAlerts) RecordVerifyInvalid(ctx context.Context, accountID int64) {
	if a == nil {
		return
	}
	a.record(ctx, ZhipuSignMetricVerifyInvalid, accountID)
	a.evaluateCircuitBreak(ctx, accountID)
}

// RecordReplay 记录一次自愈重放结果（成功计 replay_ok，失败计 replay_fail）。
func (a *ZhipuSignAlerts) RecordReplay(ctx context.Context, accountID int64, ok bool) {
	if a == nil {
		return
	}
	if ok {
		a.record(ctx, ZhipuSignMetricReplayOK, accountID)
		return
	}
	a.record(ctx, ZhipuSignMetricReplayFail, accountID)
}

// CounterSnapshot 返回某类指标当前窗口的计数（全局 + 账号维度）。
//
// 全局维度优先取 Redis（跨实例聚合，失败则退化内存并告警一次）；账号维度取本进程
// 内存镜像 —— 它同时是熔断判定的输入，且告警文案只需要「哪个账号」这一层定位。
func (a *ZhipuSignAlerts) CounterSnapshot(ctx context.Context, kind string) ZhipuSignCounterSnapshot {
	snapshot := ZhipuSignCounterSnapshot{Kind: kind, Accounts: map[int64]int64{}}
	if a == nil {
		return snapshot
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bucket := zhipuSignMetricBucketID(a.now())
	snapshot.Bucket = bucket

	a.mu.Lock()
	snapshot.Global = a.counts[zhipuSignCounterKey{kind: kind, bucket: bucket}]
	for key, count := range a.counts {
		if key.kind == kind && key.bucket == bucket && key.accountID > 0 {
			snapshot.Accounts[key.accountID] = count
		}
	}
	a.mu.Unlock()

	if a.cache != nil {
		value, err := a.cache.GetZhipuSignCounter(ctx, ZhipuSignCounterKey(kind, bucket))
		if err != nil {
			a.reportCounterDegrade(err)
			return snapshot
		}
		snapshot.Global = value
	}
	return snapshot
}

// CircuitBreakState 返回账号的熔断只读状态；窗口已滚动（标记属于旧桶）时惰性清除标记
// 并返回「未熔断」——「窗口滚动后恢复」不需要任何后台任务。
func (a *ZhipuSignAlerts) CircuitBreakState(accountID int64) ZhipuSignCircuitBreakState {
	if a == nil || accountID <= 0 {
		return ZhipuSignCircuitBreakState{}
	}
	bucket := zhipuSignMetricBucketID(a.now())

	a.mu.Lock()
	mark, ok := a.breakers[accountID]
	if !ok || mark.Bucket != bucket {
		delete(a.breakers, accountID)
		a.mu.Unlock()
		return ZhipuSignCircuitBreakState{}
	}
	count := a.counts[zhipuSignCounterKey{kind: ZhipuSignMetricVerifyInvalid, bucket: bucket, accountID: accountID}]
	a.mu.Unlock()

	return ZhipuSignCircuitBreakState{
		Tripped:     true,
		Reason:      mark.Reason,
		Since:       mark.Since,
		WindowCount: count,
		Threshold:   mark.Threshold,
		Bucket:      mark.Bucket,
	}
}

// RestoreCircuitBreak 是「管理员确认版本后恢复」的唯一入口：清除熔断标记并归零该账号
// 当前窗口的 verify_invalid 计数 —— 版本修正后不应被旧证据在新请求上立刻再次触发。
// 恢复只影响内存状态，不触碰 credentials（与触发路径同款约束）。
func (a *ZhipuSignAlerts) RestoreCircuitBreak(accountID int64) {
	if a == nil || accountID <= 0 {
		return
	}
	bucket := zhipuSignMetricBucketID(a.now())
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.breakers, accountID)
	delete(a.counts, zhipuSignCounterKey{kind: ZhipuSignMetricVerifyInvalid, bucket: bucket, accountID: accountID})
}

// record 是全部计数的唯一写入点：先写内存镜像（熔断判定与账号维度读取的事实源），
// 再写 Redis 桶计数器（跨实例的全局维度）。Redis 失败只影响跨实例聚合，不影响本地判定。
func (a *ZhipuSignAlerts) record(ctx context.Context, kind string, accountID int64) {
	if a == nil || kind == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bucket := zhipuSignMetricBucketID(a.now())
	a.recordMemory(kind, bucket, accountID)

	if a.cache == nil {
		a.reportCounterDegrade(errZhipuSignCounterCacheMissing)
		return
	}
	keys := []string{ZhipuSignCounterKey(kind, bucket)}
	if accountID > 0 {
		keys = append(keys, ZhipuSignAccountCounterKey(kind, bucket, accountID))
	}
	for _, key := range keys {
		if _, err := a.cache.IncrZhipuSignCounter(ctx, key, zhipuSignCounterKeyTTL); err != nil {
			a.reportCounterDegrade(err)
			return
		}
	}
}

// recordMemory 递增内存镜像并淘汰过旧的桶（只保留当前桶与上一桶）。
func (a *ZhipuSignAlerts) recordMemory(kind string, bucket int64, accountID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counts == nil {
		a.counts = make(map[zhipuSignCounterKey]int64)
	}
	if a.prunedAt != bucket {
		for key := range a.counts {
			if key.bucket < bucket-1 {
				delete(a.counts, key)
			}
		}
		a.prunedAt = bucket
	}
	a.counts[zhipuSignCounterKey{kind: kind, bucket: bucket, accountID: accountID}]++
}

// evaluateCircuitBreak 在计数写入后判定熔断：同一窗口内重复达标只触发一次事件，
// 新窗口重新达标会重新触发（标记随窗口滚动失效）。
func (a *ZhipuSignAlerts) evaluateCircuitBreak(ctx context.Context, accountID int64) {
	if a == nil || accountID <= 0 {
		return
	}
	threshold := a.CircuitBreakThreshold(ctx)
	if threshold <= 0 {
		return
	}
	at := a.now()
	bucket := zhipuSignMetricBucketID(at)

	a.mu.Lock()
	count := a.counts[zhipuSignCounterKey{kind: ZhipuSignMetricVerifyInvalid, bucket: bucket, accountID: accountID}]
	if count < int64(threshold) {
		a.mu.Unlock()
		return
	}
	if existing, ok := a.breakers[accountID]; ok && existing.Bucket == bucket {
		a.mu.Unlock()
		return
	}
	if a.breakers == nil {
		a.breakers = make(map[int64]zhipuSignCircuitBreak)
	}
	a.breakers[accountID] = zhipuSignCircuitBreak{
		Reason:      ZhipuSignCircuitBreakReasonVerifyInvalid,
		Since:       at,
		WindowCount: count,
		Threshold:   int64(threshold),
		Bucket:      bucket,
	}
	sink := a.sink
	a.mu.Unlock()

	logger.L().Warn("zhipu client sign: account circuit breaker tripped, signing detached for this account",
		zap.Int64("account_id", accountID),
		zap.Int64("window_count", count),
		zap.Int("threshold", threshold),
		zap.Int64("bucket", bucket),
	)
	if sink != nil {
		sink(ZhipuSignL1Event{
			Kind:      ZhipuSignMetricVerifyInvalid,
			AccountID: accountID,
			Bucket:    bucket,
			Count:     count,
			Threshold: int64(threshold),
			Reason:    ZhipuSignCircuitBreakReasonVerifyInvalid,
			At:        at,
		})
	}
}

// reportCounterDegrade 处理计数器降级（无 Redis 或读写失败）：只告警一次（warnOnce），
// 之后静默退化 —— 指标是尽力而为的旁路，绝不因 Redis 抖动影响数据面。
func (a *ZhipuSignAlerts) reportCounterDegrade(err error) {
	if a == nil {
		return
	}
	a.degradeOne.Do(func() {
		a.mu.Lock()
		a.degraded = true
		report := a.counterDegrade
		a.mu.Unlock()
		if report != nil {
			report(err)
		}
	})
}

// errZhipuSignCounterCacheMissing 是「未装配 Redis 计数器」的降级原因（只用于告警文本）。
var errZhipuSignCounterCacheMissing = errors.New("zhipu sign: no counter cache configured")

func zhipuSignLogCounterDegrade(err error) {
	logger.L().Warn("zhipu sign metrics: Redis counter cache unavailable, degrading to process memory",
		zap.Error(err))
}

// zhipuSignNormalizeFailPolicy 把配置值收敛到两个合法策略（默认 open）。
func zhipuSignNormalizeFailPolicy(policy string) string {
	if strings.ToLower(strings.TrimSpace(policy)) == ZhipuSignFailPolicyClosed {
		return ZhipuSignFailPolicyClosed
	}
	return ZhipuSignFailPolicyOpen
}

// zhipuSignFailPolicyFromConfig 是引擎未接线时（测试装配/纯部署层）的策略读取。
func zhipuSignFailPolicyFromConfig(cfg *config.Config) string {
	if cfg == nil {
		return zhipuSignProtocolDefaults().SignFailPolicy
	}
	return zhipuSignNormalizeFailPolicy(cfg.Gateway.Zhipu.SignFailPolicy)
}

// zhipuSignSanitizeCircuitBreakThreshold 收敛阈值：越界（<1 或 >上限）回落默认值，
// 与票 28 的 sanitizeZhipuSignConfig 同口径。
func zhipuSignSanitizeCircuitBreakThreshold(threshold int) int {
	if threshold < 1 || threshold > zhipuSignCircuitBreakThresholdMax {
		return zhipuSignProtocolDefaults().SignAccountCircuitBreakThreshold
	}
	return threshold
}
