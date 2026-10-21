//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	contTestCreatedEvent = `{"type":"response.created","sequence_number":1,"response":{"id":"resp_test","created_at":1700000123}}`
	contTestMessageEvent = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_test","type":"message","role":"assistant","content":[]}}`
	contTestDeltaEvent   = `{"type":"response.output_text.delta","item_id":"msg_test","output_index":0,"content_index":0,"delta":"abc","sequence_number":2}`
	contTestCapacityJSON = `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"server is overloaded"}}}`
	contTestNormalJSON   = `{"type":"response.failed","response":{"error":{"code":"invalid_request_error"}}}`
	contTestFunctionCall = `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":""}}`
	contTestFunctionDone = `{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{}"}}`
	contTestRefusalEvent = `{"type":"response.refusal.delta","item_id":"msg_test","output_index":0,"content_index":1,"delta":"no"}`
	contTestImageEvent   = `{"type":"response.image_generation_call.partial_image","output_index":0,"partial_image_b64":"AAAA"}`

	contTestReasoningAdded = `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`
	contTestReasoningDone  = `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`
	contTestReasoningDelta = `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"thinking"}`
)

func contTestGinContext(path string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	return c
}

func contTestAccount(platform, accountType string, enabled, passthrough bool) *Account {
	extra := map[string]any{}
	if enabled {
		extra[openAIStream503AutoContinueKey] = true
	}
	if passthrough {
		extra["openai_passthrough"] = true
	}
	return &Account{ID: 7, Name: "cont-test", Platform: platform, Type: accountType, Extra: extra}
}

// contTestObserveTextDelta 走真实热路径（同一个 observe 入口），
// 避免测试直接写 state 内部字段。
func contTestObserveTextDelta(t *testing.T, svc *OpenAIGatewayService, st *openAIStreamContinuationState, delta string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       "msg_test",
		"output_index":  0,
		"content_index": 0,
		"delta":         delta,
	})
	require.NoError(t, err)
	svc.observeOpenAIStreamContinuationEvent(st, payload, "")
}

func contTestObservedState(t *testing.T, svc *OpenAIGatewayService, c *gin.Context, account *Account) *openAIStreamContinuationState {
	t.Helper()
	st := svc.newOpenAIStreamContinuationState(account, c, true, []byte(`{"model":"gpt-5","input":[]}`))
	require.NotNil(t, st)
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestMessageEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestDeltaEvent), "")
	return st
}

func TestOpenAIStreamContinuationStateCreationGate(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := contTestGinContext("/v1/responses")
	compactC := contTestGinContext("/v1/responses/compact")

	tests := []struct {
		name        string
		account     *Account
		ctx         *gin.Context
		reqStream   bool
		wantCreated bool
	}{
		{name: "switch_off", account: contTestAccount(PlatformOpenAI, AccountTypeOAuth, false, false), ctx: c, reqStream: true, wantCreated: false},
		{name: "api_key", account: contTestAccount(PlatformOpenAI, AccountTypeAPIKey, true, false), ctx: c, reqStream: true, wantCreated: false},
		{name: "setup_token_switch_off", account: contTestAccount(PlatformOpenAI, AccountTypeSetupToken, false, false), ctx: c, reqStream: true, wantCreated: false},
		{name: "passthrough", account: contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, true), ctx: c, reqStream: true, wantCreated: false},
		{name: "compact_path", account: contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), ctx: compactC, reqStream: true, wantCreated: false},
		{name: "non_stream", account: contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), ctx: c, reqStream: false, wantCreated: false},
		{name: "non_openai", account: contTestAccount(PlatformAnthropic, AccountTypeOAuth, true, false), ctx: c, reqStream: true, wantCreated: false},
		{name: "oauth_enabled", account: contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), ctx: c, reqStream: true, wantCreated: true},
		{name: "setup_token_enabled", account: contTestAccount(PlatformOpenAI, AccountTypeSetupToken, true, false), ctx: c, reqStream: true, wantCreated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := svc.newOpenAIStreamContinuationState(tt.account, tt.ctx, tt.reqStream, []byte(`{"input":[]}`))
			if tt.wantCreated {
				require.NotNil(t, st)
				require.True(t, st.enabled)
				require.Equal(t, int64(7), st.accountID)
			} else {
				require.Nil(t, st)
			}
		})
	}
}

