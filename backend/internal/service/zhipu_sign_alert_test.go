//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 智谱签名 L1 指标 / fail 策略 / 账号级熔断（design M3.1(b)(c)(d) / 票 24）的表驱动用例。
//
// 断言全部落在公开路径与接缝上：
//   - 计数器键与桶归属（ZhipuSignCounterKey / ZhipuSignAccountCounterKey / 5 分钟桶）；
//   - 两个签名挂点（复用票 22/23 的 CC 直转与 anthropic 原生直通入口）；
//   - 自愈检测点（票 23 的 VERIFY_* 路径）与 L1 计数的联动；
//   - 只读访问器（CounterSnapshot / CircuitBreakState / FailPolicy / CircuitBreakThreshold）；
//   - 管理端状态投影（票 28 的 ZhipuSignConfigService.Status）。
//
// 计数器要么用内存替身（断言键规范与调用），要么用真实进程内存镜像（无 Redis 的退化路径）。

// zhipuSignAlertTestBase 是一个 5 分钟桶的起点（Unix 1699999800 = 300 × 5666666），
// 让桶归属断言是「独立算出的整数」而不是实现的自证。
var zhipuSignAlertTestBase = time.Unix(1699999800, 0).UTC()

const (
	zhipuSignAlertTestBaseBucket = 5666666
	zhipuSignAlertTestAccountID  = 2201
)

// zhipuSignAlertClock 是可推进的测试时钟：桶归属与窗口滚动完全确定。
type zhipuSignAlertClock struct {
	mu sync.Mutex
	at time.Time
}

func newZhipuSignAlertClock(at time.Time) *zhipuSignAlertClock {
	return &zhipuSignAlertClock{at: at}
}

func (c *zhipuSignAlertClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *zhipuSignAlertClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// zhipuSignCounterCacheStub 是 ZhipuSignCounterCache 的内存替身：记录键、首次 TTL 与
// 调用顺序；err 非 nil 时一律失败（模拟 Redis 抖动）。
type zhipuSignCounterCacheStub struct {
	mu     sync.Mutex
	values map[string]int64
	ttls   map[string]time.Duration
	calls  []string
	err    error
}

func newZhipuSignCounterCacheStub() *zhipuSignCounterCacheStub {
	return &zhipuSignCounterCacheStub{
		values: make(map[string]int64),
		ttls:   make(map[string]time.Duration),
	}
}

func (c *zhipuSignCounterCacheStub) IncrZhipuSignCounter(_ context.Context, key string, ttl time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, key)
	if c.err != nil {
		return 0, c.err
	}
	c.values[key]++
	if _, ok := c.ttls[key]; !ok {
		c.ttls[key] = ttl
	}
	return c.values[key], nil
}

func (c *zhipuSignCounterCacheStub) GetZhipuSignCounter(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	return c.values[key], nil
}

func (c *zhipuSignCounterCacheStub) value(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

func (c *zhipuSignCounterCacheStub) ttl(key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttls[key]
}

func (c *zhipuSignCounterCacheStub) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// zhipuSignAlertConfigStub 是 zhipuSignConfigSource 的替身：生效值可在运行期改写，
// 用于断言「策略切换热生效（无需重启）」。
type zhipuSignAlertConfigStub struct {
	mu     sync.Mutex
	config ZhipuSignConfig
}

func newZhipuSignAlertConfigStub() *zhipuSignAlertConfigStub {
	return &zhipuSignAlertConfigStub{config: ZhipuSignConfig{
		SignFailPolicy:                   ZhipuSignFailPolicyOpen,
		SignAccountCircuitBreakThreshold: 10,
	}}
}

func (s *zhipuSignAlertConfigStub) Effective(context.Context) ZhipuSignConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

func (s *zhipuSignAlertConfigStub) setPolicy(policy string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.SignFailPolicy = policy
}

func (s *zhipuSignAlertConfigStub) setThreshold(threshold int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.SignAccountCircuitBreakThreshold = threshold
}

// zhipuSignAlertHarness 是票 24 用例的公共装配：注入时钟的引擎 + 假计数器 +
// 可热切换配置源 + L1 事件观测。
type zhipuSignAlertHarness struct {
	alerts *ZhipuSignAlerts
	cache  *zhipuSignCounterCacheStub
	source *zhipuSignAlertConfigStub
	clock  *zhipuSignAlertClock
	mu     sync.Mutex
	events []ZhipuSignL1Event
}

func newZhipuSignAlertHarness(cfg *config.Config) *zhipuSignAlertHarness {
	harness := &zhipuSignAlertHarness{
		cache:  newZhipuSignCounterCacheStub(),
		source: newZhipuSignAlertConfigStub(),
		clock:  newZhipuSignAlertClock(zhipuSignAlertTestBase),
	}
	harness.alerts = NewZhipuSignAlerts(cfg, harness.cache, harness.source, harness.clock.now)
	harness.alerts.SetEventSink(func(event ZhipuSignL1Event) {
		harness.mu.Lock()
		defer harness.mu.Unlock()
		harness.events = append(harness.events, event)
	})
	return harness
}

// bucket 是当前时钟所在的 5 分钟桶编号。
func (h *zhipuSignAlertHarness) bucket() int64 {
	return zhipuSignMetricBucketID(h.clock.now())
}

// global 读某类指标的全局维度计数（Redis 替身优先，与生产读取口径一致）。
func (h *zhipuSignAlertHarness) global(kind string) int64 {
	return h.alerts.CounterSnapshot(context.Background(), kind).Global
}

// account 读某类指标的账号维度计数（进程内存镜像）。
func (h *zhipuSignAlertHarness) account(kind string, accountID int64) int64 {
	return h.alerts.CounterSnapshot(context.Background(), kind).Accounts[accountID]
}

func (h *zhipuSignAlertHarness) emittedEvents() []ZhipuSignL1Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ZhipuSignL1Event(nil), h.events...)
}

