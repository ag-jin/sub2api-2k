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
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// CodeBuddy 上游链路专用逻辑（/v2/chat/completions，仅流式）：
//   - 请求体约束：强制 stream=true + stream_options.include_usage、developer→system；
//   - 专用身份头注入（X-User-Id / X-Enterprise-Id / X-Tenant-Id / X-Domain）；
//   - 客户端非流式请求的本地聚合（tool_calls 按 index 分片拼接）；
//   - 错误归类：确认性 401/403 → StatusError 提示重录，瞬态 → 通用 temp-unsched 链。

// codeBuddyUpstreamUserAgent 是 CodeBuddy 上游出站的自有 UA。
const codeBuddyUpstreamUserAgent = "codebuddy2openai/2.0"

// transformCodeBuddyRequestBody 对 CodeBuddy 上游体施加平台约束：
// developer→system 无需区分流式与否；kimi/zhipu 等共享链路不受影响（仅 codebuddy 调用）。
func transformCodeBuddyRequestBody(body []byte) ([]byte, error) {
	out := body
	arr := gjson.GetBytes(out, "messages")
	if arr.IsArray() {
		for i, m := range arr.Array() {
			if strings.EqualFold(strings.TrimSpace(m.Get("role").String()), "developer") {
				path := fmt.Sprintf("messages.%d.role", i)
				updated, err := sjson.SetBytes(out, path, "system")
				if err != nil {
					return nil, fmt.Errorf("normalize codebuddy developer role: %w", err)
				}
				out = updated
			}
		}
	}
	updated, err := sjson.SetBytes(out, "stream", true)
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

// applyCodeBuddyUpstreamHeaders 把 CodeBuddy 专用身份头写入上游请求。
// Authorization 由调用方统一注入（Bearer accessToken），在此不重复。
func applyCodeBuddyUpstreamHeaders(header http.Header, account *Account) {
	if account == nil || !account.IsCodeBuddy() {
		return
	}
	for k, v := range BuildCodeBuddyUpstreamHeaders(account) {
		header.Set(k, v)
	}
	header.Set("User-Agent", codeBuddyUpstreamUserAgent)
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
func (s *OpenAIGatewayService) handleCodeBuddyAccountUpstreamError(ctx context.Context, account *Account, statusCode int, upstreamMsg string) {
	if s == nil || account == nil || !account.IsCodeBuddy() {
		return
	}
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusForbidden {
		return
	}
	message := fmt.Sprintf(
		"CodeBuddy credentials rejected by upstream (HTTP %d)%s — re-import the auth JSON on the account",
		statusCode, codeBuddyRedactedMessage(upstreamMsg),
	)
	if err := s.accountRepo.SetError(ctx, account.ID, message); err != nil {
		logger.L().Warn("codebuddy account error state persist failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}
}

// codeBuddyRedactedMessage 对上游错误消息做统一脱敏（错误体可能回显请求内容）。
func codeBuddyRedactedMessage(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return ": " + logredact.RedactText(msg)
}
