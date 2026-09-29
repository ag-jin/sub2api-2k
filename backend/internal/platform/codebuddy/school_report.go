package codebuddy

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"time"
)

// 开学季（F 通道）的**事件形状**与伙伴标识。
//
// ## 为什么这些事件不复用 CodeBuddyActivityEvent
//
// 开学季上报与 A5 的对话活跃上报**打到同一个端点**（`POST /v2/report`），
// 但事件字段集不同：开学季事件必须带 `activityId`（不带则服务端不关联任务，
// 200 静默丢弃）以及一组小程序/桌面端的身份字段（source / ideName / extName …），
// 而 A5 的事件结构里根本没有这些字段。
//
// 折中做法有两种，都不好：
//   - 往 A5 的结构体上加字段 → 那些字段对 A5 永远是空，变成"两个用途共用一张
//     总是对不上的表"，且 A5 的 `ValidateCodeBuddyActivityEvent` 语义会被污染；
//   - 用 map[string]any 拼 → 丢类型安全，字段名拼错编译期发现不了。
//
// 所以这里为开学季单独定义结构体。两者共享的只有"目标端点"与"鉴权方式"，
// 那部分在 service 层复用（`sendCodeBuddySchoolReport`）。
//
// ## 依据
//
// 参考实现 `scripts/school_open_day_2026.py` 的 `mini_chat_event` /
// `expert_actual_use_event` / `desktop_chat_sequence`（workbuddy2api @ 2967a48）。

// CodeBuddySchoolActivityID 开学季活动的 activityId。
//
// ⚠️ 这是**关联任务的唯一纽带**：事件必须带它，否则服务端收到 200 但不把事件
// 计入任何任务进度（参考实现原文「必须带 activityId=school_open_day_2026，
// 否则服务端不关联任务」）。
const CodeBuddySchoolActivityID = "school_open_day_2026"

// CodeBuddySchoolExpertCategory BackToSchool 专家分类 id。
// 带数字前缀（`16-`），参考实现原文注明「list API 不做归一化」——照抄，别去掉。
const CodeBuddySchoolExpertCategory = "16-BackToSchool"

// CodeBuddySchoolExpertListPath 专家列表端点（在 cloud agent 域上，非 school 前缀）。
const CodeBuddySchoolExpertListPath = "/v2/operation-platform/market/expert/list"

// CodeBuddySchoolClientPlatform 小程序请求头 `X-Client-Platform`（见 growth_tasks.go 同名字段）。
//
// 保留在本文件不重复定义：字面量只有一份，在 growth_tasks.go。