// zhipuSignAlertTestBody 是一条最小的 CC 请求体（两个挂点共用）。
func zhipuSignAlertTestBody() []byte {
	return []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)
}

// zhipuSignAlertService 把引擎接到网关上（与 wire 装配同款注入）。
func zhipuSignAlertService(cfg *config.Config, signer zhipuClientSigner, upstream HTTPUpstream, alerts *ZhipuSignAlerts) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg:             cfg,
		httpUpstream:    upstream,
		zhipuSigner:     signer,
		zhipuSignAlerts: alerts,
	}
}

// TestZhipuSignMetricBucketOwnershipAndKeySpec 覆盖验收标准第一条：桶归属（t 与 t+5min
// 落在不同桶、边界值固定）与键规范（全局 + :acct:{accountID} 后缀）。
func TestZhipuSignMetricBucketOwnershipAndKeySpec(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"桶起点", zhipuSignAlertTestBase, zhipuSignAlertTestBaseBucket},
		{"桶内最后一秒", zhipuSignAlertTestBase.Add(299 * time.Second), zhipuSignAlertTestBaseBucket},
		{"下一个桶的第一秒（边界）", zhipuSignAlertTestBase.Add(300 * time.Second), zhipuSignAlertTestBaseBucket + 1},
		{"t+5min 落在下一个桶", zhipuSignAlertTestBase.Add(5 * time.Minute), zhipuSignAlertTestBaseBucket + 1},
		{"跨两个窗口", zhipuSignAlertTestBase.Add(10 * time.Minute), zhipuSignAlertTestBaseBucket + 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, zhipuSignMetricBucketID(tc.at))
		})
	}

	require.Equal(t, "zhipu:sign:verify_invalid:5666666",
		ZhipuSignCounterKey(ZhipuSignMetricVerifyInvalid, 5666666))
	require.Equal(t, "zhipu:sign:verify_invalid:5666666:acct:2201",
		ZhipuSignAccountCounterKey(ZhipuSignMetricVerifyInvalid, 5666666, 2201))
	require.Equal(t, "zhipu:sign:failopen:5666667:acct:42",
		ZhipuSignAccountCounterKey(ZhipuSignMetricFailOpen, 5666667, 42))
}

