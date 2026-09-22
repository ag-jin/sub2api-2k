package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// CodeBuddy 上游链路专用逻辑（/v2/chat/completions，仅流式）：
//   - 请求体规范化（对齐参考实现 workbuddy2api 的 payload/sanitize 两层）：
//     强制 stream=true、developer→system（键名大小写不敏感）、tool_choice 归一化为 string、
//     DeepSeek 思维链开关与 reasoning_content 回填（见 codebuddy_payload_rules.go）、
//     第三方提示词指纹净化（见 codebuddy_prompt_sanitize.go）；
//   - 出站头规范化（官方客户端形态：身份/归属/会话头族，见 codebuddy_upstream_identity.go）；
//   - 客户端非流式请求的本地聚合（tool_calls 按 index 分片拼接）；
//   - 错误归类：确认性 401/403 → StatusError 提示重录，瞬态 → 通用 temp-unsched 链。

// transformCodeBuddyRequestBody 对 CodeBuddy 上游体施加平台全部约束与规范化规则。
// account 用于 realm 相关规则（全局域 console 兜底 system）；传 nil 视为 CN。
//
// 管线顺序（对齐参考实现 payload.go 的 PrepareBodyOpt，并保留本仓的角色键名大小写语义）：
//  1. 角色归一化 developer→system（必须在净化之前：上游按归一化后的角色判定 content）；
//     1'. SDK 字段清理（verbosity / reasoning_summary，官方 SdkFieldCleanupRule）；
//  2. tool_choice 归一化（上游该字段是 string，对象形式 400 code 11101）；
//  3. DeepSeek 思维链开关 + 默认 effort 档；
//  4. DeepSeek assistant reasoning_content 回填（多轮一致性）；
//  5. 全局域 console 兜底 system 注入；
//  6. 提示词指纹净化（全部角色 content + tool_calls.arguments）；
//  7. 强制 stream=true；stream_options.include_usage 恒置 true。
//
// 与参考实现的两处有意差异（记录在案，非省略）：
//   - stream_options：参考实现仅在缺省时补，本仓恒置 true——用量统计驱动计费，
//     且官方客户端流式恒发该字段；
//   - reasoning_effort 按模型支持档位降级：依赖参考实现的模型目录元数据，本仓无该表，
//     不伪造（客户端显式档位一律透传；默认档 high 已在本端点三个 deepseek 模型实测通过）。
func transformCodeBuddyRequestBody(body []byte, account *Account) ([]byte, error) {
	out := body

	// 1. 角色归一化：键名大小写不敏感定位（上游按 Go encoding/json 语义解析消息对象，
	//    {"Role":"developer"} 与 {"role":"developer"} 等价触发 11128）。
	arr := gjson.GetBytes(out, "messages")
	if arr.IsArray() {
		for i, m := range arr.Array() {
			roleKey, role := codeBuddyMessageRoleField(m)
			if roleKey == "" || !strings.EqualFold(role, "developer") {
				continue
			}
			updated, err := sjson.SetBytes(out, fmt.Sprintf("messages.%d.%s", i, roleKey), "system")
			if err != nil {
				return nil, fmt.Errorf("normalize codebuddy developer role: %w", err)
			}
			out = updated
		}
	}

	// 2'. 官方 SdkFieldCleanupRule：剥离 SDK 内部字段（verbosity / reasoning_summary）。
	if cleaned := codebuddy.CleanupCodeBuddySdkFields(out); len(cleaned) != len(out) {
		out = cleaned
	}

	// 2. tool_choice 归一化。
	updated, err := codebuddy.NormalizeCodeBuddyToolChoice(out)
	if err != nil {
		return nil, err
	}
	out = updated

	// 3. DeepSeek 思维链开关（含默认 effort 档）。
	updated, err = codebuddy.InjectCodeBuddyDeepSeekThinking(out)
	if err != nil {
		return nil, err
	}
	out = updated

	// 4. DeepSeek assistant reasoning_content 回填。
	updated, err = codebuddy.BackfillCodeBuddyReasoningContent(out)
	if err != nil {
		return nil, err
	}
	out = updated

	// 5. 全局域 console 兜底 system。
	updated, err = codebuddy.EnsureCodeBuddyConsoleSystem(out, account)
	if err != nil {
		return nil, err
	}
	out = updated

	// 6. 提示词指纹净化（放在最后：覆盖前面步骤可能触及的文本）。
	updated, err = codebuddy.SanitizeCodeBuddyBodyFingerprints(out)
	if err != nil {
		return nil, err
	}
	out = updated

	// 7. 强制流式 + usage。
	updated, err = sjson.SetBytes(out, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("force codebuddy stream: %w", err)
	}
	out = updated
	updated, err = sjson.SetBytes(out, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("force codebuddy stream usage: %w", err)
	}
	return updated, nil
}