// CodeBuddySchoolReportEvent 开学季上报事件（`chat_request_send` / `expert_actual_use`）。
//
// 字段按参考实现逐字对齐。用 `omitempty` 之外**不加**字段：
// 上游按字段形状识别事件来源，凭空多字段比少字段更容易被风控标记。
type CodeBuddySchoolReportEvent struct {
	EventCode   string `json:"eventCode"`
	Timestamp   int64  `json:"timestamp"`
	ReportDelay int    `json:"reportDelay"`
	Source      string `json:"source"`
	IDEName     string `json:"ideName"`
	IDEType     string `json:"ideType"`
	ExtName     string `json:"extName"`
	ExtVersion  string `json:"extVersion"`
	Mode        string `json:"mode,omitempty"`
	// ConversationID / RequestID 同值（参考实现 mini_chat_event 里两者都是 conv）。
	ConversationID string `json:"conversationId"`
	RequestID      string `json:"requestId"`
	InputLength    int    `json:"inputLength"`
	// ActivityID 见 CodeBuddySchoolActivityID 的说明——**少了它事件不计入任务**。
	ActivityID          string `json:"activityId"`
	MentionContexts     []any  `json:"mentionContexts"`
	MentionContextCount int    `json:"mentionContextCount"`
	UserID              string `json:"userId"`

	// --- expert_actual_use 专属字段（chat_request_send 不使用）---
	//
	// 同一个结构体承载两种事件：expert 事件比 chat 事件多这几个字段，
	// 用 omitempty 让它们在 chat 事件里不出现。为 expert 另建结构体会让
	// "公共字段"（source/ideName/userId/activityId）两处各写一遍并漂移。
	UserNickname   string `json:"userNickname,omitempty"`
	MachineID      string `json:"machineId,omitempty"`
	OS             string `json:"os,omitempty"`
	OSVersion      string `json:"osVersion,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Timezone       string `json:"timezone,omitempty"`
	ExpertID       string `json:"id,omitempty"`
	ExpertName     string `json:"name,omitempty"`
	ExpertTitle    string `json:"expertTitle,omitempty"`
	ExpertAction   string `json:"type,omitempty"`
	CharacterCount int    `json:"characterCount,omitempty"`
	ExpertType     string `json:"expertType,omitempty"`
}

// NewCodeBuddySchoolMiniChatEvent 构造一条小程序域对话事件（点亮 `chat_3_times`）。
//
// `conversationID` 由调用方生成（每次点亮用新会话，避免跨任务串味）。
func NewCodeBuddySchoolMiniChatEvent(userID, conversationID string, now time.Time) CodeBuddySchoolReportEvent {
	return CodeBuddySchoolReportEvent{
		EventCode:           "chat_request_send",
		Timestamp:           now.UnixMilli(),
		ReportDelay:         0,
		Source:              "mini_program",
		IDEName:             "wx_app_cloud",
		IDEType:             "WorkBuddy_MP",
		ExtName:             "workbuddy-mp",
		ExtVersion:          "SaaS",
		Mode:                "chat",
		ConversationID:      conversationID,
		RequestID:           conversationID,
		InputLength:         12,
		ActivityID:          CodeBuddySchoolActivityID,
		MentionContexts:     []any{},
		MentionContextCount: 0,
		UserID:              userID,
	}
}

// NewCodeBuddySchoolExpertEvent 构造一条 `expert_actual_use` 事件（点亮 `expert_use`）。
//
// 参考实现原文「服务端不校验真实性」——但字段形状仍逐字对齐（含 android/arm64
// 这类指纹），因为风控看的是形状分布，不是单条真实性。
func NewCodeBuddySchoolExpertEvent(
	userID, nickname, expertID, expertName, conversationID string,
	machineID string,
	now time.Time,
) CodeBuddySchoolReportEvent {
	return CodeBuddySchoolReportEvent{
		EventCode:      "expert_actual_use",
		Timestamp:      now.UnixMilli(),
		ReportDelay:    0,
		Source:         "mini_program",
		IDEName:        "wx_app_cloud",
		IDEType:        "WorkBuddy_MP",
		ExtName:        "workbuddy-mp",
		ExtVersion:     "SaaS",
		MachineID:      machineID,
		OS:             "android",
		OSVersion:      "14",
		Arch:           "arm64",
		Timezone:       "Asia/Shanghai",
		UserID:         userID,
		UserNickname:   nickname,
		ConversationID: conversationID,
		ActivityID:     CodeBuddySchoolActivityID,
		ExpertID:       expertID,
		ExpertName:     expertID,
		ExpertTitle:    expertName,
		ExpertAction:   "send_message",
		// CharacterCount 参考实现写死 12（"发送了 12 个字符"的伪造事实）。
		CharacterCount: 12,
		ExpertType:     "agent",
	}
}

// CodeBuddySchoolExpertFallback 专家列表拉取失败时的回落项（id → 展示名）。
//
// 参考实现同款回落（`SCHOOL_EXPERT_FALLBACK`）：列表端点抽风时不该让整个
// `expert_use` 任务失败——用已知的开学期专家兜底，事件照样能点亮。
//
// 取第一个可用的（map 遍历顺序不稳定，所以这里用有序切片）。
var CodeBuddySchoolExpertFallback = []CodeBuddySchoolExpert{
	{ID: "ex_jB0dyFIQJEWa", Title: "论小舟"},
	{ID: "ex_lQjkerakvIex", Title: "大学英语学习教练"},
}

// CodeBuddySchoolExpert 一个开学季专家（点亮 expert_use 用）。
type CodeBuddySchoolExpert struct {
	ID    string
	Title string
}

// CodeBuddySchoolDerivedMachineID 由 uid 稳定派生设备标识（参考实现 `derive_id`）。
//
// 用 md5(salt:uid) 前 36 位而不是随机值：同一账号每次上报的 machineId 必须一致，
// 否则"同一个用户来自不同设备"本身就是风控信号。salt 固定为 "machine"，
// 与参考实现同值。
func CodeBuddySchoolDerivedMachineID(userID string) string {
	sum := md5.Sum([]byte("machine:" + userID))
	return hex.EncodeToString(sum[:])[:36]
}

// CodeBuddySchoolConversationID 生成开学季上报用的会话标识（前缀区分用途）。
func CodeBuddySchoolConversationID(prefix string, now time.Time) string {
	if prefix == "" {
		prefix = "wbschool"
	}
	return fmt.Sprintf("%s-%d", prefix, now.UnixMilli())
}

// CodeBuddySchoolDesktopUserAgent 桌面上报的 User-Agent（参考实现 `DESKTOP_UA` 原值）。
const CodeBuddySchoolDesktopUserAgent = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

// CodeBuddySchoolDesktopSequenceEvent 桌面 6 连里的一条事件。
//
// 用 `map[string]any` 而不是结构体：这条链路里含 `codebuddy.session_id` 与
// `codebuddy.conversation_request_id` 两个**带点的键**——Go 的结构体标签无法
// 表达（encoding/json 会把标签原样输出，带点是合法 JSON 键，但字段名可读性差
// 且易拼错）。用 map 逐字照抄参考实现的字段表更不容易错。
//
// 类型安全由**唯一构造点**保证：所有 map 都在 NewCodeBuddySchoolDesktopSequence
// 内部产生，外部无法绕过；字段集有单测钉住。
type CodeBuddySchoolDesktopSequenceEvent = map[string]any

// NewCodeBuddySchoolDesktopSequence 构造桌面端成功对话 6 连事件链。
//
// 事件顺序（参考实现 `desktop_chat_sequence`，顺序不可调换）：
//
//	agent_task_created → chat_message_send → chat_request_send →
//	chat_message_response → chat_message_status → chat_request_response
//
// ⚠️ **必须整体上报**：一次 6 连 = 一次桌面对话。拆开发送不点亮
// （参考实现原文「必须整体上报」）。
//
// 注：参考实现还往每条事件注入了 20 项的桌面指纹（ideVersion / commit /
// releaseDate / cpuCores 等写死的"这台机器"的快照）。这里**注入其中可稳定
// 复现的部分**（machineId / sessionId 由 uid 派生、OS 指纹、时区）——
// 逐字照抄那组写死的版本号（如 releaseDate=1789036585355）价值不大：
// 它们是随客户端版本变化的值，写死反而会在上游更新后显得更异常。
func NewCodeBuddySchoolDesktopSequence(
	userID, conversationID string,
	now time.Time,
) []CodeBuddySchoolDesktopSequenceEvent {
	ts := now.UnixMilli()
	requestID := conversationID
	messageID := conversationID
	machineID := CodeBuddySchoolDerivedMachineID(userID)

	// fingerprint 注入每个事件的稳定身份字段。
	fingerprint := func() map[string]any {
		return map[string]any{
			"timezone":       "Asia/Shanghai",
			"reportDelay":    2000,
			"userId":         userID,
			"product":        "SaaS",
			"ideName":        "WorkBuddy",
			"ideType":        "WorkBuddy",
			"machineId":      machineID,
			"sessionId":      machineID,
			"extName":        "workbuddy-desktop",
			"os":             "win32",
			"arch":           "x64",
			"osVersion":      "10.0.26220",
			"cpuCores":       20,
			"memorySize":     24,
			"timestamp":      ts,
			"presentAt":      ts,
			"activityId":     CodeBuddySchoolActivityID,
			"conversationId": conversationID,
		}
	}
	// merge 把 base 与 extra 合成一条事件，extra 覆盖 base 同名键。
	merge := func(eventCode string, extra map[string]any) CodeBuddySchoolDesktopSequenceEvent {
		event := fingerprint()
		event["eventCode"] = eventCode
		for k, v := range extra {
			event[k] = v
		}
		return event
	}

	return []CodeBuddySchoolDesktopSequenceEvent{
		merge("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": "fast-model", "requestModelName": "fast-model",
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"messageId": messageID, "buddyId": "", "buddyName": "",
		}),
		merge("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		merge("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		merge("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": "fast-model",
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": ts, "traceId": requestID,
			"rootRequestId": requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		merge("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		merge("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}
