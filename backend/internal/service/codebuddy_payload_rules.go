package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodeBuddy 出站请求体规范化规则（对齐参考实现 workbuddy2api internal/upstream/payload.go）。
//
// 与"指纹净化"（codebuddy_prompt_sanitize.go）的分工：本文件是**协议兼容**改写
// （补上游字段类型/白名单/开关要求），净化是**内容脱敏**；两者独立，互不依赖。

// cleanupCodeBuddySdkFields 对齐官方 SdkFieldCleanupRule：出站体删除 SDK 内部字段
// `verbosity` 与 `reasoning_summary`（官方 CLI 的 ModelRequestProcessor 首个规则，
// matches 恒真）。这两个字段由客户端 SDK 生成、上游不消费，官方一律剥离。
func cleanupCodeBuddySdkFields(out []byte) []byte {
	for _, field := range []string{"verbosity", "reasoning_summary"} {
		if gjson.GetBytes(out, field).Exists() {
			out = deleteCodeBuddyJSONField(out, field)
		}
	}
	return out
}

// normalizeCodeBuddyToolChoice 把 tool_choice 归一化为上游要求的 string 形态。
//
// 上游该字段是 Go string 类型，**对象形式直接 400 code 11101**
// （2026-09-14 本端点实测：`{"type":"function",…}` 与 `{"type":"auto"}` 均
// `cannot unmarshal object into Go struct field Request.tool_choice`）。规则对齐参考实现：
//   - "none" / {"type":"none"} → 删 tool_choice，并删 tools/functions（本地抑制工具）
//   - {"type":"auto"|"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"（无名字段时回落 "auto"）
//   - 其它对象/非标量 → 删 tool_choice
func normalizeCodeBuddyToolChoice(out []byte) ([]byte, error) {
	tc := gjson.GetBytes(out, "tool_choice")
	if !tc.Exists() {
		return out, nil
	}
	suppressTools := func(body []byte) []byte {
		body = deleteCodeBuddyJSONField(body, "tools")
		return deleteCodeBuddyJSONField(body, "functions")
	}
	setString := func(body []byte, v string) ([]byte, error) {
		updated, err := sjson.SetBytes(body, "tool_choice", v)
		if err != nil {
			return nil, fmt.Errorf("normalize codebuddy tool_choice: %w", err)
		}
		return updated, nil
	}

	switch {
	case tc.Type == gjson.String:
		if strings.EqualFold(strings.TrimSpace(tc.String()), "none") {
			return suppressTools(deleteCodeBuddyJSONField(out, "tool_choice")), nil
		}
		return out, nil
	case tc.IsObject():
		typ := strings.ToLower(strings.TrimSpace(tc.Get("type").String()))
		switch typ {
		case "none":
			return suppressTools(deleteCodeBuddyJSONField(out, "tool_choice")), nil
		case "auto", "required":
			return setString(out, typ)
		case "function":
			name := strings.TrimSpace(tc.Get("function.name").String())
			if name == "" {
				name = strings.TrimSpace(tc.Get("name").String())
			}
			if name == "" {
				return setString(out, "auto")
			}
			return setString(out, name)
		default:
			return deleteCodeBuddyJSONField(out, "tool_choice"), nil
		}
	default:
		return deleteCodeBuddyJSONField(out, "tool_choice"), nil
	}
}

// deleteCodeBuddyJSONField 删除顶层字段（失败时原样返回：删字段失败不比保留字段更安全）。
func deleteCodeBuddyJSONField(body []byte, field string) []byte {
	updated, err := sjson.DeleteBytes(body, field)
	if err != nil {
		return body
	}
	return updated
}

// codeBuddyModelDefaultEfforts 服务端目录 `reasoning.effort` 快照（2026-09-14 用真账号
// GET {base}/console/enterprises/personal/models 实测）。官方客户端在用户未选档位时，
// 请求层用该默认档兜底（其注释：「没具体 effort → model-interceptors 走 modelConfig.defaultEffort
// 兜底」）。未列出的模型 = 目录未声明默认档 → 本仓不注入档位（开不开思考由客户端决定）。
//
// 该端点在真机可实时获取（CN /console/…、global /v2/…）；本表是快照，新增模型需更新，
// 或后续改为带 TTL 的目录拉取（与参考实现 FetchModels + realm 分层缓存同构）。
var codeBuddyModelDefaultEfforts = map[string]string{
	"auto":                 "high",
	"hy3":                  "high",
	"deepseek-v4-flash":    "high",
	"deepseek-v4.1-flash":  "high",
	"deepseek-v4-pro":      "high",
	"glm-5.2":              "medium",
	"glm-5.1":              "medium",
	"glm-5v-turbo":         "medium",
	"glm-4.6v":             "medium",
	"minimax-m3":           "medium",
	"minimax-m2.5":         "medium",
	"kimi-k3-1":            "medium",
	"kimi-k2.7":            "medium",
	"kimi-k2.6":            "medium",
	"kimi-k2.5":            "medium",
	"kimi-k2-thinking":     "medium",
	"deepseek-v3-2-volc":   "medium",
	"hunyuan-2.0-thinking": "medium",
}

