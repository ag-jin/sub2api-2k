package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

const (
	openAIFirstOutputStageMemoryLimit        = 64 * 1024
	openAIFirstOutputStageMaxBytes           = 8 * 1024 * 1024
	openAIFirstOutputScannerFramingAllowance = 64
	openAIFirstOutputGuardQueueSize          = 1
	openAIDefaultStreamQueueSize             = 16
)

var (
	errOpenAIFirstOutputStageLimit   = errors.New("openai first-output staging limit exceeded")
	errOpenAIFirstOutputScannerLimit = errors.New("openai pre-output scanner token limit exceeded")
)

type openAIFirstOutputStage struct {
	limit      int64
	size       int64
	memory     bytes.Buffer
	tempFile   *os.File
	tempPath   string
	createTemp func() (*os.File, error)
	removeFile func(string) error
	memoryOnly bool
	cleanupErr error
	closed     bool
}

func newOpenAIFirstOutputStage(limit int64) *openAIFirstOutputStage {
	if limit < 1 {
		limit = 1
	}
	return &openAIFirstOutputStage{
		limit:      limit,
		createTemp: func() (*os.File, error) { return os.CreateTemp("", "sub2api-openai-first-output-*") },
		removeFile: os.Remove,
		memoryOnly: runtime.GOOS == "windows",
	}
}

func newDefaultOpenAIFirstOutputStage() *openAIFirstOutputStage {
	return newOpenAIFirstOutputStage(openAIFirstOutputStageMaxBytes)
}

func openAIFirstOutputEventQueueSize(guardFirstOutput bool) int {
	if guardFirstOutput {
		return openAIFirstOutputGuardQueueSize
	}
	return openAIDefaultStreamQueueSize
}

func openAIFirstOutputDynamicScanLines(guardActive *atomic.Bool) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		if err != nil || guardActive == nil || !guardActive.Load() {
			return advance, token, err
		}
		limit := openAIFirstOutputStageMaxBytes + openAIFirstOutputScannerFramingAllowance
		if token != nil {
			if len(token) > limit {
				return 0, nil, errOpenAIFirstOutputScannerLimit
			}
			return advance, token, nil
		}
		// At the limit with no delimiter, another byte would necessarily exceed
		// the guarded token budget. Fail before Scanner grows toward MaxLineSize.
		if len(data) >= limit {
			return 0, nil, errOpenAIFirstOutputScannerLimit
		}
		return advance, token, nil
	}
}

func (s *openAIFirstOutputStage) Buffered() int64 {
	if s == nil {
		return 0
	}
	return s.size
}

func (s *openAIFirstOutputStage) WriteString(value string) (int, error) {
	if err := s.prepareWrite(len(value)); err != nil {
		return 0, err
	}
	var n int
	var err error
	if s.tempFile == nil {
		n, err = s.memory.WriteString(value)
	} else {
		n, err = io.WriteString(s.tempFile, value)
	}
	s.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write first-output stage: %w", err)
	}
	return n, nil
}

func (s *openAIFirstOutputStage) Write(p []byte) (int, error) {
	if err := s.prepareWrite(len(p)); err != nil {
		return 0, err
	}
	var n int
	var err error
	if s.tempFile == nil {
		n, err = s.memory.Write(p)
	} else {
		n, err = s.tempFile.Write(p)
	}
	s.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write first-output stage: %w", err)
	}
	return n, nil
}