// TestZhipuSignAlertCountsBothDimensions 覆盖验收标准：热路径计数同时写全局与账号维度，
// 并带上桶键 TTL（跨实例聚合与告警定位的事实源）。
func TestZhipuSignAlertCountsBothDimensions(t *testing.T) {
	h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
	ctx := context.Background()

	h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
	h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
	h.alerts.RecordVerifyInvalid(ctx, 3300)

	globalKey := ZhipuSignCounterKey(ZhipuSignMetricVerifyInvalid, zhipuSignAlertTestBaseBucket)
	accountKey := ZhipuSignAccountCounterKey(ZhipuSignMetricVerifyInvalid, zhipuSignAlertTestBaseBucket, zhipuSignAlertTestAccountID)

	require.Equal(t, int64(3), h.cache.value(globalKey), "全局维度必须计数")
	require.Equal(t, int64(2), h.cache.value(accountKey), "账号维度必须计数")
	require.Equal(t, zhipuSignCounterKeyTTL, h.cache.ttl(globalKey), "桶键必须带 TTL（不落库、不过期即泄漏）")
	require.Equal(t, zhipuSignCounterKeyTTL, h.cache.ttl(accountKey))

	require.Contains(t, h.cache.keys(), globalKey)
	require.Contains(t, h.cache.keys(), accountKey)

	snapshot := h.alerts.CounterSnapshot(ctx, ZhipuSignMetricVerifyInvalid)
	require.Equal(t, ZhipuSignMetricVerifyInvalid, snapshot.Kind)
	require.Equal(t, int64(zhipuSignAlertTestBaseBucket), snapshot.Bucket)
	require.Equal(t, int64(3), snapshot.Global)
	require.Equal(t, map[int64]int64{zhipuSignAlertTestAccountID: 2, 3300: 1}, snapshot.Accounts)

	other := h.alerts.CounterSnapshot(ctx, ZhipuSignMetricHandshakeFail)
	require.Zero(t, other.Global, "未发生的指标必须读作 0")
	require.Empty(t, other.Accounts)
}

// TestZhipuSignAlertCounterDegradeWarnsOnceAndKeepsCounting 覆盖验收标准：Redis 不可用
// （未装配或读写失败）时退化为进程内存、只告警一次，且计数不丢（对账 27 兜底）。
func TestZhipuSignAlertCounterDegradeWarnsOnceAndKeepsCounting(t *testing.T) {
	cases := []struct {
		name  string
		cache ZhipuSignCounterCache
	}{
		{name: "未装配 Redis 计数器", cache: nil},
		{name: "Redis 读写失败", cache: func() ZhipuSignCounterCache {
			stub := newZhipuSignCounterCacheStub()
			stub.err = errors.New("redis: connection refused")
			return stub
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := zhipuSignTestConfig(true)
			alerts := NewZhipuSignAlerts(cfg, tc.cache, nil, newZhipuSignAlertClock(zhipuSignAlertTestBase).now)
			var degradations []error
			alerts.counterDegrade = func(err error) { degradations = append(degradations, err) }

			for i := 0; i < 3; i++ {
				alerts.RecordFailOpen(context.Background(), zhipuSignAlertTestAccountID)
			}

			require.Len(t, degradations, 1, "退化告警必须只出现一次（warnOnce）")
			require.True(t, alerts.CounterDegraded())

			snapshot := alerts.CounterSnapshot(context.Background(), ZhipuSignMetricFailOpen)
			require.Equal(t, int64(3), snapshot.Global, "退化后计数必须继续落在进程内存")
			require.Equal(t, int64(3), snapshot.Accounts[zhipuSignAlertTestAccountID])
		})
	}
}

// TestZhipuSignAlertHandshakeBackoffIsNotANewHandshakeFailure 覆盖计数口径：每 key 退避
// 窗口内被拒的请求不是新的握手失败（与 Signer.KeyStatus 的口径一致），但 Sign 本身失败
// 一定计 handshake_fail。
func TestZhipuSignAlertHandshakeBackoffIsNotANewHandshakeFailure(t *testing.T) {
	h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
	ctx := context.Background()

	h.alerts.RecordSignFailure(ctx, zhipuSignAlertTestAccountID, fmt.Errorf("sign failed: %w", zcodesign.ErrHandshakeBackoff))
	require.Zero(t, h.global(ZhipuSignMetricHandshakeFail), "退避窗口内的拒绝不计握手失败")

	h.alerts.RecordSignFailure(ctx, zhipuSignAlertTestAccountID, errors.New("handshake: business code 3012"))
	require.Equal(t, int64(1), h.global(ZhipuSignMetricHandshakeFail))
	require.Equal(t, int64(1), h.account(ZhipuSignMetricHandshakeFail, zhipuSignAlertTestAccountID))
}

