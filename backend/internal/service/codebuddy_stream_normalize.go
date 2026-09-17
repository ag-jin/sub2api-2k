package service

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodeBuddy 上游（腾讯 /v2/chat/completions）的流式帧是本家"全量 delta"形状：
// 每个 chunk 的 delta 都带齐 content/reasoning_content/refusal 空串、tool_calls
// 空数组、function_call/extra_fields 空壳，而 OpenAI 协议约定"本帧无该类内容"应
// 表现为字段缺省或 null。CC 直转路径（streamRawChatCompletions）原本逐行透传，
// 这些空壳会在下游客户端解析器里越界命中：
//
//   - ZCode 的 openai-compatible 适配器判定 `delta.tool_calls != null` 即认为本帧
//     出现工具调用、并收尾当前 reasoning 块（**空数组同样成立**），于是思考被逐帧
//     切成独立块——2026-09-17 生产实证：1311 字符思维被切成 307 个
//     reasoning-start/reasoning-end 对，UI 表现为几百行"思考 · 持续了几秒"
//     （用户报"思考爆卡"）；
//   - `finish_reason:""` 不是 null，按"非空即已收尾"判定的客户端会误判收尾；
//   - 空串 content 会让"content 存在即开文本块"的客户端每帧新开一个空文本块。
//
// 归一化只做减法：删掉空壳字段、把空串 finish_reason 归一为 null；id/model/created/
// usage 与一切未知字段原样保留——raw 直转路径不重编码，避免丢失上游后续新增字段。

// codeBuddyStreamDropDeltaKeys 是 delta 里恒为空壳、对下游无意义且可能触发
// 客户端误判的供应商私有字段（extra_fields 恒为 null）。
var codeBuddyStreamDropDeltaKeys = []string{"extra_fields"}

// codeBuddyStreamEmptyStringDeltaKeys 是"空串即无内容"的文本字段：OpenAI 语义里
// 无内容应缺省或为 null。例外见 normalizeCodeBuddyChatStreamLine 对首帧 content 的保留。
var codeBuddyStreamEmptyStringDeltaKeys = []string{"content", "reasoning_content", "reasoning", "refusal"}

// normalizeCodeBuddyChatStreamLine 把一行上游 SSE（含 `data: ` 前缀）中每个
// choice 的 delta 归一为标准 OpenAI 帧形状，返回新行与是否发生改写。
//
// 非 data 行、[DONE]、空 payload、JSON 解析失败或无需改写的行原样返回（ok=false），
// 调用方可据此跳过写入替换。改写是幂等的：对归一化后的行再次调用返回 ok=false。
func normalizeCodeBuddyChatStreamLine(line string) (string, bool) {
	payload, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line, false
	}
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed == "[DONE]" {
		return line, false
	}
	data := []byte(trimmed)
	choices := gjson.GetBytes(data, "choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		return line, false
	}

	out := data
	changed := false
	apply := func(updated []byte, err error) {
		if err != nil {
			return
		}
		out = updated
		changed = true
	}
	finishReasonPath := func(i int) string { return "choices." + strconv.Itoa(i) + ".finish_reason" }
	deltaPath := func(i int) string { return "choices." + strconv.Itoa(i) + ".delta" }

	for i, choice := range choices.Array() {
		// tool_calls 只在真正携带工具时输出：空数组/空值都会让按 `!= null` 判定的
		// 客户端误以为本帧出现工具调用（并收尾思考块，见文件头注释）。
		if delta := choice.Get("delta"); delta.IsObject() {
			if tc := delta.Get("tool_calls"); tc.Exists() && (!tc.IsArray() || len(tc.Array()) == 0) {
				apply(sjson.DeleteBytes(out, deltaPath(i)+".tool_calls"))
			}
			for _, key := range codeBuddyStreamDropDeltaKeys {
				if delta.Get(key).Exists() {
					apply(sjson.DeleteBytes(out, deltaPath(i)+"."+key))
				}
			}
			// 首帧的 `content:""` 是标准形状（官方首帧即 role + 空 content），
			// 带 role 的帧保留 content 空串，其余空串文本字段一律删除。
			roleFrame := strings.TrimSpace(delta.Get("role").String()) != ""
			for _, key := range codeBuddyStreamEmptyStringDeltaKeys {
				v := delta.Get(key)
				if !v.Exists() || v.Type != gjson.String || v.String() != "" {
					continue
				}
				if key == "content" && roleFrame {
					continue
				}
				apply(sjson.DeleteBytes(out, deltaPath(i)+"."+key))
			}
			// 空壳 function_call（null，或 name/arguments 均空）删除；真实旧式函数调用保留。
			if fc := delta.Get("function_call"); fc.Exists() {
				emptyShell := fc.Type == gjson.Null ||
					(fc.IsObject() &&
						strings.TrimSpace(fc.Get("name").String()) == "" &&
						strings.TrimSpace(fc.Get("arguments").String()) == "")
				if emptyShell {
					apply(sjson.DeleteBytes(out, deltaPath(i)+".function_call"))
				}
			}
		}
		// finish_reason 空串不是合法取值（OpenAI 用 null 表示"本帧未收尾"），归一为 null。
		if fr := choice.Get("finish_reason"); fr.Exists() && fr.Type == gjson.String && fr.String() == "" {
			apply(sjson.SetBytes(out, finishReasonPath(i), nil))
		}
	}
	if !changed {
		return line, false
	}
	return "data: " + string(out), true
}
