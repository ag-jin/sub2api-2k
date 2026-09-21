//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/stretchr/testify/require"
)

// stallBody 前 delay 静默、之后才给出 payload；用于模拟「上游接住连接但不吐数据」。
type stallBody struct {
	delay   time.Duration
	payload string
	done    bool
}

func (b *stallBody) Read(p []byte) (int, error) {
	if !b.done {
		time.Sleep(b.delay)
		b.done = true
	}
	if b.payload == "" {
		return 0, io.EOF
	}
	n := copy(p, b.payload)
	b.payload = b.payload[n:]
	return n, nil
}
func (b *stallBody) Close() error { return nil }

// TestUpstreamStallGuard_FiresOnSilentUpstream 锁定核心行为：
// 上游长时间零字节时，守卫必须在阈值附近触发并取消上游，而不是无限等待。
func TestUpstreamStallGuard_FiresOnSilentUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const idle = 600 * time.Millisecond
	guard := newUpstreamStallGuard(ctx, idle, cancel)
	require.NotNil(t, guard)
	defer guard.stop()

	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("停顿守卫未在阈值内取消上游：上游零字节时请求会无限挂着")
	}
	elapsed := time.Since(start)
	require.True(t, guard.Fired(), "应标记为已触发")
	require.GreaterOrEqual(t, elapsed, idle, "不应早于阈值触发")
	require.Less(t, elapsed, idle+2*time.Second, "应在阈值附近触发，而非拖延")
}

// TestUpstreamStallGuard_TrickleKeepsItAlive 锁定反向行为：
// 上游持续滴流（含 SSE 注释心跳）时守卫不得误杀——这正是原 180s 守卫失效的
// 反面：锚点必须跟随真实产出，而不是被伪造的「有数据」骗过。
func TestUpstreamStallGuard_TrickleKeepsItAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const idle = 700 * time.Millisecond
	guard := newUpstreamStallGuard(ctx, idle, cancel)
	defer guard.stop()

	// 以远快于 idle 的节奏喂数据，持续超过 idle 总时长。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 12; i++ {
			time.Sleep(idle / 4)
			guard.touch()
		}
	}()
	<-done

	require.False(t, guard.Fired(), "上游持续有产出时不得判定停顿")
	require.NoError(t, ctx.Err(), "不应取消上游")
}

// TestUpstreamStallGuard_DisabledWhenZero 锁定「0 = 禁用」约定。
func TestUpstreamStallGuard_DisabledWhenZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.Nil(t, newUpstreamStallGuard(ctx, 0, cancel))
}

// TestUpstreamStallGuard_WrapTouchesTimer 锁定 wrap() 会把真实读取计入存活。
func TestUpstreamStallGuard_WrapTouchesTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const idle = 500 * time.Millisecond
	guard := newUpstreamStallGuard(ctx, idle, cancel)
	defer guard.stop()

	body := guard.wrap(io.NopCloser(strings.NewReader("data: {\"x\":1}\n\n")))
	idleBefore := guard.Idle()
	time.Sleep(150 * time.Millisecond)

	buf := make([]byte, 64)
	n, err := body.Read(buf)
	require.NoError(t, err)
	require.Positive(t, n)
	require.Less(t, guard.Idle(), idleBefore+150*time.Millisecond,
		"成功读取后空闲计时应被刷新")
}

// TestUpstreamStallError_MentionsIdle 锁定错误文案说清「多久没有数据」，
// 而不是笼统的 context canceled——这是用户能看到「为什么没反应」的关键。
func TestUpstreamStallError_MentionsIdle(t *testing.T) {
	err := upstreamStallError("chat.completions", 90*time.Second)
	require.True(t, errors.Is(err, errUpstreamStreamStalled))
	require.Contains(t, err.Error(), "1m30s")
}

// TestAccountTestStallTimeout_DefaultsToNonZero 锁定「未配置也有保护」。
//
// 生产 config.yaml 没有 gateway 段，所有 gateway.* 取代码默认，其中
// openai_response_header_timeout 默认为 0（无上限）。测试路径若照抄「0=禁用」
// 语义就等于默认无保护——这正是本次事故。故测试路径必须有非零兜底。
func TestAccountTestStallTimeout_DefaultsToNonZero(t *testing.T) {
	// 无 cfg（等价于配置缺失）
	require.Equal(t, accountTestDefaultStallTimeout, (&AccountTestService{}).accountTestStallTimeout())
	require.Positive(t, accountTestDefaultStallTimeout)

	// 未设置该项时同样用兜底
	require.Equal(t, accountTestDefaultStallTimeout,
		(&AccountTestService{cfg: &config.Config{}}).accountTestStallTimeout())

	// 显式配置时以配置为准（运维可覆盖）
	cfg := &config.Config{}
	cfg.Gateway.StreamDataIntervalTimeout = 45
	require.Equal(t, 45*time.Second, (&AccountTestService{cfg: cfg}).accountTestStallTimeout())
}

// TestUpstreamStallGuard_CoversHeaderWaitPhase 锁定顺序要求：
// 守卫必须在发起上游请求**之前**就绪，否则「等响应头」阶段无保护——
// 生产卡住的正是这个阶段。
func TestUpstreamStallGuard_CoversHeaderWaitPhase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const idle = 500 * time.Millisecond
	// 模拟调用方：先建守卫并派生 ctx，再拿它发请求（等响应头阶段）。
	stallCtx, stallCancel := context.WithCancel(ctx)
	defer stallCancel()
	guard := newUpstreamStallGuard(stallCtx, idle, stallCancel)
	defer guard.stop()

	start := time.Now()
	select {
	case <-stallCtx.Done():
		require.True(t, guard.Fired(), "等响应头阶段超时应标记为守卫触发")
		require.GreaterOrEqual(t, time.Since(start), idle)
		require.Less(t, time.Since(start), idle+2*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("等响应头阶段未被守卫覆盖：上游不返回响应头时会无限挂着")
	}
}