// codeBuddyThinkingLevels 官方客户端的六个思考档位（app 内 EFFORT_ORDER 与
// VALID_REASONING_EFFORTS 双处一致：minimal < low < medium < high < xhigh < max）。
var codeBuddyThinkingLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// isCodeBuddyThinkingLevel 是否为合法档位值。
func isCodeBuddyThinkingLevel(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, lvl := range codeBuddyThinkingLevels {
		if v == lvl {
			return true
		}
	}
	return false
}

// isCodeBuddyDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
// 对齐官方 thinkingFormat:"deepseek" 的判定口径（该格式才写 thinking.type）。
func isCodeBuddyDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectCodeBuddyDeepSeekThinking 思考开关与档位（逐条对齐官方 CLI 的请求改写规则链）。
//
// 官方实现（app.asar.unpacked/cli/dist/codebuddy.js 的 ModelRequestProcessor 规则链，2026-09-14 读源码）：
//   - ThinkingFormatTranslatorRule · case "deepseek"：
//     `thinkingEnabled || reasoning_effort !== undefined` → 若 `thinking.type` 未定义则置
//     `"enabled"`；否则（关思考且无档位）→ `thinking={type:"disabled"}` 并删除 reasoning_effort。
//     即**deepseek 出站恒带 thinking.type，且档位与 thinking.type 并存**。
//   - ThinkingEffortTranslatorRule / LegacyXhighFallbackRule：按 thinkingLevelMap 翻译档位
//     （null → 删字段；无映射时 `xhigh`→`high`）。CN 后端的目录只给 supportedEfforts、不给 map，
//     本仓按"不翻译"处理（本端点实测六档均可直接使用）。
//   - 默认档来自模型目录 reasoning.effort（见 codeBuddyModelDefaultEfforts）。
//
// 本端点实测补充（deepseek-v4.1-flash，每格 3 次 reasoning_tokens）：单独 thinking.type
// 任何取值都不开思考（0），`reasoning_effort` 才是开关；`type="disabled"` 压过 effort 真关闸。
//
// 规则（绝不覆盖客户端显式意图）：
//   - type = disabled → 尊重，并删 reasoning_effort（snake/camel）；
//   - 客户端已给 reasoning_effort → 原样保留；deepseek 且 type 未定义时补 type="enabled"；
//   - 客户端把档位放在 thinking.type 里（六档之一）→ 转写成 reasoning_effort（尊重选择且真正开启）；
//   - 其余：有目录默认档 → 注入该档；deepseek 无档位时兜底 "high"；deepseek 补 type="enabled"；
//   - 既非 deepseek、目录也无默认档 → 零改动。
func injectCodeBuddyDeepSeekThinking(out []byte) ([]byte, error) {
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(out, "model").String()))
	if model == "" {
		return out, nil
	}
	isDeepSeek := isCodeBuddyDeepSeekModel(model)
	defaultEffort, hasDefault := codeBuddyModelDefaultEfforts[model]
	if !isDeepSeek && !hasDefault {
		return out, nil
	}
	thinking := gjson.GetBytes(out, "thinking")
	typ := ""
	if thinking.IsObject() {
		typ = strings.TrimSpace(thinking.Get("type").String())
	}
	if strings.EqualFold(typ, "disabled") {
		out = deleteCodeBuddyJSONField(out, "reasoning_effort")
		return deleteCodeBuddyJSONField(out, "reasoningEffort"), nil
	}
	level := ""
	switch {
	case hasCodeBuddyReasoningEffort(out):
		level = "" // 客户端显式档位：原样保留，不改写
	case isCodeBuddyThinkingLevel(typ):
		level = strings.ToLower(typ) // 客户端把档位放在 type 里 → 转写成 effort
	case isDeepSeek:
		level = codeBuddyDeepSeekFallbackEffort
		if hasDefault {
			level = defaultEffort
		}
	case hasDefault:
		level = defaultEffort
	}
	if level != "" {
		// 官方 LegacyXhighFallbackRule：模型没有 thinkingLevelMap 时 `xhigh` → `high`。
		// CN 后端的目录只给 supportedEfforts、不给 map（实测六个 CN 模型 id 均无 map 条目），
		// 故本链路一律按"无 map"处理。
		if strings.EqualFold(level, "xhigh") {
			level = "high"
		}
		updated, err := sjson.SetBytes(out, "reasoning_effort", level)
		if err != nil {
			return nil, fmt.Errorf("set codebuddy reasoning effort: %w", err)
		}
		out = updated
	}
	// 客户端已有档位时同样适用官方改写（LegacyXhighFallbackRule 不看来源）。
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(out, "reasoning_effort").String()), "xhigh") {
		updated, err := sjson.SetBytes(out, "reasoning_effort", "high")
		if err != nil {
			return nil, fmt.Errorf("fallback codebuddy xhigh effort: %w", err)
		}
		out = updated
	}
	// deepseek 出站恒带 thinking.type（官方规则链口径）：type 未定义时补 "enabled"。
	if isDeepSeek && typ == "" {
		updated, err := sjson.SetBytes(out, "thinking.type", "enabled")
		if err != nil {
			return nil, fmt.Errorf("set codebuddy thinking type: %w", err)
		}
		out = updated
	}
	return out, nil
}

