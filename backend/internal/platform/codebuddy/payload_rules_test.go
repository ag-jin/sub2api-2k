package codebuddy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// reasoning 回填的行为口径见 payload_rules.go::BackfillCodeBuddyReasoningContent
// 与 cbPlaceholder。这一组用例钉住三件事，任何一件回退都会让上游
// 报 400 code=11155 reasoning_content_missing（进而账号冷却、池空、503 死循环）：
//
//  1. 两个字段名都写（reasoning + reasoning_content）；
//  2. 无原文时补**空格**占位，不是空串（上游校验 len>0 且不 trim）；
//  3. 非 deepseek 模型 / 未触发时零改动。

const (
	cbTestModel         = "deepseek-v4.1-flash"
	cbTestOtherModel    = "glm-5.2"
	cbReasoningOriginal = "先看用户想问什么，再组织答案。"
	// cbPlaceholder 是**线上格式约定**（不是引用生产常量）：上游校验 len>0 且不 trim，
	// 无原文时必须出这个单空格。用字面量而非生产常量，基线（未修复）也能编译，
	// 从而让回归用例在基线以"行为不符"失败，而不是构建失败。
	cbPlaceholder = " "
)

// cbAssistantReasoning 取指定下标 assistant 消息上的推理字段值。
func cbAssistantReasoning(t *testing.T, body []byte, idx int) (string, string) {
	t.Helper()
	path := "messages." + itoa(idx) + "."
	return gjson.GetBytes(body, path+"reasoning").String(),
		gjson.GetBytes(body, path+"reasoning_content").String()
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// 客户端只带了 reasoning_content（响应侧字段名回传），出站必须同时补 reasoning
// ——上游请求侧若按 reasoning 校验，只写 reasoning_content 等于没写。
// 基线（修复前）在 reasoning_content 已存在时直接 continue，从不写 reasoning → 本用例失败。
func TestBackfillCodeBuddyReasoningContent_WritesReasoningAlias(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"早","reasoning_content":"` + cbReasoningOriginal + `"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)

	reasoning, reasoningContent := cbAssistantReasoning(t, out, 1)
	assert.Equal(t, cbReasoningOriginal, reasoningContent, "原文必须原样保留")
	assert.Equal(t, cbReasoningOriginal, reasoning, "reasoning 别名必须同时写入")
}

// 客户端只带了 reasoning，出站必须补 reasoning_content（原有行为，防回退）。
func TestBackfillCodeBuddyReasoningContent_CopiesReasoningToContent(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","messages":[
		{"role":"assistant","content":"早","reasoning":"` + cbReasoningOriginal + `"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)

	reasoning, reasoningContent := cbAssistantReasoning(t, out, 0)
	assert.Equal(t, cbReasoningOriginal, reasoning)
	assert.Equal(t, cbReasoningOriginal, reasoningContent)
}

// 没有任何推理原文的 assistant 必须拿到**空格**占位：上游校验 len(value)>0 且不 trim，
// 空串过不了。基线写空串 → 本用例失败。
func TestBackfillCodeBuddyReasoningContent_EmptyGetsSpacePlaceholder(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"第一轮","reasoning":"` + cbReasoningOriginal + `"},
		{"role":"user","content":"继续"},
		{"role":"assistant","content":"第二轮"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)

	// 第一条 assistant：原文保留
	r0, rc0 := cbAssistantReasoning(t, out, 1)
	assert.Equal(t, cbReasoningOriginal, r0)
	assert.Equal(t, cbReasoningOriginal, rc0)

	// 第二条 assistant：无原文 → 两个字段都必须是空格占位，且非空
	r1, rc1 := cbAssistantReasoning(t, out, 3)
	assert.Equal(t, cbPlaceholder, r1)
	assert.Equal(t, cbPlaceholder, rc1)
	assert.NotEmpty(t, r1, "上游校验 len>0，占位不能是空串")
	assert.NotEmpty(t, rc1, "上游校验 len>0，占位不能是空串")
}

// 客户端回传了空的推理字段（畸形输入）时同样补占位，不能留空串。
func TestBackfillCodeBuddyReasoningContent_BlankValueUpgradedToPlaceholder(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","messages":[
		{"role":"assistant","content":"早","reasoning":"","reasoning_content":""}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)

	reasoning, reasoningContent := cbAssistantReasoning(t, out, 0)
	assert.Equal(t, cbPlaceholder, reasoning)
	assert.Equal(t, cbPlaceholder, reasoningContent)
}

// thinking 开启但历史里还没有推理痕迹的新会话，也要补齐占位。
func TestBackfillCodeBuddyReasoningContent_ThinkingEnabledTriggersBackfill(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","reasoning_effort":"high","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"早"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)

	reasoning, reasoningContent := cbAssistantReasoning(t, out, 1)
	assert.Equal(t, cbPlaceholder, reasoning)
	assert.Equal(t, cbPlaceholder, reasoningContent)
}

// 非 deepseek 模型零改动。
func TestBackfillCodeBuddyReasoningContent_NonDeepSeekUntouched(t *testing.T) {
	body := []byte(`{"model":"` + cbTestOtherModel + `","reasoning_effort":"high","messages":[
		{"role":"assistant","content":"早"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(out), "非 deepseek 模型必须逐字不动")
}

// 无痕迹且未开思考：零改动，不白白加字段。
func TestBackfillCodeBuddyReasoningContent_NoTraceNoThinkingUntouched(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"早"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(out), "未触发时必须逐字不动")
}

// 非 assistant 角色（user/tool）绝不能被写推理字段——上游校验只针对 assistant 回合，
// 给别的角色硬塞字段会造出上游不认的组合。
func TestBackfillCodeBuddyReasoningContent_OnlyAssistantTouched(t *testing.T) {
	body := []byte(`{"model":"` + cbTestModel + `","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"早","reasoning":"` + cbReasoningOriginal + `"},
		{"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)

	out, err := BackfillCodeBuddyReasoningContent(body)
	require.NoError(t, err)

	for _, idx := range []int{0, 1, 3} {
		path := "messages." + itoa(idx) + "."
		assert.False(t, gjson.GetBytes(out, path+"reasoning").Exists(), "messages.%d 不该有 reasoning", idx)
		assert.False(t, gjson.GetBytes(out, path+"reasoning_content").Exists(), "messages.%d 不该有 reasoning_content", idx)
	}
}