// TestUpstreamStallGuard_OnStallHookUnblocksDetachedRead 锁定关键机制：
// raw 直转路径的上游请求经 detachUpstreamContext 剥离了取消链（客户端断开后仍要
// drain 计费），因此守卫的 cancel() **到不了上游**。必须靠 onStall 钩子关闭响应体
// 来解除阻塞中的 Read——否则守卫"触发了"却无法让请求真正结束（生产实证：
// 隔离实例上上游永久静默时，请求挂满 90s 观测窗仍未收尾）。
func TestUpstreamStallGuard_OnStallHookUnblocksDetachedRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const idle = 500 * time.Millisecond
	guard := newUpstreamStallGuard(ctx, idle, cancel)
	defer guard.stop()

	// 模拟阻塞的响应体：Close 才能解除阻塞（模拟 net/http 语义）。
	unblocked := make(chan struct{})
	body := &blockingReadCloser{unblock: unblocked}
	guard.onStall(func() { _ = body.Close() })

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8)
		_, _ = guard.wrap(body).Read(buf) // 会一直阻塞，直到 Close
	}()

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("停顿守卫未能解除阻塞中的 Read：onStall 钩子没有生效")
	}
	require.True(t, guard.Fired())
}

// blockingReadCloser 的 Read 永久阻塞，直到 Close 被调用。
type blockingReadCloser struct {
	unblock chan struct{}
	once    sync.Once
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	<-b.unblock
	return 0, io.EOF
}

func (b *blockingReadCloser) Close() error {
	b.once.Do(func() { close(b.unblock) })
	return nil
}

// ── 回归：注释心跳不得"续命"守卫 ────────────────────────────────────────────
//
// 生产实测（本次事故取证）：上游在长期静默期内会只滴流 SSE 注释行
// （捕获到的真实帧是 `: heartbeat`），而客户端在该时段**收不到任何语义输出**。
// 若守卫按"任意字节"计时，这些注释就能无限续命 → 守卫永不触发 → 客户端无限干等。
//
// 历史：Responses 路径的同形缺陷已修（锚点改为语义事件）；
// raw 路径的守卫一度仍按字节计时（`touch()` 在 n>0 时无条件调用），
// 由本组测试锁定为"必须按语义行计时"。

// commentHeartbeatOnlyBody 定期产出 SSE 注释行、**永不产出 data 帧**。
type commentHeartbeatOnlyBody struct {
	interval time.Duration
	closed   chan struct{}
	once     sync.Once
}

func (b *commentHeartbeatOnlyBody) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.EOF
	case <-time.After(b.interval):
	}
	return copy(p, ": heartbeat\n\n"), nil
}

func (b *commentHeartbeatOnlyBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestUpstreamStallGuard_CommentHeartbeatDoesNotRefreshIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idle := 400 * time.Millisecond
	g := newUpstreamStallGuard(ctx, idle, cancel)
	require.NotNil(t, g)
	defer g.stop()

	body := &commentHeartbeatOnlyBody{interval: 60 * time.Millisecond, closed: make(chan struct{})}
	defer body.Close()
	rc := g.wrap(body)

	buf := make([]byte, 256)
	// 持续读：注释心跳会不断到达，但守卫必须仍然判定停顿。
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := rc.Read(buf); err != nil {
			break
		}
		if g.Fired() {
			return // 期望路径：守卫触发
		}
	}
	t.Fatalf("上游只发注释心跳（: heartbeat）时守卫未触发——"+
		"注释不得续命空闲计时；Idle=%v Fired=%v", g.Idle(), g.Fired())
}

func TestUpstreamStallGuard_DataLineRefreshesIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := newUpstreamStallGuard(ctx, 500*time.Millisecond, cancel)
	require.NotNil(t, g)
	defer g.stop()

	for i := 0; i < 5; i++ {
		rc := g.wrap(io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")))
		buf := make([]byte, 256)
		_, _ = rc.Read(buf)
		require.False(t, g.Fired(), "data 帧必须续命守卫（第 %d 次）", i+1)
		time.Sleep(200 * time.Millisecond)
	}
}

func TestContainsSemanticSSELine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"注释心跳", ": heartbeat\n\n", false},
		{"裸冒号", ":\n\n", false},
		{"data 帧", "data: {\"a\":1}\n\n", false /* 见下方修正 */},
		{"data 完整行", "data: {\"a\":1}", true},
		{"data 无空格", "data:{\"a\":1}", true},
		{"DONE", "data: [DONE]", true},
		{"event 行", "event: message", false},
		{"空行", "\n", false},
		{"CRLF data", "data: {\"a\":1}\r\n", true},
		{"注释夹 data", ": ping\ndata: {\"a\":1}\n\n", true},
		{"多行含注释", ": hb\n\n: hb\n\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "data 帧" {
				// "data: {...}\n\n" 含一行 data: 前缀 → 视为语义行
				require.True(t, containsSemanticSSELine([]byte(c.in)))
				return
			}
			require.Equal(t, c.want, containsSemanticSSELine([]byte(c.in)))
		})
	}
}