func TestOpenAIStreamContinuationObserveTracksTextAndCoordinates(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := contTestGinContext("/v1/responses")
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	st := contTestObservedState(t, svc, c, account)

	require.Equal(t, "abc", st.ObservedPartialText())
	require.True(t, st.HasOpenTextItem())

	st.BeginContinuation(OpenAIUsage{InputTokens: 11}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.Equal(t, "resp_test", snap.responseID)
	require.Equal(t, int64(1700000123), snap.createdAt)
	require.Equal(t, "abc", snap.partialText)
	require.Equal(t, "msg_test", snap.openTextItemID)
	require.Equal(t, 0, snap.openTextIndex)
	require.Equal(t, 0, snap.openTextContent)
	require.Equal(t, 1, snap.nextOutputIndex)
	require.Equal(t, int64(2), snap.lastSequence)
	require.Equal(t, 11, snap.priorUsage.InputTokens)
	require.True(t, snap.keepOriginalID)
}

func TestOpenAIStreamContinuationObserveTracksInProgressNonTextItems(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := svc.newOpenAIStreamContinuationState(contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), contTestGinContext("/v1/responses"), true, nil)
	require.NotNil(t, st)

	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestFunctionCall), "")
	require.True(t, st.HasInProgressNonText())

	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestFunctionDone), "")
	require.False(t, st.HasInProgressNonText())

	st.BeginContinuation(OpenAIUsage{}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.Len(t, snap.completedItems, 1)
	require.JSONEq(t, `{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{}"}`, string(snap.completedItems[0]))
}

func TestOpenAIStreamContinuationObserveTracksRefusalAndImage(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := svc.newOpenAIStreamContinuationState(contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), contTestGinContext("/v1/responses"), true, nil)
	require.NotNil(t, st)

	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestRefusalEvent), "")
	require.True(t, st.SawRefusal())

	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestImageEvent), "")
	require.True(t, st.SawImageOutput())
}

func TestOpenAIStreamContinuationObserveInheritsSnapshotOnContinuationAttempt(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := contTestGinContext("/v1/responses")
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	st := contTestObservedState(t, svc, c, account)

	st.BeginContinuation(OpenAIUsage{InputTokens: 3}, nil)
	// 续跑 attempt 的帧已被 05 重写为快照坐标；Observe 只负责继续累加。
	svc.observeOpenAIStreamContinuationEvent(st, []byte(`{"type":"response.output_text.delta","item_id":"msg_test","output_index":0,"content_index":0,"delta":"def","sequence_number":10}`), "")

	require.Equal(t, "abcdef", st.ObservedPartialText())

	st.BeginContinuation(OpenAIUsage{InputTokens: 3, OutputTokens: 4}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.Equal(t, "abcdef", snap.partialText)
	require.Equal(t, "msg_test", snap.openTextItemID)
	require.Equal(t, int64(10), snap.lastSequence)
	require.Equal(t, 1, snap.nextOutputIndex)
	require.Equal(t, 4, snap.priorUsage.OutputTokens)
}

func TestOpenAIStreamContinuationObserveKeepsMaxSequence(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := svc.newOpenAIStreamContinuationState(contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), contTestGinContext("/v1/responses"), true, nil)
	require.NotNil(t, st)

	svc.observeOpenAIStreamContinuationEvent(st, []byte(`{"type":"response.created","sequence_number":9,"response":{"id":"resp_1"}}`), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(`{"type":"response.output_text.delta","delta":"x","output_index":0,"sequence_number":4}`), "")

	st.BeginContinuation(OpenAIUsage{}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.Equal(t, int64(9), snap.lastSequence)
}

// S1: 累加必须是 O(1) 追加 + 独立字节计数；超过上限后只停止存储文本，
// 资格判定仍按真实字节数触发 partial_too_large。
func TestOpenAIStreamContinuationObserveCapsPartialTextButCountsBytes(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := svc.newOpenAIStreamContinuationState(contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), contTestGinContext("/v1/responses"), true, nil)
	require.NotNil(t, st)
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestMessageEvent), "")

	contTestObserveTextDelta(t, svc, st, strings.Repeat("x", openAIStream503AutoContinueMaxPartialBytes+1))

	require.Len(t, st.ObservedPartialText(), openAIStream503AutoContinueMaxPartialBytes,
		"stored partial text is capped at the limit")
	ok, reason := st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, "partial_too_large", reason,
		"eligibility must use the true byte count, not the capped text length")
}

