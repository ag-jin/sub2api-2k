package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/stretchr/testify/require"
)

// A6 批 P4：五个通道的上游调用契约。
//
// 这一层是 P3 callCodeBuddyGrowth 之上的薄封装，所以测试聚焦"发出去的形状与
// 域是否正确"、"哪些码算正常态"，而不是重复测 P3 的信封解析。

// growthRecorder 记录到达的请求（路径 + 请求体原文）。
type growthRecorder struct {
	mu       sync.Mutex
	requests []recordedGrowthRequest
}

type recordedGrowthRequest struct {
	Method string
	Path   string
	Body   []byte
	Header http.Header
}

func (r *growthRecorder) record(req *http.Request, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedGrowthRequest{
		Method: req.Method, Path: req.URL.Path, Body: body, Header: req.Header.Clone(),
	})
}

func (r *growthRecorder) snapshot() []recordedGrowthRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedGrowthRequest(nil), r.requests...)
}

func newGrowthTestAccount(id int64) *Account {
	return &Account{
		ID: id, Name: "cb", Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"uid": "u-1", "access_token": "tok-1"},
	}
}

func newGrowthTestService(t *testing.T, handler http.HandlerFunc) (*CodeBuddyAdminService, *growthRecorder) {
	t.Helper()
	recorder := &growthRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0)
		if r.Body != nil {
			buf := make([]byte, 1<<16)
			n, _ := r.Body.Read(buf)
			body = buf[:n]
		}
		recorder.record(r, body)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	// growth 层的 base 按域解析，用专用的注入缝（testBaseURL 只覆盖 qr/签到路径）。
	SetCodeBuddyGrowthTestBase(server.URL)
	t.Cleanup(func() { SetCodeBuddyGrowthTestBase("") })
	return NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(), nil), recorder
}

// --- 补签判据链（必写测试 ①，四个分支）---

// Scenario：昨日有分（没漏签）→ **不补**，且不说成失败。
func TestDecideStreakMakeupSkipsWhenYesterdayHasScore(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyGrowthHeatmapPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[{"date":"2026-09-21","score":5}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)
	state := &codeBuddyGrowthStreakState{makeupCardBalance: 3}

	// 固定"昨日"为 2026-09-21：用当日=09-22 的账本口径不便注入，这里直接
	// 构造 heatmap 覆盖"昨日"——测试关心的是分支语义而非具体日期。
	decision := svc.decideStreakMakeupWithYesterday(context.Background(), account, state, "2026-09-21")

	require.False(t, decision.ShouldUse, "昨日有分不该补签")
	require.NotEmpty(t, decision.SkipReason)
	require.Contains(t, decision.SkipReason, "已活跃", "原因要说清是没漏签，而不是含糊的失败")
}

