package codebuddy

import (
	"encoding/json"
	"testing"
	"time"
)

// A5 批 5.1 的事件体形状回归保护。
//
// 任务书坑 2：必须照抄官方 chat_request_send 的**完整字段形状**（36 字段），
// 不能"精简"。精简能通过当下上游校验，但会在上游加严时静默失效。
// 所以这里断言**等值**（字段集合 + 数量），而不是"包含若干关键字段"——
// 后者对"少了一个字段"完全无感（A1 的 L5 就是栽在只断言包含上）。

// expectedActivityEventJSONKeys 是官方 chat_request_send 的完整字段名清单，
// 与参考实现 workbuddy2api/internal/upstream/report.go 的 chatRequestEvent 一致。
var expectedActivityEventJSONKeys = []string{
	"eventCode", "timestamp", "reportDelay", "mode", "conversationId", "requestId",
	"inputLength", "requestModelId", "requestModelName", "isPlan",
	"isAutoExecuteTerminal", "isAutoModify", "codebaseEnable", "maxToken", "maxSteps",
	"temperature", "maxRetries", "mentionContexts", "knowledgeId", "knowledgeName",
	"codebaseId", "mentionContextCount", "command", "expertId", "recommendId",
	"skillId", "skillCount", "totalCount", "fileUri", "presentAt", "traceId",
	"rootRequestId", "parentConversationId", "agentName", "agentType", "userId",
}

func TestActivityEventCarriesFullChatRequestSendShape(t *testing.T) {
	event := NewCodeBuddyActivityEvent("u-1", "wb2api-1", "wb2api-1-r1", time.UnixMilli(1700000000000))
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 等值断言：数量 + 逐字段在场。删字段 / 改字段名都会红。
	if len(decoded) != len(expectedActivityEventJSONKeys) {
		t.Fatalf("event field count = %d, want %d (完整形状不可精简)",
			len(decoded), len(expectedActivityEventJSONKeys))
	}
	for _, key := range expectedActivityEventJSONKeys {
		if _, ok := decoded[key]; !ok {
			t.Errorf("event is missing field %q", key)
		}
	}
}

func TestActivityEventKeepsReferenceFieldValues(t *testing.T) {
	now := time.UnixMilli(1700000000000)
	event := NewCodeBuddyActivityEvent("u-1", "cid-1", "cid-1-r3", now)

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"eventCode", event.EventCode, "chat_request_send"},
		{"mode", event.Mode, "craft"},
		{"conversationId", event.ConversationID, "cid-1"},
		{"requestId", event.RequestID, "cid-1-r3"},
		{"inputLength", event.InputLength, 12},
		{"requestModelId", event.RequestModelID, "deepseek-v4-flash"},
		{"rootRequestId", event.RootRequestID, "cid-1"},
		{"parentConversationId", event.ParentConversationID, "cid-1"},
		{"agentName", event.AgentName, "default"},
		{"agentType", event.AgentType, "conversation"},
		{"timestamp", event.Timestamp, now.UnixMilli()},
		{"presentAt", event.PresentAt, now.UnixMilli()},
		{"userId", event.UserID, "u-1"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// 坑 1 的形状级保护：三个数组字段必须是 `[]` 而非 `null`。
// nil slice 会被序列化成 null，与官方的 [] 不同——形状校验类字段最容易这样被改掉。
func TestActivityEventMarshalsEmptyArraysNotNull(t *testing.T) {
	event := NewCodeBuddyActivityEvent("u-1", "cid-1", "cid-1-r1", time.UnixMilli(1))
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"mentionContexts", "knowledgeId", "knowledgeName"} {
		if got := string(decoded[key]); got != "[]" {
			t.Errorf("%s = %s, want [] (nil 序列化成 null 会改变事件形状)", key, got)
		}
	}
}

// 请求体必须是 JSON **数组**（参考实现 `json.Marshal([]chatRequestEvent{ev})`）。
// 发成裸对象会被上游按 schema 拒绝，且拒绝可能是静默的。
func TestMarshalActivityEventsProducesJSONArray(t *testing.T) {
	event := NewCodeBuddyActivityEvent("u-1", "cid-1", "cid-1-r1", time.UnixMilli(1))
	raw, err := MarshalActivityEvents([]CodeBuddyActivityEvent{event})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) == 0 || raw[0] != '[' {
		t.Fatalf("report body must be a JSON array, got %s", string(raw))
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("body is not a JSON array: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("array length = %d, want 1", len(decoded))
	}
}

// 坑 1 的直接保护：userId 为空的事件必须被判为不可发送。
// 空 userId 发出去会让上游 200 静默丢弃——比失败更糟，因为它看起来是成功的。
func TestValidateActivityEventRejectsMissingUserID(t *testing.T) {
	valid := NewCodeBuddyActivityEvent("u-1", "cid-1", "cid-1-r1", time.UnixMilli(1))
	if !ValidateCodeBuddyActivityEvent(valid) {
		t.Fatal("event with userId should be valid")
	}

	empty := NewCodeBuddyActivityEvent("", "cid-1", "cid-1-r1", time.UnixMilli(1))
	if ValidateCodeBuddyActivityEvent(empty) {
		t.Fatal("event without userId must be rejected (upstream silently drops it with HTTP 200)")
	}
}

// 坑 3：同一会话的 N 条共用 conversationId，requestId 各自独立。
func TestActivityConversationAndRequestIDs(t *testing.T) {
	conversationID := CodeBuddyActivityConversationID(time.UnixMilli(1700000000000))
	if conversationID != "wb2api-1700000000000" {
		t.Fatalf("conversationID = %q, want wb2api-1700000000000", conversationID)
	}

	seen := map[string]bool{}
	for i := 1; i <= CodeBuddyActivityReportCount; i++ {
		requestID := CodeBuddyActivityRequestID(conversationID, i)
		if seen[requestID] {
			t.Fatalf("requestID %q duplicated", requestID)
		}
		seen[requestID] = true
	}
	if len(seen) != CodeBuddyActivityReportCount {
		t.Fatalf("got %d distinct requestIDs, want %d", len(seen), CodeBuddyActivityReportCount)
	}
	// chat_5 门槛需要 5 条，常量被改小会直接让领养前置失效。
	if CodeBuddyActivityReportCount != 5 {
		t.Fatalf("ReportCount = %d, want 5 (chat_5 门槛要求同会话 5 次对话)",
			CodeBuddyActivityReportCount)
	}
}

// 路径固定、不进 realm 回落族（L8 的边界：/v2/report 不参与）。
func TestActivityReportPathIsFixedSingleCandidate(t *testing.T) {
	if CodeBuddyActivityReportPath != "/v2/report" {
		t.Fatalf("path = %q, want /v2/report", CodeBuddyActivityReportPath)
	}
}