func TestOpenAIStreamContinuationObserveAccumulatesManyDeltas(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := svc.newOpenAIStreamContinuationState(contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), contTestGinContext("/v1/responses"), true, nil)
	require.NotNil(t, st)
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestMessageEvent), "")

	for i := 0; i < 2000; i++ {
		contTestObserveTextDelta(t, svc, st, "ab")
	}

	require.Equal(t, strings.Repeat("ab", 2000), st.ObservedPartialText())
	ok, reason := st.CanContinue(time.Now())
	require.True(t, ok, "small deltas stay eligible: %s", reason)

	st.BeginContinuation(OpenAIUsage{}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.Equal(t, strings.Repeat("ab", 2000), snap.partialText)
}

func TestOpenAIStreamContinuationEligibility(t *testing.T) {
	svc := &OpenAIGatewayService{}
	capacity := []byte(contTestCapacityJSON)

	newBase := func(t *testing.T) (*gin.Context, *Account, *openAIStreamContinuationState) {
		t.Helper()
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		return c, account, contTestObservedState(t, svc, c, account)
	}

	t.Run("eligible", func(t *testing.T) {
		c, account, st := newBase(t)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.True(t, ok)
		require.Empty(t, reason)
	})

	t.Run("disabled", func(t *testing.T) {
		c, account, st := newBase(t)
		st.enabled = false
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "disabled", reason)
	})

	t.Run("nil_state", func(t *testing.T) {
		c, account, _ := newBase(t)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, nil, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "disabled", reason)
	})

	t.Run("account_nil", func(t *testing.T) {
		c, _, st := newBase(t)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, nil, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "account", reason)
	})

	t.Run("account_non_openai", func(t *testing.T) {
		c, account, st := newBase(t)
		account.Platform = PlatformAnthropic
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "account", reason)
	})

	t.Run("output_not_started", func(t *testing.T) {
		c, account, st := newBase(t)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, false, false)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("cyber_hit", func(t *testing.T) {
		c, account, st := newBase(t)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, true)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("no_open_text", func(t *testing.T) {
		c, account, _ := newBase(t)
		st := svc.newOpenAIStreamContinuationState(account, c, true, nil)
		require.NotNil(t, st)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.True(t, ok, "只有 created 的零文本形态应可续跑: %s", reason)
		require.Empty(t, reason)
	})

	t.Run("in_progress_reasoning_zero_partial", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := svc.newOpenAIStreamContinuationState(account, c, true, nil)
		require.NotNil(t, st)
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestReasoningAdded), "")
		require.False(t, st.HasOpenTextItem())
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.True(t, ok, "进行中的 reasoning 不阻断零 partial 续跑: %s", reason)
		require.Empty(t, reason)
	})

	t.Run("completed_reasoning_zero_partial", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := svc.newOpenAIStreamContinuationState(account, c, true, nil)
		require.NotNil(t, st)
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestReasoningAdded), "")
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestReasoningDone), "")
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.True(t, ok, "已完成的 reasoning 视为可回滚: %s", reason)
		require.Empty(t, reason)

		st.BeginContinuation(OpenAIUsage{}, nil)
		snap := st.Snapshot()
		require.NotNil(t, snap)
		require.True(t, snap.zeroPartial)
		require.Len(t, snap.completedItems, 1)
		require.Equal(t, "rs_1", gjson.GetBytes(snap.completedItems[0], "id").String())
	})

	t.Run("completed_function_call_zero_partial", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := svc.newOpenAIStreamContinuationState(account, c, true, nil)
		require.NotNil(t, st)
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestFunctionCall), "")
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestFunctionDone), "")
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("in_progress_function_call_zero_partial", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := svc.newOpenAIStreamContinuationState(account, c, true, nil)
		require.NotNil(t, st)
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestFunctionCall), "")
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("in_progress_non_text", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := contTestObservedState(t, svc, c, account)
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestFunctionCall), "")
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("refusal", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := contTestObservedState(t, svc, c, account)
		st.sawRefusal = true
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("image_output", func(t *testing.T) {
		c := contTestGinContext("/v1/responses")
		account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
		st := contTestObservedState(t, svc, c, account)
		st.sawImageOutput = true
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "ineligible_output", reason)
	})

	t.Run("not_capacity", func(t *testing.T) {
		c, account, st := newBase(t)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, []byte(contTestNormalJSON), true, false)
		require.False(t, ok)
		require.Equal(t, "not_capacity", reason)
	})

	t.Run("client_gone", func(t *testing.T) {
		c, account, st := newBase(t)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		c.Request = req.WithContext(ctx)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "client_gone", reason)
	})

	t.Run("budget_attempts", func(t *testing.T) {
		c, account, st := newBase(t)
		st.continuations = openAIStream503ContinueDefaultMaxContinuations
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, openAIStream503AutoContinueReasonBudgetAttempts, reason)
	})

	t.Run("budget_time", func(t *testing.T) {
		c, account, st := newBase(t)
		st.firstTriggerAt = time.Now().Add(-openAIStream503ContinueDefaultTotalBudget - time.Second)
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, openAIStream503AutoContinueReasonBudgetTime, reason)
	})

	t.Run("partial_too_large", func(t *testing.T) {
		c, account, st := newBase(t)
		contTestObserveTextDelta(t, svc, st, strings.Repeat("x", openAIStream503AutoContinueMaxPartialBytes+1))
		ok, reason := svc.openAIStream503AutoContinueEligible(c, account, st, capacity, true, false)
		require.False(t, ok)
		require.Equal(t, "partial_too_large", reason)
	})
}