// TestZhipuSignFailPolicyOnSignFailure 覆盖验收标准第二条（表驱动）：签不出来时
//   - open：剥离签名头、发无签名请求，且 failopen 计数恰好 +1；
//   - closed：不下发任何请求，交回与调度器 failover 契约一致的可 failover 错误。
func TestZhipuSignFailPolicyOnSignFailure(t *testing.T) {
	cases := []struct {
		name          string
		policy        string
		wantRequested bool
		wantFailOpen  int64
	}{
		{name: "open：剥签降级发送", policy: ZhipuSignFailPolicyOpen, wantRequested: true, wantFailOpen: 1},
		{name: "closed：不下发无签名请求", policy: ZhipuSignFailPolicyClosed, wantRequested: false, wantFailOpen: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			body := zhipuSignAlertTestBody()

			h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
			h.source.setPolicy(tc.policy)

			signer := &zhipuSignStubSigner{err: errors.New("handshake: business code 3012")}
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
			account := zhipuSignTestAccount(t, map[string]any{})

			_, err := svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")

			require.Error(t, err)
			require.Equal(t, 1, signer.calls, "签名失败路径必须恰好尝试签名一次")
			require.Equal(t, int64(1), h.global(ZhipuSignMetricHandshakeFail))
			require.Equal(t, tc.wantFailOpen, h.global(ZhipuSignMetricFailOpen))

			if tc.wantRequested {
				require.NotNil(t, upstream.lastReq, "open：降级请求必须照常发出")
				require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
				zhipuSignAssertUpstreamHeaders(t, upstream.lastReq.Header, false)
				return
			}

			require.Nil(t, upstream.lastReq, "closed：不得下发无签名请求")
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, ZhipuSignFailClosedReason, failoverErr.Reason)
			require.Equal(t, http.StatusBadGateway, failoverErr.ClientStatusCode)
			require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
			require.True(t, failoverErr.ShouldRetryNextAccount(), "必须落在调度器的换账号重试契约上")
		})
	}
}

// TestZhipuSignFailPolicyHotSwitch 覆盖验收标准：策略切换热生效（同一进程内改生效值，
// 下一次请求立即按新策略执行，无需重启）。
func TestZhipuSignFailPolicyHotSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuSignAlertTestBody()
	cfg := zhipuSignTestConfig(true)

	h := newZhipuSignAlertHarness(cfg)
	account := zhipuSignTestAccount(t, map[string]any{})

	sign := func(t *testing.T, signer *zhipuSignStubSigner, upstream *httpUpstreamRecorder) error {
		t.Helper()
		svc := zhipuSignAlertService(cfg, signer, upstream, h.alerts)
		_, err := svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
		return err
	}

	// 第一步：生效值 open（默认）→ 降级发送。
	openUpstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	require.Error(t, sign(t, &zhipuSignStubSigner{err: errors.New("handshake failed")}, openUpstream))
	require.NotNil(t, openUpstream.lastReq)
	require.Equal(t, int64(1), h.global(ZhipuSignMetricFailOpen))

	// 第二步：同一进程内把生效值改为 closed → 立即不再下发无签名请求。
	h.source.setPolicy(ZhipuSignFailPolicyClosed)
	closedUpstream := &httpUpstreamRecorder{err: errors.New("must not be called")}
	err := sign(t, &zhipuSignStubSigner{err: errors.New("handshake failed")}, closedUpstream)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, ZhipuSignFailClosedReason, failoverErr.Reason)
	require.Nil(t, closedUpstream.lastReq, "热切换后必须立即生效")
	require.Equal(t, int64(1), h.global(ZhipuSignMetricFailOpen), "closed 期间不得再计 failopen")

	// 第三步：切回 open → 立即恢复降级发送。
	h.source.setPolicy(ZhipuSignFailPolicyOpen)
	backUpstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	require.Error(t, sign(t, &zhipuSignStubSigner{err: errors.New("handshake failed")}, backUpstream))
	require.NotNil(t, backUpstream.lastReq)
	require.Equal(t, int64(2), h.global(ZhipuSignMetricFailOpen))
}