// codeBuddyDeepSeekFallbackEffort 目录缺默认档时的 deepseek 兜底档（与目录实测值一致）。
const codeBuddyDeepSeekFallbackEffort = "high"

// codeBuddyThinkingEnabled 出站体是否处于"开思考"状态：type 非 disabled 且带档位。
// 用于 reasoning_content 回填触发条件（官方：thinkingEnabled || 存在 reasoning 痕迹）。
func codeBuddyThinkingEnabled(out []byte) bool {
	if th := gjson.GetBytes(out, "thinking"); th.IsObject() {
		if strings.EqualFold(strings.TrimSpace(th.Get("type").String()), "disabled") {
			return false
		}
	}
	return hasCodeBuddyReasoningEffort(out)
}

// hasCodeBuddyReasoningEffort 客户端是否已给 reasoning_effort（snake/camel 任一）。
func hasCodeBuddyReasoningEffort(out []byte) bool {
	if v := strings.TrimSpace(gjson.GetBytes(out, "reasoning_effort").String()); v != "" {
		return true
	}
	return strings.TrimSpace(gjson.GetBytes(out, "reasoningEffort").String()) != ""
}

// backfillCodeBuddyReasoningContent DeepSeek 多轮一致性（对齐官方客户端
// requiresReasoningContentOnAssistantMessages）：会话内任一 assistant 消息带 reasoning 痕迹时，
// 上游要求**所有** assistant 消息都带 reasoning_content 字段（string，可为空串），否则报错。
//
// 规则：
//   - 任一 assistant 有非空 `reasoning`（string）或已有 `reasoning_content` 字段 → 触发；
//   - 触发后每个 assistant：已有 reasoning_content 保留；否则复制 `reasoning` 值；两者皆无 → 补空串；
//   - 无任何痕迹 → 零改动（不白白加字段）；非 deepseek 模型 → 零改动。
func backfillCodeBuddyReasoningContent(out []byte) ([]byte, error) {
	model := gjson.GetBytes(out, "model").String()
	if !isCodeBuddyDeepSeekModel(model) {
		return out, nil
	}
	arr := gjson.GetBytes(out, "messages")
	if !arr.IsArray() || len(arr.Array()) == 0 {
		return out, nil
	}
	hasTrace := false
	for _, msg := range arr.Array() {
		if role := strings.TrimSpace(msg.Get("role").String()); role != "assistant" {
			continue
		}
		if r := msg.Get("reasoning"); r.Type == gjson.String && r.String() != "" {
			hasTrace = true
			break
		}
		if msg.Get("reasoning_content").Exists() {
			hasTrace = true
			break
		}
	}
	// 官方触发条件：thinkingEnabled || 存在 reasoning 痕迹（仅看痕迹会漏"开了思考但历史里
	// 还没有 reasoning 的新会话"，而该模型要求所有 assistant 消息都带 reasoning_content）。
	if !hasTrace && !codeBuddyThinkingEnabled(out) {
		return out, nil
	}
	for i, msg := range arr.Array() {
		if role := strings.TrimSpace(msg.Get("role").String()); role != "assistant" {
			continue
		}
		if msg.Get("reasoning_content").Exists() {
			continue // 已有 → 不覆盖
		}
		value := ""
		if r := msg.Get("reasoning"); r.Type == gjson.String {
			value = r.String()
		}
		updated, err := sjson.SetBytes(out, fmt.Sprintf("messages.%d.reasoning_content", i), value)
		if err != nil {
			return nil, fmt.Errorf("backfill codebuddy reasoning_content: %w", err)
		}
		out = updated
	}
	return out, nil
}

// ensureCodeBuddyConsoleSystem 全局域（workbuddy.ai）console 兜底：首条消息非 system 时，
// 在 messages 最前补一条 fallback system（对齐官方客户端；参考实现吸收自上游 PR #45，
// 防 console 域上游按 11128 拦截）。仅 global realm 生效，CN 现状零改动。
func ensureCodeBuddyConsoleSystem(out []byte, account *Account) ([]byte, error) {
	if codeBuddyAccountRealm(account) != codeBuddyRealmKindGlobal {
		return out, nil
	}
	arr := gjson.GetBytes(out, "messages")
	if !arr.IsArray() || len(arr.Array()) == 0 {
		return out, nil
	}
	first := arr.Array()[0]
	if strings.EqualFold(strings.TrimSpace(first.Get("role").String()), "system") {
		return out, nil
	}
	var messages []any
	if err := json.Unmarshal([]byte(arr.Raw), &messages); err != nil {
		return out, nil // body 不可解析：不在这里二次错误化（与调用方口径一致）
	}
	messages = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, messages...)
	updated, err := sjson.SetBytes(out, "messages", messages)
	if err != nil {
		return nil, fmt.Errorf("inject codebuddy console system fallback: %w", err)
	}
	return updated, nil
}
