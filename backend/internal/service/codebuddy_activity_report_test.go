package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/stretchr/testify/require"
)

// A5 批 5.1：上报通道的上游调用契约。
//
// 这些用例覆盖的是"发出去的到底是什么"——形状、路径、请求头、条数与失败短路。
// 事件体的字段级断言在 platform/codebuddy/activity_report_test.go。

// activityRecorder 记录到达的请求（body 原文 + 头），供断言。
type activityRecorder struct {
	mu       sync.Mutex
	requests []recordedActivityRequest
}

type recordedActivityRequest struct {
	Path          string
	Authorization string
	UserAgent     string
	Origin        string
	Body          []byte
}

func (r *activityRecorder) record(req *http.Request, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedActivityRequest{
		Path:          req.URL.Path,
		Authorization: req.Header.Get("Authorization"),
		UserAgent:     req.Header.Get("User-Agent"),
		Origin:        req.Header.Get("Origin"),
		Body:          body,
	})
}

func (r *activityRecorder) snapshot() []recordedActivityRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedActivityRequest(nil), r.requests...)
}

func newCodeBuddyActivityAccount(id int64, uid, token string) *Account {
	return &Account{
		ID:       id,
		Name:     "cb-account",
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"uid":          uid,
			"access_token": token,
		},
	}
}

// disableActivityGap 把条间隔压到 0，避免测试真等 5×1.5s。
func disableActivityGap(t *testing.T) {
	t.Helper()
	original := codeBuddyActivitySleep
	codeBuddyActivitySleep = func(ctx context.Context, d time.Duration) error {
		return ctx.Err()
	}
	t.Cleanup(func() { codeBuddyActivitySleep = original })
}

// 未配置/无 uid 的账号一律不发（坑 1：发了也是被静默丢弃）。
func TestReportCodeBuddyActivitySkipsAccountWithoutUID(t *testing.T) {
	recorder := &activityRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r, nil)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	account := newCodeBuddyActivityAccount(1, "", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	// 三条都断言：少任何一条，删掉这个前置检查都不会变红（变异测试实测过）——
	// 去掉该检查后事件仍会被发出去（userId 空），只是最终失败在别处。
	require.ErrorIs(t, result.Err, errCodeBuddyActivityMissingUID)
	require.Equal(t, 0, result.Reported)
	require.Empty(t, recorder.snapshot(), "缺 uid 时不应发出任何请求")
	require.False(t, result.Verified, "缺 uid 时不该走到自检")
}

func TestReportCodeBuddyActivitySkipsAccountWithoutToken(t *testing.T) {
	recorder := &activityRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r, nil)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	account := newCodeBuddyActivityAccount(1, "u-1", "")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.ErrorIs(t, result.Err, errCodeBuddyActivityMissingToken)
	require.Empty(t, recorder.snapshot())
}

// 正常一轮：5 条、同 conversationId、requestId 各异、路径与鉴权头正确。
func TestReportCodeBuddyActivitySendsFiveEventsInOneConversation(t *testing.T) {
	recorder := &activityRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			// 自检回读：返回非零 streak，避免被判成"静默丢弃"。
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":7}}}`))
			return
		}
		// 读**原文**（不重新序列化）：这样断言的就是真正发出去的字节。
		raw, _ := io.ReadAll(r.Body)
		recorder.record(r, raw)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	disableActivityGap(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.NoError(t, result.Err)
	require.Equal(t, 5, result.Reported)

	requests := recorder.snapshot()
	require.Len(t, requests, 5)

	conversationIDs := map[string]bool{}
	requestIDs := map[string]bool{}
	for _, req := range requests {
		// 路径固定 /v2/report，且**不带** /v2/billing/meter 回落族。
		require.Equal(t, codebuddy.CodeBuddyActivityReportPath, req.Path)
		require.Equal(t, "Bearer token-1", req.Authorization)
		require.Contains(t, req.UserAgent, "CodeBuddy/")

		var events []map[string]any
		require.NoError(t, json.Unmarshal(req.Body, &events), "body 必须是 JSON 数组")
		require.Len(t, events, 1)
		event := events[0]

		// 坑 1 + 坑 3：userId 在场；同会话共用 conversationId；requestId 各异。
		require.Equal(t, "u-1", event["userId"])
		require.Equal(t, "chat_request_send", event["eventCode"])
		conversationIDs[event["conversationId"].(string)] = true
		requestIDs[event["requestId"].(string)] = true
	}
	require.Len(t, conversationIDs, 1, "N 条必须共用同一 conversationId")
	require.Len(t, requestIDs, 5, "requestId 必须各条独立")
}