func TestOpenAIStreamContinuationCanContinueBudget(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := contTestObservedState(t, svc, contTestGinContext("/v1/responses"), contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false))

	require.Equal(t, 6, openAIStream503ContinueDefaultMaxContinuations,
		"v1.2 默认续跑次数上限加大到 6")
	for i := 0; i < openAIStream503ContinueDefaultMaxContinuations; i++ {
		ok, reason := st.CanContinue(time.Now())
		require.True(t, ok, "continuation %d should be allowed: %s", i+1, reason)
		st.BeginContinuation(OpenAIUsage{}, nil)
	}
	require.Equal(t, 6, st.Continuations())
	require.True(t, st.InContinuation())

	ok, reason := st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetAttempts, reason)
}

func TestOpenAIStreamContinuationCanContinueElapsed(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := contTestObservedState(t, svc, contTestGinContext("/v1/responses"), contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false))
	st.firstTriggerAt = time.Now().Add(-openAIStream503ContinueDefaultTotalBudget - time.Second)

	ok, reason := st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, openAIStream503AutoContinueReasonBudgetTime, reason)
}

func TestOpenAIStreamContinuationCanContinuePartialTooLarge(t *testing.T) {
	svc := &OpenAIGatewayService{}
	newState := func() *openAIStreamContinuationState {
		st := svc.newOpenAIStreamContinuationState(contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false), contTestGinContext("/v1/responses"), true, nil)
		require.NotNil(t, st)
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
		svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestMessageEvent), "")
		return st
	}

	st := newState()
	contTestObserveTextDelta(t, svc, st, strings.Repeat("x", openAIStream503AutoContinueMaxPartialBytes))
	ok, reason := st.CanContinue(time.Now())
	require.True(t, ok, "exactly at the limit stays eligible: %s", reason)

	st = newState()
	contTestObserveTextDelta(t, svc, st, strings.Repeat("x", openAIStream503AutoContinueMaxPartialBytes+1))
	ok, reason = st.CanContinue(time.Now())
	require.False(t, ok)
	require.Equal(t, "partial_too_large", reason)
}

func TestBuildOpenAIStreamContinuationBodyAppendsForArrayInput(t *testing.T) {
	original := `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"f"}],"instructions":"sys","include":["reasoning.encrypted_content"],"prompt_cache_key":"sess_1","store":false,"previous_response_id":"resp_0"}`
	bodyIn := []byte(original)

	out, err := buildOpenAIStreamContinuationBody(bodyIn, "partial one")
	require.NoError(t, err)
	require.Equal(t, original, string(bodyIn), "source body must not be mutated")

	items := gjson.GetBytes(out, "input").Array()
	require.Len(t, items, 3)
	require.Equal(t, "user", items[0].Get("role").String())
	require.Equal(t, "hi", items[0].Get("content.0.text").String())
	require.Equal(t, "assistant", items[1].Get("role").String())
	require.Equal(t, "output_text", items[1].Get("content.0.type").String())
	require.Equal(t, "partial one", items[1].Get("content.0.text").String())
	require.Equal(t, "user", items[2].Get("role").String())
	require.Equal(t, "input_text", items[2].Get("content.0.type").String())
	require.Equal(t, openAIStream503AutoContinueInstruction, items[2].Get("content.0.text").String())

	for _, field := range []string{"model", "tools", "instructions", "include", "prompt_cache_key", "store", "previous_response_id"} {
		require.Equal(t, gjson.Get(original, field).Raw, gjson.GetBytes(out, field).Raw, "field %s must be preserved", field)
	}
}