// TestZhipuSignAccountCircuitBreaker 覆盖验收标准第三条：5 分钟窗口内 verify_invalid 超
// 阈值 → 内存摘除签名生效位（不改 credentials）+ L1 事件；阈值-1 不触发；窗口滚动后
// 恢复；管理员恢复后计数归零。
func TestZhipuSignAccountCircuitBreaker(t *testing.T) {
	t.Run("阈值-1 不触发，仍正常签名", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		body := zhipuSignAlertTestBody()
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setThreshold(3)
		ctx := context.Background()

		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)

		state := h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID)
		require.False(t, state.Tripped, "低于阈值不得熔断")
		require.Empty(t, h.emittedEvents(), "未触发不得产生 L1 事件")

		signer := &zhipuSignStubSigner{}
		upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
		svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
		_, err := svc.forwardAsRawChatCompletions(ctx, zhipuSignTestContext(body), zhipuSignTestAccount(t, map[string]any{}), body, "")
		require.Error(t, err)
		require.Equal(t, 1, signer.calls, "未熔断账号必须照常签名")
		require.NotNil(t, upstream.lastReq)
		zhipuSignAssertUpstreamHeaders(t, upstream.lastReq.Header, true)
	})

	t.Run("达到阈值即摘除签名生效位并产生 L1 事件", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		body := zhipuSignAlertTestBody()
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setThreshold(3)
		ctx := context.Background()

		for i := 0; i < 3; i++ {
			h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		}

		state := h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID)
		require.True(t, state.Tripped)
		require.Equal(t, ZhipuSignCircuitBreakReasonVerifyInvalid, state.Reason)
		require.Equal(t, int64(3), state.WindowCount)
		require.Equal(t, int64(3), state.Threshold)
		require.Equal(t, h.bucket(), state.Bucket)

		events := h.emittedEvents()
		require.Len(t, events, 1, "熔断只产生一条事件（同一窗口内不重复）")
		require.Equal(t, ZhipuSignCircuitBreakReasonVerifyInvalid, events[0].Reason)
		require.Equal(t, int64(zhipuSignAlertTestAccountID), events[0].AccountID)
		require.Equal(t, int64(3), events[0].Count)

		// 后续请求不再签名（内存摘除生效位），open 策略下仍发送（并计 failopen）。
		signer := &zhipuSignStubSigner{}
		upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
		svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
		_, err := svc.forwardAsRawChatCompletions(ctx, zhipuSignTestContext(body), zhipuSignTestAccount(t, map[string]any{}), body, "")
		require.Error(t, err)
		require.Equal(t, 0, signer.calls, "熔断账号不得再签名")
		require.NotNil(t, upstream.lastReq, "open 策略下仍照常发送（无签名）")
		zhipuSignAssertUpstreamHeaders(t, upstream.lastReq.Header, false)
		require.Equal(t, int64(1), h.global(ZhipuSignMetricFailOpen), "熔断造成的降级必须可见")
	})

	t.Run("窗口滚动后自动恢复", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setThreshold(2)
		ctx := context.Background()

		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		require.True(t, h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID).Tripped)

		h.clock.advance(5 * time.Minute)

		require.False(t, h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID).Tripped,
			"窗口滚动后必须自动恢复（熔断只对触发窗口有效）")
		require.Zero(t, h.global(ZhipuSignMetricVerifyInvalid), "新窗口计数从 0 开始")

		// 恢复后签名重新生效（用真实请求路径验证「生效位」确实回来了）。
		gin.SetMode(gin.TestMode)
		body := zhipuSignAlertTestBody()
		signer := &zhipuSignStubSigner{}
		upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
		svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
		_, err := svc.forwardAsRawChatCompletions(ctx, zhipuSignTestContext(body), zhipuSignTestAccount(t, map[string]any{}), body, "")
		require.Error(t, err)
		require.Equal(t, 1, signer.calls, "窗口滚动后签名必须重新生效")
		require.NotNil(t, upstream.lastReq)
		zhipuSignAssertUpstreamHeaders(t, upstream.lastReq.Header, true)
	})

	t.Run("管理员恢复：清除标记并归零计数", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setThreshold(2)
		ctx := context.Background()

		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		require.True(t, h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID).Tripped)

		h.alerts.RestoreCircuitBreak(zhipuSignAlertTestAccountID)

		require.False(t, h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID).Tripped)
		require.Zero(t, h.account(ZhipuSignMetricVerifyInvalid, zhipuSignAlertTestAccountID),
			"恢复后当前窗口计数必须归零（否则版本修好后会被旧证据立刻再次熔断）")

		// 恢复后一次新的验签失效不应立即重新熔断（阈值 2，当前窗口计数从 0 起）。
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		require.False(t, h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID).Tripped)
	})

	t.Run("closed 策略下熔断账号交回可 failover 错误", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		body := zhipuSignAlertTestBody()
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setThreshold(2)
		h.source.setPolicy(ZhipuSignFailPolicyClosed)
		ctx := context.Background()

		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)

		signer := &zhipuSignStubSigner{}
		upstream := &httpUpstreamRecorder{err: errors.New("must not be called")}
		svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
		_, err := svc.forwardAsRawChatCompletions(ctx, zhipuSignTestContext(body), zhipuSignTestAccount(t, map[string]any{}), body, "")

		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, ZhipuSignFailClosedReason, failoverErr.Reason)
		require.Nil(t, upstream.lastReq, "closed 策略下熔断账号不得降级发送")
		require.Equal(t, 0, signer.calls)
		require.Zero(t, h.global(ZhipuSignMetricFailOpen))
	})

	t.Run("其它账号不受影响", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setThreshold(1)
		ctx := context.Background()

		h.alerts.RecordVerifyInvalid(ctx, zhipuSignAlertTestAccountID)
		require.True(t, h.alerts.CircuitBreakState(zhipuSignAlertTestAccountID).Tripped)
		require.False(t, h.alerts.CircuitBreakState(3300).Tripped, "熔断是账号级，不扩散到其它账号")
	})
}