func (s *openAIFirstOutputStage) prepareWrite(incoming int) error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if int64(incoming) > s.limit-s.size {
		return fmt.Errorf("%w: buffered=%d incoming=%d limit=%d", errOpenAIFirstOutputStageLimit, s.size, incoming, s.limit)
	}
	if s.tempFile != nil || s.memoryOnly || s.size+int64(incoming) <= openAIFirstOutputStageMemoryLimit {
		return nil
	}
	file, err := s.createTemp()
	if err != nil {
		return fmt.Errorf("create first-output spool: %w", err)
	}
	path := file.Name()
	// Unlink before writing any request data. Unix keeps the file descriptor
	// readable, while crashes and SIGKILL cannot leave a named plaintext spool.
	if unlinkErr := s.removeFile(path); unlinkErr != nil {
		closeErr := file.Close()
		removeErr := s.removeFile(path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		s.memoryOnly = true
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.tempPath = path
		}
		s.cleanupErr = errors.Join(
			s.cleanupErr,
			fmt.Errorf("unlink first-output spool before use: %w", unlinkErr),
			closeErr,
			removeErr,
		)
		return nil
	}
	if _, err := file.Write(s.memory.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("initialize first-output spool: %w", err)
	}
	s.tempFile = file
	s.tempPath = path
	s.memory.Reset()
	return nil
}

func (s *openAIFirstOutputStage) CommitTo(dst io.Writer) error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if s.tempFile == nil {
		if _, err := io.Copy(dst, bytes.NewReader(s.memory.Bytes())); err != nil {
			return err
		}
	} else {
		if _, err := s.tempFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek first-output spool: %w", err)
		}
		if _, err := io.CopyN(dst, s.tempFile, s.size); err != nil {
			return err
		}
	}
	if err := s.Close(); err != nil {
		// Delivery succeeded. Preserve cleanup failures for the handler's deferred
		// cleanup/logging pass instead of turning committed bytes into a stream error.
		s.cleanupErr = errors.Join(s.cleanupErr, err)
	}
	return nil
}

func (s *openAIFirstOutputStage) Close() error {
	if s == nil {
		return nil
	}
	if s.closed && s.tempFile == nil && s.tempPath == "" && s.cleanupErr == nil {
		return nil
	}
	s.closed = true
	s.size = 0
	s.memory.Reset()
	closeErr := s.cleanupErr
	s.cleanupErr = nil
	if s.tempFile != nil {
		closeErr = errors.Join(closeErr, s.tempFile.Close())
		s.tempFile = nil
	}
	if s.tempPath != "" {
		removeErr := s.removeFile(s.tempPath)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			s.tempPath = ""
		} else {
			closeErr = errors.Join(closeErr, removeErr)
		}
	}
	return closeErr
}

func (s *OpenAIGatewayService) openAIFirstOutputTimeout(reasoningEffort string) time.Duration {
	if s == nil || s.cfg == nil || s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds <= 0 {
		return 0
	}
	seconds := s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds
	switch strings.ToLower(strings.TrimSpace(reasoningEffort)) {
	case "high", "xhigh", "max":
		if override := s.cfg.Gateway.OpenAIHighEffortFirstOutputTimeoutSeconds; override > 0 {
			seconds = override
		}
	}
	return time.Duration(seconds) * time.Second
}

// chatCompletionsFirstTokenTimeout 返回 /v1/chat/completions 流式上游首个数据块的
// 截止时限；0 表示禁用。独立于 Responses 路径的 OpenAIFirstOutputTimeoutSeconds，
// 面向 CC 直转/转换链路的上游排队挂起（HTTP 200 后长时间不产出首个 data 帧）。
func (s *OpenAIGatewayService) chatCompletionsFirstTokenTimeout() time.Duration {
	if s == nil || s.cfg == nil || s.cfg.Gateway.ChatCompletionsFirstTokenTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(s.cfg.Gateway.ChatCompletionsFirstTokenTimeoutSeconds) * time.Second
}