func TestBuildOpenAIStreamContinuationBodyRewritesStringInput(t *testing.T) {
	out, err := buildOpenAIStreamContinuationBody([]byte(`{"model":"m","input":"hello"}`), "rest")
	require.NoError(t, err)

	items := gjson.GetBytes(out, "input").Array()
	require.Len(t, items, 3)
	require.Equal(t, "user", items[0].Get("role").String())
	require.Equal(t, "input_text", items[0].Get("content.0.type").String())
	require.Equal(t, "hello", items[0].Get("content.0.text").String())
	require.Equal(t, "assistant", items[1].Get("role").String())
	require.Equal(t, "rest", items[1].Get("content.0.text").String())
	require.Equal(t, openAIStream503AutoContinueInstruction, items[2].Get("content.0.text").String())
	require.Equal(t, "m", gjson.GetBytes(out, "model").String())
}

func TestBuildOpenAIStreamContinuationBodyAppendsToEmptyArray(t *testing.T) {
	out, err := buildOpenAIStreamContinuationBody([]byte(`{"input":[]}`), "rest")
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(out, "input").Array(), 2)
}

func TestBuildOpenAIStreamContinuationBodyErrors(t *testing.T) {
	t.Run("missing_input", func(t *testing.T) {
		_, err := buildOpenAIStreamContinuationBody([]byte(`{"model":"m"}`), "rest")
		require.ErrorIs(t, err, ErrNoInput)
	})

	t.Run("null_input", func(t *testing.T) {
		_, err := buildOpenAIStreamContinuationBody([]byte(`{"input":null}`), "rest")
		require.ErrorIs(t, err, ErrNoInput)
	})

	t.Run("object_input", func(t *testing.T) {
		_, err := buildOpenAIStreamContinuationBody([]byte(`{"input":{"a":1}}`), "rest")
		require.ErrorIs(t, err, ErrNoInput)
	})

	t.Run("empty_partial", func(t *testing.T) {
		_, err := buildOpenAIStreamContinuationBody([]byte(`{"input":[]}`), "")
		require.ErrorIs(t, err, ErrNoPartial)
	})
}

func TestBuildOpenAIStreamContinuationBodyKeepsNonHTMLEscaping(t *testing.T) {
	out, err := buildOpenAIStreamContinuationBody([]byte(`{"input":[]}`), `a <b> & c`)
	require.NoError(t, err)
	require.Contains(t, string(out), `a <b> & c`)
	require.NotContains(t, string(out), `\u003c`)
	require.NotContains(t, string(out), `\u0026`)
}

func TestOpenAIStreamContinuationNextRequestBody(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := contTestGinContext("/v1/responses")
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	body := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	st := svc.newOpenAIStreamContinuationState(account, c, true, body)
	require.NotNil(t, st)
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestMessageEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestDeltaEvent), "")

	st.BeginContinuation(OpenAIUsage{InputTokens: 5}, nil)
	out, err := st.NextRequestBody()
	require.NoError(t, err)

	items := gjson.GetBytes(out, "input").Array()
	require.Len(t, items, 3)
	require.Equal(t, "abc", items[1].Get("content.0.text").String())
	require.Equal(t, openAIStream503AutoContinueInstruction, items[2].Get("content.0.text").String())
	require.Equal(t, body, []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
}