// 任一条失败即停止该号后续条数（不续发）。
func TestReportCodeBuddyActivityStopsOnFirstFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
			return
		}
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current >= 2 {
			// ⚠️ 必须是**带引号的字符串**码，且 msg 不含"拒绝/失效"等词——否则
			// 会命中别的分支，测不到"业务码非零即失败"这条主路径。
			// （另注：Go 裸字符串里写 `11-128` 会被 json 解析成数字 -117，
			//  那是 A1 的 L2 坑，这里刻意用字符串形态。）
			_, _ = w.Write([]byte(`{"code":"99999","msg":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	disableActivityGap(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.Error(t, result.Err, "第 2 条失败必须让整体失败")
	require.Equal(t, 1, result.Reported, "失败后不再续发")
	mu.Lock()
	require.Equal(t, 2, attempts, "失败后不应再有第 3 次尝试")
	mu.Unlock()
}

// L3 保护：字符串形态的业务码必须被当作**失败**，而不是被 gjson .Int()
// 读成 0（成功）。{"code":"11-128"} 在上游是真实存在的形态。
func TestReportCodeBuddyActivityTreatsStringBizCodeAsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
			return
		}
		// 上游以字符串承载业务码（真实形态）。
		_, _ = w.Write([]byte(`{"code":"11-128","msg":"Illegal API invocation"}`))
	}))
	defer server.Close()
	disableActivityGap(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.Error(t, result.Err, "字符串业务码不可被读成成功")
	require.Equal(t, 0, result.Reported)
}

// 自检（坑 1 的对策）：上报 200 但 streak.days=0 → 标为可疑，不得静默当成功。
func TestReportCodeBuddyActivityFlagsSilentDropOnZeroStreak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			// 上报"成功"但连登天数没动 —— 缺 userId 的典型症状。
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":0}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	disableActivityGap(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.NoError(t, result.Err, "上报本身 HTTP/信封都成功")
	require.Equal(t, 5, result.Reported)
	require.True(t, result.Verified, "自检已执行")
	require.True(t, result.SelfCheckFailed, "days=0 必须标记为可疑，不能静默当成功")
}

// 自检成功路径：days>0 → 无异常。
func TestReportCodeBuddyActivityVerifiesHealthyStreak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	disableActivityGap(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.NoError(t, result.Err)
	require.Equal(t, 3, result.StreakDays)
	require.True(t, result.Verified)
	require.False(t, result.SelfCheckFailed)
}

// --- 5.3：域分离（上报走 billing、自检走 chat）---

// Scenario：上报与自检落在**两个不同域**上。
//
// ⚠️ 为什么必须单独测这个：两个端点在实现上只差一个"用哪个 base"的参数，
// 而测试若只用单一 testBaseURL，两条请求会打到同一个 httptest server——
// **把自检错写成 billing 域，所有用例照样全绿**（已用变异实测确认）。
// 这里绕开 testBaseURL，直接断言两个纯函数解析出的 host。
func TestCodeBuddyActivityEndpointsUseDifferentDomains(t *testing.T) {
	svc := NewCodeBuddyAdminService(nil, nil, nil)

	cn := newCodeBuddyActivityAccount(1, "u-1", "t-1")
	report := svc.codeBuddyActivityReportEndpoint(cn)
	streak := svc.codeBuddyActivityStreakEndpoint(cn)

	require.Equal(t, CodeBuddyBillingBaseCN+codebuddy.CodeBuddyActivityReportPath, report,
		"上报必须走 billing 域（签到/积分同域）")
	require.Equal(t, CodeBuddyChatBaseCN+CodeBuddyActivityStreakPath, streak,
		"自检必须走 chat 域（与上报**不同**主机）")

	// 两条端点的 host 必须不同——这是本用例的核心断言。
	reportHost := strings.Split(strings.TrimPrefix(report, "https://"), "/")[0]
	streakHost := strings.Split(strings.TrimPrefix(streak, "https://"), "/")[0]
	require.NotEqual(t, reportHost, streakHost,
		"上报与自检不得落在同一主机（CN 下 billing=www.codebuddy.cn、chat=copilot.tencent.com）")
}

// Scenario：global 账号两个域同样分离（base 按 realm 切换后依然不同）。
func TestCodeBuddyActivityEndpointsSeparateDomainsForGlobalRealm(t *testing.T) {
	svc := NewCodeBuddyAdminService(nil, nil, nil)

	global := newCodeBuddyActivityAccount(2, "u-2", "t-2")
	global.Credentials["realm"] = "global"

	require.Equal(t, CodeBuddyBillingBaseGlobal+codebuddy.CodeBuddyActivityReportPath,
		svc.codeBuddyActivityReportEndpoint(global))
	require.Equal(t, CodeBuddyChatBaseGlobal+CodeBuddyActivityStreakPath,
		svc.codeBuddyActivityStreakEndpoint(global))
}

// Scenario：路径本身固定且各自唯一（上报不带 /v2 回落族、自检在 growth 族）。
func TestCodeBuddyActivityEndpointPathsAreFixed(t *testing.T) {
	require.Equal(t, "/v2/report", codebuddy.CodeBuddyActivityReportPath)
	require.Equal(t, "/activity/growth/streak", CodeBuddyActivityStreakPath)
	require.NotEqual(t, codebuddy.CodeBuddyActivityReportPath, CodeBuddyActivityStreakPath)
}

// --- 必写测试 ④：条间 1.5s 间隔 ---

// captureActivityGaps 记录 sleep 收到的间隔，并立即返回（不真等）。
func captureActivityGaps(t *testing.T) *[]time.Duration {
	t.Helper()
	original := codeBuddyActivitySleep
	gaps := &[]time.Duration{}
	codeBuddyActivitySleep = func(ctx context.Context, d time.Duration) error {
		*gaps = append(*gaps, d)
		return ctx.Err()
	}
	t.Cleanup(func() { codeBuddyActivitySleep = original })
	return gaps
}

// Scenario：同一账号内 5 条之间的间隔必须是 **1.5s**，且条与条之间都要等
// （5 条 = 4 个间隔，最后一条后面不等）。
//
// ⚠️ 为什么必须专门测：其它用例统统把间隔压成 0（不然要真等 6 秒），
// 于是"间隔到底是多少"**从未被断言过**——有人把它改成 0（秒发 5 条触发风控）
// 或改成 10 秒（整轮超时），全套用例照样全绿。
func TestReportCodeBuddyActivityGapsBetweenEventsAreOnePointFiveSeconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	gaps := captureActivityGaps(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.NoError(t, result.Err)
	require.Equal(t, 5, result.Reported)
	require.Len(t, *gaps, 4, "5 条之间应有 4 个间隔（最后一条后面不再等）")
	for i, gap := range *gaps {
		require.Equal(t, 1500*time.Millisecond, gap,
			"第 %d 个间隔应为 1.5s（避免秒发触发上游风控）", i+1)
	}
}

// Scenario：上报条数可配——1 条时不产生任何间隔（不空等）。
func TestReportCodeBuddyActivitySingleEventWaitsNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	gaps := captureActivityGaps(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 1)

	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Reported)
	require.Empty(t, *gaps, "只有一条时不该产生间隔")
}

// Scenario：失败即停时，失败那条之后**不再等待**（不白等 1.5s）。
func TestReportCodeBuddyActivityDoesNotWaitAfterFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CodeBuddyActivityStreakPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
			return
		}
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current >= 2 {
			_, _ = w.Write([]byte(`{"code":"99999","msg":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	gaps := captureActivityGaps(t)

	account := newCodeBuddyActivityAccount(1, "u-1", "token-1")
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(account), nil).WithTestBaseURL(server.URL)

	result := svc.ReportCodeBuddyActivity(context.Background(), account, 5)

	require.Error(t, result.Err)
	require.Equal(t, 1, result.Reported)
	// 第 1 条成功 → 等 1.5s → 第 2 条失败 → 停止，不再等。
	require.Len(t, *gaps, 1, "失败后不该再有间隔等待")
}