// newOpenAIChatFirstTokenTimeoutError 构造 CC 流式首 token 超时的 failover 错误。
// SafeToFailoverAfterWrite：超时期间下游至多收到 SSE 注释 keepalive，没有语义输出，
// 可在同一客户端流内切换账号重放。
func (s *OpenAIGatewayService) newOpenAIChatFirstTokenTimeoutError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	model string,
	upstreamRequestID string,
	elapsed time.Duration,
) *UpstreamFailoverError {
	timeout := s.chatCompletionsFirstTokenTimeout()
	logger.LegacyPrintf(
		"service.openai_gateway",
		"Chat Completions first token timeout: account=%d model=%s elapsed=%s limit=%s",
		account.ID, model, elapsed, timeout,
	)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusGatewayTimeout,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "first_token_timeout",
		Message:            "Upstream produced no first token before the deadline",
		Detail:             fmt.Sprintf("elapsed_ms=%d timeout_ms=%d", elapsed.Milliseconds(), timeout.Milliseconds()),
	})
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTimeout(ctx, account, model)
	}
	return &UpstreamFailoverError{
		StatusCode: http.StatusGatewayTimeout,
		ResponseBody: []byte(
			`{"error":{"type":"first_token_timeout","message":"Upstream produced no first token before the deadline"}}`,
		),
		SafeToFailoverAfterWrite: true,
	}
}

func (s *OpenAIGatewayService) newOpenAIFirstOutputTimeoutError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	originalModel string,
	reasoningEffort string,
	timeout time.Duration,
	phase string,
	responseHeaders http.Header,
) *UpstreamFailoverError {
	elapsed := time.Since(startTime)
	logger.LegacyPrintf(
		"service.openai_gateway",
		"OpenAI first output timeout: account=%d model=%s effort=%s phase=%s elapsed=%s limit=%s",
		account.ID, originalModel, reasoningEffort, phase, elapsed, timeout,
	)
	requestID := strings.TrimSpace(responseHeaders.Get("x-request-id"))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: http.StatusGatewayTimeout, UpstreamRequestID: requestID,
		Kind: "first_output_timeout", Message: "OpenAI upstream produced no semantic output before the deadline",
		Detail: fmt.Sprintf("phase=%s elapsed_ms=%d timeout_ms=%d", phase, elapsed.Milliseconds(), timeout.Milliseconds()),
	})
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTimeout(ctx, account, originalModel)
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusGatewayTimeout,
		ResponseBody:    []byte(`{"error":{"type":"first_output_timeout","message":"Upstream produced no output before the deadline"}}`),
		ResponseHeaders: responseHeaders.Clone(), SafeToFailoverAfterWrite: true,
	}
}

type openAIFirstOutputHeaderGuard struct {
	cancel  context.CancelFunc
	release context.CancelFunc
	timer   *time.Timer
	fired   chan struct{}
	once    sync.Once
}

func newOpenAIFirstOutputHeaderGuard(
	ctx context.Context,
	release context.CancelFunc,
	deadline time.Time,
) (context.Context, *openAIFirstOutputHeaderGuard) {
	guardedCtx, cancel := context.WithCancel(ctx)
	guard := &openAIFirstOutputHeaderGuard{cancel: cancel, release: release, fired: make(chan struct{})}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	guard.timer = time.AfterFunc(remaining, func() {
		close(guard.fired)
		cancel()
	})
	return guardedCtx, guard
}

func (g *openAIFirstOutputHeaderGuard) stopHeaderWait() bool {
	if g == nil {
		return false
	}
	if g.timer.Stop() {
		return false
	}
	<-g.fired
	return true
}

// Fired 报告守卫截止是否已触发（触发即守卫派生的上游 context 已被取消）。
func (g *openAIFirstOutputHeaderGuard) Fired() bool {
	if g == nil {
		return false
	}
	select {
	case <-g.fired:
		return true
	default:
		return false
	}
}

// fire 在 close 之后人为恢复 fired 标记：守卫因截止触发而取消请求时，
// 传输错误路径会先 close 释放资源，调用方仍需 Fired() 归因首 token 超时。
func (g *openAIFirstOutputHeaderGuard) fire() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		close(g.fired)
	})
}

func (g *openAIFirstOutputHeaderGuard) close() {
	g.once.Do(func() {
		g.timer.Stop()
		g.cancel()
		g.release()
	})
}

type openAIRequestContextReadCloser struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
	err     error
}

func (r *openAIRequestContextReadCloser) Close() error {
	r.once.Do(func() {
		r.cleanup()
		r.err = r.ReadCloser.Close()
	})
	return r.err
}
