package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 批量签到汇总测试（A4 批 4.3）。
//
// 核心风险是**四态不串味**：成功 / 已签到（上游 10001 幂等）/ 失败 / 跳过
// 必须各自独立计数。尤其"跳过"——它代表我们**故意**不发请求（账号停调或
// 没凭据），混进"失败"会虚高失败率并掩盖真实故障。
//
// 断言同时覆盖**计数**与**是否真的发了请求**：只数计数无法区分
// "跳过了" 与 "发了请求但恰好失败"。

// countingCheckinStub 按路径统计请求次数的上游桩。
type countingCheckinStub struct {
	mu      sync.Mutex
	hits    map[string]int
	handler func(path string) (int, string)
}

func newCountingCheckinStub(handler func(path string) (int, string)) *countingCheckinStub {
	return &countingCheckinStub{hits: map[string]int{}, handler: handler}
}

func (s *countingCheckinStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	s.mu.Unlock()

	status, body := s.handler(r.URL.Path)
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *countingCheckinStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.hits {
		total += n
	}
	return total
}

// codebuddyAccount 造一个可调度的 codebuddy 账号。
func codebuddyAccount(id int64, name string) *Account {
	return &Account{
		ID:          id,
		Name:        name,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": fmt.Sprintf("at-%d", id), "uid": fmt.Sprintf("u%d", id)},
		Extra:       map[string]any{},
	}
}

// --- 必写测试 5：批量签到汇总四态各一例 ---

// Scenario：四个账号分别落入 成功 / 已签到 / 失败 / 跳过，四态计数各自独立。
//
// 用一个可区分账号的上游桩：按 uid 决定返回码。跳过账号（缺 token）
// 必须在**不发请求**的前提下计入 skipped。
func TestCodeBuddyCheckinAllSummarizesFourStates(t *testing.T) {
	// 按请求体里的 Bearer token 区分账号：at-1 成功、at-2 幂等、at-3 被拒。
	stub := newCountingCheckinStub(func(_ string) (int, string) {
		return http.StatusOK, `{"code":0,"msg":"OK","data":{"credit":100,"streak_days":1}}`
	})
	// 用 per-token 路由替代：包装成按 Authorization 头分流。
	distinguishing := &authRoutingStub{inner: stub, routing: map[string]string{
		"Bearer at-1": `{"code":0,"msg":"OK","data":{"credit":100,"streak_days":1}}`,
		"Bearer at-2": `{"code":10001,"msg":"今日已签到","data":{"credit":0,"streak_days":2}}`,
		"Bearer at-3": `{"code":12153,"msg":"session expired"}`,
	}}
	server := newTestServerWith(t, distinguishing)

	success := codebuddyAccount(1, "success")
	already := codebuddyAccount(2, "already")
	failing := codebuddyAccount(3, "failing")
	// 跳过：没有 access token（我们故意不发请求）。
	skipped := codebuddyAccount(4, "skipped")
	skipped.Credentials = map[string]any{"uid": "u4"}

	repo := newCodebuddyAdminTestRepo(success, already, failing, skipped)
	svc := NewCodeBuddyAdminService(&codebuddyAdminStubAdmin{repo: repo, nextID: 100}, repo, nil).
		WithTestBaseURL(server)

	response, err := svc.CheckinAll(context.Background())
	require.NoError(t, err)
	require.NotNil(t, response)

	require.Equal(t, 4, response.Total, "四个候选账号")
	require.Equal(t, 1, response.Succeeded, "成功态")
	require.Equal(t, 1, response.AlreadyCheckedIn, "已签到态（上游 10001 幂等成功）")
	require.Equal(t, 1, response.Failed, "失败态")
	require.Equal(t, 1, response.Skipped, "跳过态")

	// 四态互斥且完备。
	require.Equal(t,
		response.Total,
		response.Succeeded+response.AlreadyCheckedIn+response.Failed+response.Skipped,
		"四态之和等于总数（互斥且完备）")

	// 明细：失败 1 条、跳过 1 条，且跳过原因可读。
	require.Len(t, response.Errors, 1)
	require.Equal(t, int64(3), response.Errors[0].AccountID)
	require.NotEmpty(t, response.Errors[0].Message)
	require.Len(t, response.SkippedNotes, 1)
	require.Equal(t, int64(4), response.SkippedNotes[0].AccountID)
	require.Contains(t, response.SkippedNotes[0].Message, "access token",
		"跳过原因应说明缺凭据")

	// 关键：跳过账号**没有**发出上游请求（3 个可签账号各一次）。
	require.Equal(t, 3, distinguishing.hitCount(),
		"跳过账号不得发出上游请求")
}

// Scenario：跳过是"我们不发的"，不是"签到出错"——它**不得**计入 Failed。
// 这条单独钉一遍，因为混在一起会让失败率虚高、掩盖真实故障。
func TestCodeBuddyCheckinAllSkipIsNotFailure(t *testing.T) {
	stub := newCountingCheckinStub(func(_ string) (int, string) {
		return http.StatusOK, `{"code":0,"data":{"credit":1,"streak_days":1}}`
	})
	server := newTestServerWith(t, stub)

	// 停调账号（IsSchedulable=false）→ 应跳过而非失败。
	paused := codebuddyAccount(11, "paused")
	paused.Schedulable = false

	// 缺 token → 跳过。
	noToken := codebuddyAccount(12, "no-token")
	noToken.Credentials = map[string]any{"uid": "u12"}

	repo := newCodebuddyAdminTestRepo(paused, noToken)
	svc := NewCodeBuddyAdminService(&codebuddyAdminStubAdmin{repo: repo, nextID: 100}, repo, nil).
		WithTestBaseURL(server)

	response, err := svc.CheckinAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, response.Skipped)
	require.Zero(t, response.Failed, "跳过不得计入失败")
	require.Zero(t, response.Succeeded)
	require.Zero(t, stub.hitCount(), "全部跳过时不得发出任何上游请求")
}