// Scenario：昨日漏签但**无补签卡** → 不补。
func TestDecideStreakMakeupSkipsWhenNoCard(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyGrowthHeatmapPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[{"date":"2026-09-21","score":0}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	state := &codeBuddyGrowthStreakState{makeupCardBalance: 0}

	decision := svc.decideStreakMakeupWithYesterday(context.Background(), newGrowthTestAccount(1), state, "2026-09-21")

	require.False(t, decision.ShouldUse)
	require.Contains(t, decision.SkipReason, "无补签卡")
}

// Scenario：昨日漏签 + 有卡 → **应补**，且目标日期是昨日。
func TestDecideStreakMakeupUsesCardWhenGapAndCardPresent(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyGrowthHeatmapPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[{"date":"2026-09-21","score":0}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	state := &codeBuddyGrowthStreakState{makeupCardBalance: 1}

	decision := svc.decideStreakMakeupWithYesterday(context.Background(), newGrowthTestAccount(1), state, "2026-09-21")

	require.True(t, decision.ShouldUse, "漏签且有卡就该补（连登一断要重攒 7 天）")
	require.Equal(t, "2026-09-21", decision.TargetDate)
}

// Scenario：heatmap **查询失败** → 静默（只读判据拿不到，不动手）。
func TestDecideStreakMakeupStaysQuietWhenHeatmapFails(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyGrowthHeatmapPath {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":50001,"msg":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	state := &codeBuddyGrowthStreakState{makeupCardBalance: 5}

	decision := svc.decideStreakMakeupWithYesterday(context.Background(), newGrowthTestAccount(1), state, "2026-09-21")

	require.False(t, decision.ShouldUse, "判据拿不到时不得贸然补签（会白白消耗卡）")
	// 只查了 heatmap，**没有**发补签请求。
	for _, req := range recorder.snapshot() {
		require.NotEqual(t, codebuddy.CodeBuddyGrowthMakeupCardUsePath, req.Path,
			"判据失败时不该发出补签请求")
	}
}

// --- 抽奖：每次新 client_token（不幂等，见通道注释）---

// Scenario：抽 n 次 = n 个请求，且**每次 client_token 都不同**。
//
// 这是参考实现点名的 security-relevant 要求（`scheduler.go:637`）：
// 复用旧键会被上游按幂等吞掉，表现为"发出去了但没抽到"。
func TestLotteryDrawUsesFreshClientTokenEachTime(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"prize_code":"p1","prize_name":"积分","prize_type":"credit","credit_amount":10}}`))
	})

	drawn, err := svc.drawCodeBuddyLotteryTimes(context.Background(), newGrowthTestAccount(1), 3)

	require.NoError(t, err)
	require.Equal(t, 3, drawn)
	requests := recorder.snapshot()
	require.Len(t, requests, 3)

	tokens := map[string]bool{}
	for _, req := range requests {
		require.Equal(t, codebuddy.CodeBuddyGrowthLotteryDrawPath, req.Path)
		key := extractJSONField(t, req.Body, "client_token")
		require.NotEmpty(t, key, "client_token 必须带（上游用它做幂等键）")
		tokens[key] = true
	}
	require.Len(t, tokens, 3, "每次抽奖必须用新的 client_token，不能复用")
}

// Scenario：中途失败即停止，返回已成功次数 + 错误（不谎报全成功）。
func TestLotteryDrawStopsOnFailureAndReportsPartial(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current >= 3 {
			_, _ = w.Write([]byte(`{"code":"99999","msg":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"prize_type":"credit"}}`))
	})

	drawn, err := svc.drawCodeBuddyLotteryTimes(context.Background(), newGrowthTestAccount(1), 5)

	require.Error(t, err, "失败必须如实报出")
	require.Equal(t, 2, drawn, "已成功 2 次后失败应返回 2")
	mu.Lock()
	require.Equal(t, 3, calls, "失败后不该继续抽")
	mu.Unlock()
}

// --- 礼包/补偿：billing 域 + 缺 data 不算失败 ---

// Scenario：claim-gift 返回 `{"code":0}` 不带 data → 按 0 记，**不是错误**。
//
// 上游对"已领过"经常只回 code=0 不带 data。把缺 data 当失败会让所有
// 已领过的账号天天报错误告警。
func TestClaimGiftWithoutDataIsNotAnError(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	})

	credit, err := svc.claimCodeBuddyGrowthGift(context.Background(), newGrowthTestAccount(1))

	require.NoError(t, err, "缺 data 不该算失败")
	require.Equal(t, 0, credit)
	requests := recorder.snapshot()
	require.Len(t, requests, 1)
	require.Equal(t, codebuddy.CodeBuddyClaimGiftPath, requests[0].Path)
}

// --- 领养：门槛未达是预期行为 ---

// Scenario：HTTP 400 + first_buddy 关键词 → 判为"门槛未达"（预期，非故障）。
func TestBuddyThresholdNotMetIsRecognised(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyBuddyInfoPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
			return
		}
		if r.URL.Path == codebuddy.CodeBuddyBuddyFirstPath {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":40001,"msg":"first_buddy task not completed yet"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})
	account := newGrowthTestAccount(1)

	result := svc.RunCodeBuddyGrowthAdoptNow(context.Background(), account, "2026-09-22")

	require.True(t, result.ThresholdNotMet, "门槛未达是预期行为，要能识别")
	require.Empty(t, result.Error, "门槛未达不该记成错误")
	require.False(t, result.Adopted)
}

// Scenario：已有猫 → 跳过（不发 buddy/first）。
func TestAdoptSkipsWhenBuddyAlreadyExists(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codebuddy.CodeBuddyBuddyInfoPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":42,"name":"咪咪"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	})

	result := svc.RunCodeBuddyGrowthAdoptNow(context.Background(), newGrowthTestAccount(1), "2026-09-22")

	require.False(t, result.Adopted)
	require.Contains(t, result.SkipReason, "已有猫")
	for _, req := range recorder.snapshot() {
		require.NotEqual(t, codebuddy.CodeBuddyBuddyFirstPath, req.Path,
			"已有猫时不该再发领养请求")
	}
}

// --- trial：仅 global，且幂等码算正常态 ---

// Scenario：CN 账号**不发请求**（CN 无该端点，发出去只会 404）。
func TestTrialSkipsNonGlobalWithoutRequest(t *testing.T) {
	svc, recorder := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	})

	result := svc.runCodeBuddyGrowthTrial(context.Background(), newGrowthTestAccount(1), "2026-09-22")

	require.Empty(t, result.Error)
	require.Contains(t, result.SkipReason, "国际版")
	require.Empty(t, recorder.snapshot(), "CN 账号不该发出 trial 请求")
}

// Scenario：global 账号收到幂等码 14051 → 判为"已领过"（正常态），并记台账。
func TestTrialAlreadyClaimedIsIdempotentSuccess(t *testing.T) {
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":14051,"msg":"already claimed"}`))
	})
	account := newGrowthTestAccount(1)
	account.Credentials["realm"] = "global"

	result := svc.runCodeBuddyGrowthTrial(context.Background(), account, "2026-09-22")

	require.True(t, result.AlreadyClaimed)
	require.Empty(t, result.Error, "已领过是幂等正常态，不是错误")
	require.NotEmpty(t, codeBuddyGrowthLedgerValue(account, ledgerTrialClaimed),
		"应记台账，避免后续每轮都打一次上游")
}

// extractJSONField 从请求体原文里取一个顶层字符串字段（测试用）。
// 读到**原文**而不是反序列化再比较，是为了断言"真正发出去的字节"。
func extractJSONField(t *testing.T, body []byte, field string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("请求体不是 JSON: %s", string(body))
	}
	value, _ := decoded[field].(string)
	return value
}