// codeBuddyMessageRoleField 大小写不敏感地取出消息对象的角色字段名与值。
// 上游对键名大小写不敏感（Go encoding/json 语义），归一化必须按同样语义定位，
// 返回实际键名以便就地改写（不新造 "role" 键，避免同对象出现两个大小写不同的角色键）。
func codeBuddyMessageRoleField(msg gjson.Result) (key string, value string) {
	msg.ForEach(func(k, v gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(k.String()), "role") {
			key, value = k.String(), strings.TrimSpace(v.String())
			return false
		}
		return true
	})
	return key, value
}

type codeBuddyAggregatedToolCall struct {
	id       string
	callType string
	name     string
	args     string
}

// aggregateCodeBuddyCCResponse 完整消费一段 CodeBuddy（OpenAI Chat 协议）SSE 响应体，
// 聚合为单个非流式 Chat Completions JSON 对象（语义与 codebuddy2api
// converter._collect_stream 对齐）：
//   - delta.content 顺序拼接；
//   - tool_calls 按 index 分片合并（id/name 覆盖式，arguments 逐片追加）；
//   - model 取上游回显实值；
//   - finish_reason 取最末非空值；
//   - usage 取最末 chunk（覆盖语义）。
func aggregateCodeBuddyCCResponse(r io.Reader) ([]byte, OpenAIUsage, string, error) {
	return aggregateCodeBuddyCCResponseWithFallback(r, "")
}