func TestOpenAIStreamContinuationZeroPartialNextRequestBodyReplaysOriginalBody(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := contTestGinContext("/v1/responses")
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	body := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"prompt_cache_key":"k"}`)
	st := svc.newOpenAIStreamContinuationState(account, c, true, body)
	require.NotNil(t, st)
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestReasoningAdded), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestReasoningDelta), "")
	require.False(t, st.HasOpenTextItem(), "reasoning 帧不得被视为已打开文本 item")
	require.Zero(t, st.textBytes)

	st.BeginContinuation(OpenAIUsage{}, nil)
	require.True(t, st.Snapshot().zeroPartial)

	got, err := st.NextRequestBody()
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, body), "零 partial 必须逐字节重放原始 body")
	require.NotContains(t, string(got), "assistant")
	require.NotContains(t, string(got), openAIStream503AutoContinueInstruction)

	original := append([]byte(nil), body...)
	got[0] = '['
	require.True(t, bytes.Equal(body, original), "重放结果必须是独立拷贝，改写不得影响原 body")
}

func TestOpenAIStreamContinuationEmptyOpenMessageReplaysButMaps(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := contTestGinContext("/v1/responses")
	account := contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false)
	body := []byte(`{"model":"gpt-6.1-sol","input":[]}`)
	st := svc.newOpenAIStreamContinuationState(account, c, true, body)
	require.NotNil(t, st)
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestCreatedEvent), "")
	svc.observeOpenAIStreamContinuationEvent(st, []byte(contTestMessageEvent), "")
	require.True(t, st.HasOpenTextItem())
	require.Zero(t, st.textBytes)

	st.BeginContinuation(OpenAIUsage{}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.False(t, snap.zeroPartial, "文本 item 已 added（零字节）仍走 v1 映射模式")
	require.Empty(t, snap.partialText)

	got, err := st.NextRequestBody()
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, body), "空 partial 同样重放原始 body")
}

func TestOpenAIStreamContinuationCounterAccessors(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := contTestObservedState(t, svc, contTestGinContext("/v1/responses"), contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false))

	st.AddOverlapTrimmedBytes(12)
	st.AddDroppedReasoningEvents(2)
	st.BeginContinuation(OpenAIUsage{}, nil)
	snap := st.Snapshot()
	require.NotNil(t, snap)
	require.Equal(t, 12, snap.overlapTrimmedBytes)
	require.Equal(t, 2, snap.droppedReasoningEvents)

	// 05 在续跑 attempt 期间继续累加；07 在收尾时通过 live accessor 读取。
	st.AddOverlapTrimmedBytes(3)
	st.AddDroppedReasoningEvents(1)
	require.Equal(t, 15, st.OverlapTrimmedBytes())
	require.Equal(t, 3, st.DroppedReasoningEvents())
}

func TestOpenAIStreamContinuationBackoff(t *testing.T) {
	svc := &OpenAIGatewayService{}
	st := contTestObservedState(t, svc, contTestGinContext("/v1/responses"), contTestAccount(PlatformOpenAI, AccountTypeOAuth, true, false))

	assertInRange := func(d time.Duration, min, max time.Duration) {
		t.Helper()
		require.GreaterOrEqual(t, d, min)
		require.LessOrEqual(t, d, max)
	}

	// v1.2 默认序列：150ms 起每次翻倍、封顶 2000ms，±20% 抖动。
	st.BeginContinuation(OpenAIUsage{}, nil)
	assertInRange(st.Backoff(), 120*time.Millisecond, 180*time.Millisecond)

	st.BeginContinuation(OpenAIUsage{}, nil)
	assertInRange(st.Backoff(), 240*time.Millisecond, 360*time.Millisecond)

	st.BeginContinuation(OpenAIUsage{}, nil)
	assertInRange(st.Backoff(), 480*time.Millisecond, 720*time.Millisecond)
}

func TestAddOpenAIUsageSumsEveryField(t *testing.T) {
	dst := OpenAIUsage{
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
		ImageInputTokens:         5,
		ImageCacheReadTokens:     6,
		ImageOutputTokens:        7,
		UpstreamCredit:           8,
	}
	src := OpenAIUsage{
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
		ImageInputTokens:         50,
		ImageCacheReadTokens:     60,
		ImageOutputTokens:        70,
		UpstreamCredit:           80,
	}

	addOpenAIUsage(&dst, src)

	require.Equal(t, 11, dst.InputTokens)
	require.Equal(t, 22, dst.OutputTokens)
	require.Equal(t, 33, dst.CacheCreationInputTokens)
	require.Equal(t, 44, dst.CacheReadInputTokens)
	require.Equal(t, 55, dst.ImageInputTokens)
	require.Equal(t, 66, dst.ImageCacheReadTokens)
	require.Equal(t, 77, dst.ImageOutputTokens)
	require.Equal(t, 88.0, dst.UpstreamCredit)
}

func TestOpenAIStreamContinueSignal(t *testing.T) {
	sig := &openAIStreamContinueSignal{
		capacityPayload:   []byte(contTestCapacityJSON),
		message:           "server is overloaded",
		upstreamRequestID: "req_1",
		usage:             OpenAIUsage{InputTokens: 3},
	}
	var err error = sig
	require.Equal(t, "server is overloaded", sig.Error())

	got, ok := asOpenAIStreamContinueSignal(err)
	require.True(t, ok)
	require.Same(t, sig, got)

	_, ok = asOpenAIStreamContinueSignal(errors.New("other"))
	require.False(t, ok)

	var missing *openAIStreamContinueSignal
	require.Equal(t, "", missing.Error())
}