// TestZhipuSignSelfHealL1CountersAndFailPolicy 覆盖验收标准的集成点：自愈检测点
// （票 23）必须恰好各计一次，且重放仍失败时按 fail 策略分支。
func TestZhipuSignSelfHealL1CountersAndFailPolicy(t *testing.T) {
	verifyBody := `{"error":{"code":40005,"message":"signature rejected","reason":"VERIFY_SIGNATURE_INVALID"}}`

	run := func(t *testing.T, h *zhipuSignAlertHarness, respond func(call int, req *http.Request) (*http.Response, error)) (*zhipuSelfHealUpstream, *zhipuSelfHealSignerStub, error) {
		t.Helper()
		gin.SetMode(gin.TestMode)
		body := zhipuSignAlertTestBody()
		signer := &zhipuSelfHealSignerStub{}
		upstream := &zhipuSelfHealUpstream{respond: respond}
		svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
		c, _ := zhipuSelfHealTestContext("/v1/chat/completions", body)
		_, err := svc.forwardAsRawChatCompletions(context.Background(), c, zhipuSignTestAccount(t, map[string]any{}), body, "")
		return upstream, signer, err
	}

	t.Run("重放成功：verify_invalid 与 replay_ok 各一次", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		upstream, signer, err := run(t, h, func(call int, _ *http.Request) (*http.Response, error) {
			if call == 1 {
				return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized, verifyBody), nil
			}
			return zhipuSelfHealJSONResponse(http.StatusOK, zhipuSelfHealCCSuccessBody), nil
		})

		require.NoError(t, err)
		require.Equal(t, 2, upstream.callCount(), "自愈重放恰好一次")
		require.Equal(t, []string{"1234567890abcdef"}, signer.invalidatedIDs())
		require.Equal(t, int64(1), h.global(ZhipuSignMetricVerifyInvalid))
		require.Equal(t, int64(1), h.account(ZhipuSignMetricVerifyInvalid, zhipuSignAlertTestAccountID))
		require.Equal(t, int64(1), h.global(ZhipuSignMetricReplayOK))
		require.Zero(t, h.global(ZhipuSignMetricReplayFail))
		require.Zero(t, h.global(ZhipuSignMetricFailOpen), "重放成功没有任何降级")
	})

	t.Run("重放仍失败(open)：计 replay_fail 并把响应交回既有错误链", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		upstream, _, err := run(t, h, func(_ int, _ *http.Request) (*http.Response, error) {
			return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized, verifyBody), nil
		})

		require.Error(t, err)
		require.Equal(t, 2, upstream.callCount(), "同一请求最多自愈 1 次：不得出现第三次发送")
		require.Equal(t, int64(1), h.global(ZhipuSignMetricVerifyInvalid))
		require.Equal(t, int64(1), h.global(ZhipuSignMetricReplayFail))
		require.Zero(t, h.global(ZhipuSignMetricReplayOK))

		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.NotEqual(t, ZhipuSignFailClosedReason, failoverErr.Reason,
			"open 策略不冒充「签不出来」的 failover 理由：交回既有错误链")
	})

	t.Run("重放仍失败(closed)：交回可 failover 错误", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		h.source.setPolicy(ZhipuSignFailPolicyClosed)
		upstream, _, err := run(t, h, func(_ int, _ *http.Request) (*http.Response, error) {
			return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized, verifyBody), nil
		})

		require.Equal(t, 2, upstream.callCount())
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, ZhipuSignFailClosedReason, failoverErr.Reason)
		require.True(t, failoverErr.ShouldRetryNextAccount())
		require.Equal(t, int64(1), h.global(ZhipuSignMetricReplayFail))
	})

	t.Run("重放传输失败：计 replay_fail 且只发送两次", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		upstream, _, err := run(t, h, func(call int, _ *http.Request) (*http.Response, error) {
			if call == 1 {
				return zhipuSelfHealErrorResponse(t, http.StatusUnauthorized,
					`{"error":{"message":"VERIFY_SIGNATURE_INVALID"}}`), nil
			}
			return nil, errors.New("dial tcp: connection refused")
		})

		require.Error(t, err)
		require.Equal(t, 2, upstream.callCount())
		require.Equal(t, int64(1), h.global(ZhipuSignMetricVerifyInvalid))
		require.Equal(t, int64(1), h.global(ZhipuSignMetricReplayFail))
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
	})

	t.Run("非 VERIFY_* 响应：零计数、不重放", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
		upstream, signer, err := run(t, h, func(_ int, _ *http.Request) (*http.Response, error) {
			return zhipuSelfHealErrorResponse(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`), nil
		})

		require.Error(t, err)
		require.Equal(t, 1, upstream.callCount(), "非 VERIFY_* 不得触发重放")
		require.Equal(t, 1, signer.signCallCount())
		require.Zero(t, h.global(ZhipuSignMetricVerifyInvalid))
		require.Zero(t, h.global(ZhipuSignMetricReplayOK))
		require.Zero(t, h.global(ZhipuSignMetricReplayFail))
		require.Zero(t, h.global(ZhipuSignMetricFailOpen))
	})
}

// TestZhipuSignAlertPolicyAndThresholdReadAccessors 覆盖验收标准第四条：只读访问器
// （当前策略、熔断阈值）—— 运行层生效值优先、未知/越界值回落安全默认。
func TestZhipuSignAlertPolicyAndThresholdReadAccessors(t *testing.T) {
	cases := []struct {
		name          string
		cfg           *config.Config
		source        *ZhipuSignConfig
		wantPolicy    string
		wantThreshold int
	}{
		{
			name:          "部署层缺省：open + 阈值 10（协议默认）",
			cfg:           zhipuSignTestConfig(true),
			wantPolicy:    ZhipuSignFailPolicyOpen,
			wantThreshold: 10,
		},
		{
			name: "部署层显式 closed + 阈值 3",
			cfg: func() *config.Config {
				cfg := zhipuSignTestConfig(true)
				cfg.Gateway.Zhipu.SignFailPolicy = ZhipuSignFailPolicyClosed
				cfg.Gateway.Zhipu.SignAccountCircuitBreakThreshold = 3
				return cfg
			}(),
			wantPolicy:    ZhipuSignFailPolicyClosed,
			wantThreshold: 3,
		},
		{
			name: "未知策略与越界阈值一律回落安全默认",
			cfg: func() *config.Config {
				cfg := zhipuSignTestConfig(true)
				cfg.Gateway.Zhipu.SignFailPolicy = "chaos"
				cfg.Gateway.Zhipu.SignAccountCircuitBreakThreshold = -5
				return cfg
			}(),
			wantPolicy:    ZhipuSignFailPolicyOpen,
			wantThreshold: 10,
		},
		{
			name:          "运行层覆盖优先（票 28 生效值）",
			cfg:           zhipuSignTestConfig(true),
			source:        &ZhipuSignConfig{SignFailPolicy: ZhipuSignFailPolicyClosed, SignAccountCircuitBreakThreshold: 7},
			wantPolicy:    ZhipuSignFailPolicyClosed,
			wantThreshold: 7,
		},
		{
			name:          "运行层大小写与空白被归一",
			cfg:           zhipuSignTestConfig(true),
			source:        &ZhipuSignConfig{SignFailPolicy: " Closed ", SignAccountCircuitBreakThreshold: 4},
			wantPolicy:    ZhipuSignFailPolicyClosed,
			wantThreshold: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var source zhipuSignConfigSource
			if tc.source != nil {
				source = &zhipuSignAlertConfigStub{config: *tc.source}
			}
			alerts := NewZhipuSignAlerts(tc.cfg, nil, source, func() time.Time { return zhipuSignAlertTestBase })
			ctx := context.Background()

			require.Equal(t, tc.wantPolicy, alerts.FailPolicy(ctx))
			require.Equal(t, tc.wantThreshold, alerts.CircuitBreakThreshold(ctx))
		})
	}

	t.Run("未接线引擎也给出安全默认", func(t *testing.T) {
		var alerts *ZhipuSignAlerts
		require.Equal(t, ZhipuSignFailPolicyOpen, alerts.FailPolicy(context.Background()))
		require.Equal(t, 10, alerts.CircuitBreakThreshold(context.Background()))
		require.False(t, alerts.CircuitBreakState(1).Tripped)
		require.True(t, alerts.CounterDegraded(), "未接线引擎视为指标不可信")
		alerts.RecordVerifyInvalid(context.Background(), 1) // 不得 panic
		alerts.RecordFailOpen(context.Background(), 1)
		alerts.RecordSignFailure(context.Background(), 1, errors.New("x"))
		alerts.RecordReplay(context.Background(), 1, true)
		alerts.RestoreCircuitBreak(1)
	})
}

// TestZhipuSignStatusProjectsCircuitBreakRuntimeState 覆盖验收标准第四条的下游消费：
// 管理端状态投影（票 28 的 TODO(#24) 位）必须如实反映熔断状态；未接线时报「未知」。
func TestZhipuSignStatusProjectsCircuitBreakRuntimeState(t *testing.T) {
	accounts := []Account{zhipuSignConfigTestAccount(1, "zhipu-signed-a", zhipuSignConfigTestAPIKey, "v4")}

	t.Run("已接线：熔断状态如实投影", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestGatewayConfig())
		h.source.setThreshold(2)
		h.alerts.RecordVerifyInvalid(context.Background(), 1)
		h.alerts.RecordVerifyInvalid(context.Background(), 1)

		svc := NewZhipuSignConfigService(
			zhipuSignTestGatewayConfig(),
			newZhipuSignSettingRepoStub(nil),
			&zhipuSignAccountListerStub{accounts: accounts},
			&zhipuSignRuntimeStub{},
		)
		svc.SetCircuitBreakReader(h.alerts)

		status, err := svc.Status(context.Background())
		require.NoError(t, err)
		require.Len(t, status.Accounts, 1)
		require.True(t, status.Accounts[0].RuntimeStateAvailable)
		require.True(t, status.Accounts[0].CircuitBreakTripped)
		require.Equal(t, ZhipuSignCircuitBreakReasonVerifyInvalid, status.Accounts[0].CircuitBreakReason)
	})

	t.Run("未接线：状态未知（不得显示未熔断）", func(t *testing.T) {
		svc := NewZhipuSignConfigService(
			zhipuSignTestGatewayConfig(),
			newZhipuSignSettingRepoStub(nil),
			&zhipuSignAccountListerStub{accounts: accounts},
			&zhipuSignRuntimeStub{},
		)

		status, err := svc.Status(context.Background())
		require.NoError(t, err)
		require.Len(t, status.Accounts, 1)
		require.False(t, status.Accounts[0].RuntimeStateAvailable)
		require.False(t, status.Accounts[0].CircuitBreakTripped)
		require.Empty(t, status.Accounts[0].CircuitBreakReason)
	})

	t.Run("已接线但未熔断：available=true 且 tripped=false", func(t *testing.T) {
		h := newZhipuSignAlertHarness(zhipuSignTestGatewayConfig())
		svc := NewZhipuSignConfigService(
			zhipuSignTestGatewayConfig(),
			newZhipuSignSettingRepoStub(nil),
			&zhipuSignAccountListerStub{accounts: accounts},
			&zhipuSignRuntimeStub{},
		)
		svc.SetCircuitBreakReader(h.alerts)

		status, err := svc.Status(context.Background())
		require.NoError(t, err)
		require.True(t, status.Accounts[0].RuntimeStateAvailable)
		require.False(t, status.Accounts[0].CircuitBreakTripped)
		require.Empty(t, status.Accounts[0].CircuitBreakReason)
	})
}

// TestZhipuSignAlertCircuitBreakDoesNotTouchCredentials 是不变量断言：熔断只改内存状态，
// 账号凭据（api_key / zcode_client_sign / auth_flow）逐字节不变——管理员确认版本后可恢复。
func TestZhipuSignAlertCircuitBreakDoesNotTouchCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuSignAlertTestBody()
	h := newZhipuSignAlertHarness(zhipuSignTestConfig(true))
	h.source.setThreshold(1)

	account := zhipuSignTestAccount(t, map[string]any{})
	before := map[string]any{}
	for key, value := range account.Credentials {
		before[key] = value
	}

	h.alerts.RecordVerifyInvalid(context.Background(), account.ID)
	require.True(t, h.alerts.CircuitBreakState(account.ID).Tripped)

	signer := &zhipuSignStubSigner{}
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := zhipuSignAlertService(zhipuSignTestConfig(true), signer, upstream, h.alerts)
	_, err := svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
	require.Error(t, err)

	require.Equal(t, before, account.Credentials, "熔断绝不修改账号凭据")
	require.Equal(t, zhipuSignCredentialV4, account.GetCredential(zhipuSignCredentialKey))
}
