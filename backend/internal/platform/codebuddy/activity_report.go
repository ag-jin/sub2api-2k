package codebuddy

import (
	"encoding/json"
	"time"
)

// 对话活跃上报（growth 域 `POST {billingBase}/v2/report`）的事件形状。
//
// 为什么单独成篇、且**逐字段照抄**：参考实现（workbuddy2api
// `internal/upstream/report.go`）明确记录——必须照抄客户端 `chat_request_send`
// 事件的完整形状，**勿用最小 3 字段**，防上游后续加严。所以我方也不"精简"：
// 字段名与类型一一对应，宁可留一堆零值字段。
//
// 本文件只承载**纯数据与序列化**，不依赖 service 类型（依赖方向仍是
// service → platform/codebuddy 单向）。账号 uid 由调用方传入，不在本包读凭据。
//
// ⚠️ 最容易踩的坑（参考实现原文，实测结论）：事件**必须带 userId**（= 账号 uid）。
// 缺失时上游返回 200 但**静默丢弃**——看起来成功、日志全绿、实际一次都没上报。
// 因此：
//   - `UserID` 是必填；序列化前由 `ValidateActivityEvent` 拦一道；
//   - 上报成功后**必须回读 streak 自检**（见 service 侧调度器），不能只看 code=0。

// CodeBuddyActivityReportPath 活跃上报通道（实测路径，单元素）。
//
// ⚠️ 不进 realm 路径回落族：参考实现注释明确——`/billing/meter/*` 族的
// "global 先无 /v2 再回落 /v2"规律**不适用于 /v2/report**（client.go:880 原文：
// "report /v2/report 不参与"）。所以这里是**单一固定路径**，别照 L8 去加回落候选。
const CodeBuddyActivityReportPath = "/v2/report"

// CodeBuddyActivityEventCode 事件码：对话请求发送。
const CodeBuddyActivityEventCode = "chat_request_send"

// CodeBuddyActivityReportCount 单账号单轮上报条数。
//
// 为什么是 5：领养（`/activity/growth/buddy/first`）要求 `chat_5` 门槛——
// 即**同一会话内 5 次对话**才算达标。少于 5 条刷不满前置，领养直接 400。
const CodeBuddyActivityReportCount = 5

// CodeBuddyActivityMode 事件 mode 字段值（照抄参考实现）。
const CodeBuddyActivityMode = "craft"

// CodeBuddyActivityDefaultModelID / Name 事件里的模型字段。
// 取值照抄参考实现，非真实调用模型——该端点只记活跃，不校验一致性。
const (
	CodeBuddyActivityDefaultModelID   = "deepseek-v4-flash"
	CodeBuddyActivityDefaultModelName = "DeepSeek V4 Flash"
)

// CodeBuddyActivityInputLength 事件 inputLength（照抄参考实现常量 12）。
const CodeBuddyActivityInputLength = 12

// CodeBuddyActivityEvent 客户端 `chat_request_send` 事件的完整形状。
//
// 字段顺序与参考实现的 struct 保持一致，便于逐字段比对（改字段时对照成本最低）。
// 全部字段都走 JSON 输出（无 omitempty）——上游按固定形状解析，缺字段比零值更可疑。
type CodeBuddyActivityEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// NewCodeBuddyActivityEvent 构造一条 `chat_request_send` 事件。
//
// userID 必填（缺失即"200 但静默丢弃"，见文件头）；conversationID / requestID
// 由调用方给：同一会话的 N 条**共用同一 conversationID**、requestID 各自独立
// （模拟同会话多轮，服务端不校验一致性）。
func NewCodeBuddyActivityEvent(userID, conversationID, requestID string, now time.Time) CodeBuddyActivityEvent {
	ms := now.UnixMilli()
	return CodeBuddyActivityEvent{
		EventCode:             CodeBuddyActivityEventCode,
		Timestamp:             ms,
		ReportDelay:           0,
		Mode:                  CodeBuddyActivityMode,
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           CodeBuddyActivityInputLength,
		RequestModelID:        CodeBuddyActivityDefaultModelID,
		RequestModelName:      CodeBuddyActivityDefaultModelName,
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		MaxToken:              0,
		MaxSteps:              0,
		Temperature:           0,
		MaxRetries:            0,
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		ExpertID:              "",
		RecommendID:           "",
		SkillID:               "",
		SkillCount:            0,
		TotalCount:            0,
		FileURI:               "",
		PresentAt:             ms,
		TraceID:               "",
		RootRequestID:         conversationID,
		ParentConversationID:  conversationID,
		AgentName:             "default",
		AgentType:             "conversation",
		UserID:                userID,
	}
}

// CodeBuddyActivityRequestID 生成本轮某条的 requestID。
// 形态 `<conversationID>-r<i>`（i 从 1 起），与参考实现一致。
func CodeBuddyActivityRequestID(conversationID string, index int) string {
	return conversationID + "-r" + codeBuddyIntToString(index)
}

// CodeBuddyActivityConversationID 生成本轮会话标识（形态 `wb2api-<ms>`）。
// 无需真实会话——服务端不校验 conversationId 与真实会话的一致性。
func CodeBuddyActivityConversationID(now time.Time) string {
	return "wb2api-" + codeBuddyInt64ToString(now.UnixMilli())
}

// MarshalActivityEvents 把事件序列序列化成上游要求的请求体。
//
// ⚠️ 请求体是 **JSON 数组**（`[{...}]`），不是单个对象——照抄参考实现
// `json.Marshal([]chatRequestEvent{ev})`。发成裸对象会被上游按 schema 拒绝，
// 而错误可能又是 200 静默丢弃，排查成本极高。
func MarshalActivityEvents(events []CodeBuddyActivityEvent) ([]byte, error) {
	if events == nil {
		events = []CodeBuddyActivityEvent{}
	}
	return json.Marshal(events)
}

// ValidateCodeBuddyActivityEvent 校验事件可否发送。
//
// 目前只拦一件事，但这一件是致命的：**UserID 为空**。
// 上游对缺 userId 的事件返回 200 并静默丢弃，若不在这里拦住，调用方会看到
// "上报成功"而实际毫无效果——这正是本任务书点名的最阴的坑。
func ValidateCodeBuddyActivityEvent(event CodeBuddyActivityEvent) bool {
	return event.UserID != ""
}

func codeBuddyIntToString(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

func codeBuddyInt64ToString(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
