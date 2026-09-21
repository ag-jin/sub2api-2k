package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ──────────────────────────────────────────────────────────────
// 上游流停顿守卫（idle / stall guard）
//
// 背景（生产事故，2026-09-20）：
//   WordBuddy 与 CodeBuddy 上游在实测中偶发「接住连接但不吐数据」——上游返回
//   200 且响应头正常，但后续长时间零字节。网关侧原本只有「首字节截止」守卫，
//   一旦响应头到达即停表；此后读取阶段没有任何空闲判定，于是：
//     - 客户端（WordBuddy 1,200,000ms / 面板 axios 30s / 模型测试 60s）一直等；
//     - 网关无限期挂着，既不报错也不收尾；
//     - 用户看到「点了没反应」「回答打到一半停住」。
//
// 设计取舍：
//   - 以「上游最后一次产出字节」为计时锚点，而非「最后一次写下游」——
//     上游滴流 SSE 注释心跳（`:` 开头）也算存活证据，不应被误杀；
//   - 上游心跳会被透传给客户端（raw 路径逐行透传），因此不会造成下游静默；
//   - 触发后返回可重试的 failover 错误，让请求切到健康账号，而不是把
//     挂起原样暴露给用户。
// ──────────────────────────────────────────────────────────────

// errUpstreamStreamStalled 表示上游流在空闲阈值内没有产出任何字节。
var errUpstreamStreamStalled = errors.New("upstream stream stalled: no data received before idle deadline")

// upstreamStallGuard 监控上游读取的空闲时长，超时后取消上游上下文。
//
// 用法：
//
//	guard := newUpstreamStallGuard(ctx, idle, cancel)
//	reader := guard.wrap(resp.Body)   // 每次成功读取自动刷新计时
//	defer guard.stop()
//	...
//	if guard.Fired() { /* 归因为停顿超时 */ }
type upstreamStallGuard struct {
	idle   time.Duration
	cancel context.CancelFunc

	// lastReadAt 为上游最后一次成功读取的 UnixNano；start 与 wrap 都会刷新。
	lastReadAt atomic.Int64

	stopCh  chan struct{}
	doneCh  chan struct{}
	stopped sync.Once
	fired   atomic.Bool

	// hook 是触发时的额外动作。用于"取消无法穿透"的场景：raw 直转路径的上游请求
	// 经 detachUpstreamContext 用 context.WithoutCancel 剥离了取消链（这样客户端
	// 断开仍能继续 drain 计费），于是 cancel() 到不了上游。此时改为**关闭响应体**
	// 来解除阻塞中的 Read —— net/http 明确规定 Close 会中断挂起的 Read。
	hook atomic.Value // func()
}

// newUpstreamStallGuard 创建停顿守卫并在后台按 idle/4（下限 250ms）的粒度轮询。
//
// idle <= 0 时返回 nil，表示禁用（与各配置项「0 表示禁用」的约定一致）。
// cancel 用于在判定停顿时中断上游请求；可为 nil（仅标记，不中断）。
func newUpstreamStallGuard(ctx context.Context, idle time.Duration, cancel context.CancelFunc) *upstreamStallGuard {
	if idle <= 0 {
		return nil
	}
	g := &upstreamStallGuard{
		idle:   idle,
		cancel: cancel,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	g.lastReadAt.Store(time.Now().UnixNano())

	tick := idle / 4
	if tick < 250*time.Millisecond {
		tick = 250 * time.Millisecond
	}
	go func() {
		defer close(g.doneCh)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-g.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if g.expired() {
					g.fired.Store(true)
					if h, ok := g.hook.Load().(func()); ok && h != nil {
						h()
					}
					if g.cancel != nil {
						g.cancel()
					}
					return
				}
			}
		}
	}()
	return g
}

func (g *upstreamStallGuard) expired() bool {
	if g == nil {
		return false
	}
	last := time.Unix(0, g.lastReadAt.Load())
	return time.Since(last) >= g.idle
}

// onStall 注册触发时的额外动作（见 hook 字段说明）。
// 必须在可能长时间阻塞的 Read 开始之前调用。
func (g *upstreamStallGuard) onStall(fn func()) {
	if g == nil || fn == nil {
		return
	}
	g.hook.Store(fn)
}

// touch 记录一次上游产出。
func (g *upstreamStallGuard) touch() {
	if g == nil {
		return
	}
	g.lastReadAt.Store(time.Now().UnixNano())
}

// Fired 报告是否已判定停顿并触发取消。
func (g *upstreamStallGuard) Fired() bool {
	if g == nil {
		return false
	}
	return g.fired.Load()
}

// Idle 返回当前空闲时长（供错误信息与日志使用）。
func (g *upstreamStallGuard) Idle() time.Duration {
	if g == nil {
		return 0
	}
	return time.Since(time.Unix(0, g.lastReadAt.Load()))
}

// stop 幂等停止后台轮询并等待其退出。
func (g *upstreamStallGuard) stop() {
	if g == nil {
		return
	}
	g.stopped.Do(func() { close(g.stopCh) })
	<-g.doneCh
}

// wrap 包装上游响应体：每次成功读取刷新空闲计时。
func (g *upstreamStallGuard) wrap(rc io.ReadCloser) io.ReadCloser {
	if g == nil || rc == nil {
		return rc
	}
	return &upstreamStallReadCloser{ReadCloser: rc, guard: g}
}

type upstreamStallReadCloser struct {
	io.ReadCloser
	guard *upstreamStallGuard
}

func (r *upstreamStallReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.guard.touch()
	}
	return n, err
}

// upstreamStallError 构造停顿超时的用户可见错误文案：明确说出「多久没有数据」，
// 便于面板与日志定位，而不是笼统的 "context canceled"。
func upstreamStallError(what string, idle time.Duration) error {
	return fmt.Errorf("%w (%s idle %s)", errUpstreamStreamStalled, what, idle.Round(time.Second))
}

// ──────────────────────────────────────────────────────────────
// 各调用面的空闲阈值解析
// ──────────────────────────────────────────────────────────────

// accountTestDefaultStallTimeout 是账号测试（面板「模型测试」）的兜底空闲阈值。
//
// 为什么单独给默认值：该项目所有相关配置都遵循「0 = 禁用」，而生产 config.yaml
// 没有 gateway 段，于是 gateway.* 全部取代码默认——其中
// openai_response_header_timeout 默认就是 0（无上限）。测试路径若照抄该语义，
// 等于默认无保护，正是本次事故的形态。因此这里给一个**非零兜底**，保证
// 「未配置也有保护」；运维可通过 gateway.stream_data_interval_timeout 覆盖。
const accountTestDefaultStallTimeout = 90 * time.Second

// accountTestStallTimeout 解析测试路径的空闲阈值：
// gateway.stream_data_interval_timeout > 0 时用它，否则用兜底值。
func (s *AccountTestService) accountTestStallTimeout() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		return time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	return accountTestDefaultStallTimeout
}

// streamDataIntervalTimeout 解析网关流式路径的空闲阈值
// （gateway.stream_data_interval_timeout，0 表示禁用）。
func (s *OpenAIGatewayService) streamDataIntervalTimeout() time.Duration {
	if s == nil || s.cfg == nil || s.cfg.Gateway.StreamDataIntervalTimeout <= 0 {
		return 0
	}
	return time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
}