// Scenario：无候选账号时返回零值汇总而不是 nil（调用方不必判空）。
func TestCodeBuddyCheckinAllEmptyReturnsZeroSummary(t *testing.T) {
	stub := newCountingCheckinStub(func(_ string) (int, string) { return 0, `{"code":0}` })
	server := newTestServerWith(t, stub)

	repo := newCodebuddyAdminTestRepo()
	svc := NewCodeBuddyAdminService(&codebuddyAdminStubAdmin{repo: repo, nextID: 100}, repo, nil).
		WithTestBaseURL(server)

	response, err := svc.CheckinAll(context.Background())
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Zero(t, response.Total)
	require.Zero(t, stub.hitCount())
}

// Scenario：多账号成功时 credit 累加（汇总给管理员看的"本次共得多少分"）。
func TestCodeBuddyCheckinAllAccumulatesCredit(t *testing.T) {
	stub := newCountingCheckinStub(func(_ string) (int, string) {
		return http.StatusOK, `{"code":0,"data":{"credit":25,"streak_days":1}}`
	})
	server := newTestServerWith(t, stub)

	accounts := []*Account{
		codebuddyAccount(21, "a"),
		codebuddyAccount(22, "b"),
		codebuddyAccount(23, "c"),
	}
	repo := newCodebuddyAdminTestRepo(accounts...)
	svc := NewCodeBuddyAdminService(&codebuddyAdminStubAdmin{repo: repo, nextID: 100}, repo, nil).
		WithTestBaseURL(server)

	response, err := svc.CheckinAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, response.Succeeded)
	require.InDelta(t, 75, response.CreditEarned, 1e-9, "3 × 25")
}

// Scenario：并发上限生效——并发执行但不超过配置的 5。
// 用上游桩记录同时在飞的最大请求数来证明，而不是读源码里的常量。
func TestCodeBuddyCheckinAllRespectsConcurrencyLimit(t *testing.T) {
	var inFlight, maxInFlight int64
	var mu sync.Mutex

	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := atomic.AddInt64(&inFlight, 1)
		mu.Lock()
		if current > maxInFlight {
			maxInFlight = current
		}
		mu.Unlock()
		// 留出重叠窗口，否则并发上限观察不到。
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		_, _ = io.WriteString(w, `{"code":0,"data":{"credit":10,"streak_days":1}}`)
	})
	server := newTestServerWith(t, stub)

	accounts := make([]*Account, 0, 20)
	for i := int64(0); i < 20; i++ {
		accounts = append(accounts, codebuddyAccount(100+i, fmt.Sprintf("acc-%d", i)))
	}
	repo := newCodebuddyAdminTestRepo(accounts...)
	svc := NewCodeBuddyAdminService(&codebuddyAdminStubAdmin{repo: repo, nextID: 100}, repo, nil).
		WithTestBaseURL(server)

	response, err := svc.CheckinAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, 20, response.Succeeded)

	mu.Lock()
	observed := maxInFlight
	mu.Unlock()
	require.LessOrEqual(t, observed, int64(codeBuddyCheckinBatchConcurrency),
		"并发不得超过上限 %d（实测峰值 %d）", codeBuddyCheckinBatchConcurrency, observed)
	require.Greater(t, observed, int64(1),
		"应并发执行（峰值 > 1），而不是退化成串行")
}

// Scenario：非 codebuddy 账号不出现在候选里（批量签到只服务本平台）。
func TestListCodeBuddyCheckinCandidatesPlatformScoped(t *testing.T) {
	stub := newCountingCheckinStub(func(_ string) (int, string) { return 0, `{"code":0}` })
	server := newTestServerWith(t, stub)

	anthropicAccount := &Account{
		ID: 31, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "anthropic-at"},
	}
	cbAccount := codebuddyAccount(32, "cb")
	repo := newCodebuddyAdminTestRepo(anthropicAccount, cbAccount)
	svc := NewCodeBuddyAdminService(&codebuddyAdminStubAdmin{repo: repo, nextID: 100}, repo, nil).
		WithTestBaseURL(server)

	candidates, err := svc.ListCodeBuddyCheckinCandidates(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, int64(32), candidates[0].AccountID)
}

// authRoutingStub 按 Authorization 头选择响应体，用于"每个账号不同结局"的用例。
type authRoutingStub struct {
	inner   *countingCheckinStub
	routing map[string]string
	mu      sync.Mutex
	hits    int
}

func (s *authRoutingStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits++
	s.mu.Unlock()

	if body, ok := s.routing[r.Header.Get("Authorization")]; ok {
		_, _ = io.WriteString(w, body)
		return
	}
	// 未登记的凭据：按成功处理（不影响四态计数用例的意图）。
	_, _ = io.WriteString(w, `{"code":0,"data":{"credit":0,"streak_days":0}}`)
}

func (s *authRoutingStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// newTestServerWith 起一个 httptest 服务并登记清理，返回 base URL。
func newTestServerWith(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimRight(srv.URL, "/")
}