// aggregateCodeBuddyCCResponseWithFallback 在上游 SSE 未回显 model 时以
// fallbackModel（客户端请求 model，R1-D6）兜底聚合结果的 model 字段。
func aggregateCodeBuddyCCResponseWithFallback(r io.Reader, fallbackModel string) ([]byte, OpenAIUsage, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), defaultMaxLineSize)

	var (
		contentParts  []string
		toolCalls     map[int]codeBuddyAggregatedToolCall
		usage         OpenAIUsage
		finishReason  string
		upstreamModel string
		id            string
		created       int64
	) // usage 取末 chunk 覆盖语义；缺失时不伪造数值（全 0 兜底，不阻塞主链路）。
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			break
		}
		if m := gjson.Get(payload, "model").String(); m != "" {
			upstreamModel = m
		}
		if v := gjson.Get(payload, "id").String(); v != "" {
			id = v
		}
		if v := gjson.Get(payload, "created").Int(); v > 0 {
			created = v
		}
		if u := extractCCStreamUsage(payload); u != nil {
			usage = *u // 末 chunk 覆盖语义
		}
		choices := gjson.Get(payload, "choices")
		if !choices.IsArray() {
			continue
		}
		for _, choice := range choices.Array() {
			if fr := strings.TrimSpace(choice.Get("finish_reason").String()); fr != "" {
				finishReason = fr
			}
			if content := choice.Get("delta.content"); content.Type == gjson.String && content.String() != "" {
				contentParts = append(contentParts, content.String())
			}
			tcs := choice.Get("delta.tool_calls")
			if !tcs.IsArray() {
				continue
			}
			for _, tc := range tcs.Array() {
				idx := int(tc.Get("index").Int())
				if toolCalls == nil {
					toolCalls = map[int]codeBuddyAggregatedToolCall{}
				}
				slot := toolCalls[idx]
				if v := tc.Get("id").String(); v != "" {
					slot.id = v
				}
				if v := tc.Get("type").String(); v != "" {
					slot.callType = v
				}
				if v := tc.Get("function.name").String(); v != "" {
					slot.name = v
				}
				if args := tc.Get("function.arguments"); args.Type == gjson.String {
					slot.args += args.String()
				}
				toolCalls[idx] = slot
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, usage, upstreamModel, fmt.Errorf("read codebuddy upstream stream: %w", err)
	}

	message := map[string]any{"role": "assistant"}
	message["content"] = strings.Join(contentParts, "")
	if len(toolCalls) > 0 {
		idxs := make([]int, 0, len(toolCalls))
		for idx := range toolCalls {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		merged := make([]map[string]any, 0, len(idxs))
		for _, idx := range idxs {
			slot := toolCalls[idx]
			function := map[string]any{"arguments": slot.args}
			if slot.name != "" {
				function["name"] = slot.name
			}
			callType := slot.callType
			if callType == "" {
				callType = "function"
			}
			merged = append(merged, map[string]any{
				"index":    idx,
				"id":       slot.id,
				"type":     callType,
				"function": function,
			})
		}
		message["tool_calls"] = merged
		if finishReason == "" {
			finishReason = "tool_calls"
		}
	} else if finishReason == "" {
		finishReason = "stop"
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}
	if strings.TrimSpace(fallbackModel) != "" && strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = fallbackModel
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   upstreamModel,
		"choices": []any{choice},
		"usage": map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens": usage.InputTokens + usage.OutputTokens +
				usage.CacheReadInputTokens + usage.CacheCreationInputTokens,
		},
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, OpenAIUsage{}, upstreamModel, err
	}
	return out, usage, upstreamModel, nil
}

// handleCodeBuddyAccountUpstreamError 登记 CodeBuddy 账号对确认性 401/403 的错误态。
// 上游 401 的“重读 DB 凭据→重试一次”兜底已由 sendCCUpstreamRequest 包装实现；
// 重试仍被拒绝（或 token 未变化）时走到这里：判死 → StatusError + 提示重录。
// 上游业务码命中码表时（A4，仅管理面），ErrorMessage 追加可读说明；
// 网关客户端 message 不改（精确匹配资产）。
func (s *OpenAIGatewayService) handleCodeBuddyAccountUpstreamError(ctx context.Context, account *Account, statusCode int, upstreamMsg string, upstreamBizCode ...int) {
	if s == nil || account == nil || !account.IsCodeBuddy() {
		return
	}
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusForbidden {
		return
	}
	bizCode := 0
	if len(upstreamBizCode) > 0 {
		bizCode = upstreamBizCode[0]
	}
	message := fmt.Sprintf(
		"CodeBuddy credentials rejected by upstream (HTTP %d)%s%s — re-import the auth JSON on the account",
		statusCode, codeBuddyRedactedMessage(upstreamMsg), codeBuddyBizCodeSuffix(bizCode),
	)
	if err := s.accountRepo.SetError(ctx, account.ID, message); err != nil {
		logger.L().Warn("codebuddy account error state persist failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}
}

// codeBuddyBizCodeSuffix 已知业务码 → 追加 "（hint）" 说明（未知码返回空串）。
func codeBuddyBizCodeSuffix(code int) string {
	hint := codebuddy.CodeBuddyBizCodeHint(code)
	if hint == "" {
		return ""
	}
	return "（" + hint + "）"
}

// codeBuddyRedactedMessage 对上游错误消息做统一脱敏（错误体可能回显请求内容）。
func codeBuddyRedactedMessage(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return ": " + logredact.RedactText(msg)
}
