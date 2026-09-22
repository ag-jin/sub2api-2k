package codebuddy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario: 码表映射覆盖**字符串与数字两种形态**。
//
// ⚠️ 这组断言是 2026-09-22 修的回归防护。上一版把这一项写成 Go 常量表达式
// `11-128:`，被 Go 词法器当作减法求值成 **-117**，于是：
//   - 表里那一项的键是 -117，永远匹配不上上游真实发送的 "11-128"；
//   - 而旧测试写成 `for _, code := range []int{…, 11-128, …}` —— 同样求值成
//     -117，断言的是那个错的键，**全绿但完全没测到真实码**。
//
// 因此本测试刻意同时覆盖两种入参形态，防止再次被"看起来对"的写法骗过。
func TestCodeBuddyBizCodeHints(t *testing.T) {
	t.Parallel()

	// 字符串形态（上游对 11-128 实际发送的形态）
	for _, code := range []string{"0", "10001", "11101", "11-128", "11217", "12153", "14018", "6004", "11102", "11140", "14017"} {
		require.NotEmpty(t, CodeBuddyBizCodeHint(code), "字符串码 %q 应有说明", code)
	}
	// 数字形态（上游多数码是 JSON 数字；经 gjson 常落成 float64）
	for _, code := range []any{0, 10001, 11101, 11217, 12153, 14018, 6004.0, int64(11102)} {
		require.NotEmpty(t, CodeBuddyBizCodeHint(code), "数字码 %v 应有说明", code)
	}

	require.Empty(t, CodeBuddyBizCodeHint(99999))
	require.Empty(t, CodeBuddyBizCodeHint("99999"))

	require.Equal(t, "raw msg", CodeBuddyBizCodeMessage(99999, "raw msg"))
	require.Contains(t, CodeBuddyBizCodeMessage(10001, "已签过"), "今日已签到")
	// 关键回归：字符串形态必须能查到说明（旧实现这里恒为空）
	hint := CodeBuddyBizCodeMessage("11-128", "Illegal API invocation from an unapproved channel")
	require.Contains(t, hint, "安全策略拦截")
	require.NotContains(t, hint, "code 0")
}

// Scenario: 反探测数字改写形态（净化器用的 6 字符形态）**不得**被当成真实业务码。
//
// 净化器会把裸误码改写成"插入一个连字符"的形态；若它与码表键撞上，就会出现
// "改写出来的文本恰好命中码表"的诡异耦合。此断言把两者的形态差异钉死。
func TestCodeBuddyBizCodeHintsRejectsForgedForm(t *testing.T) {
	t.Parallel()

	// 5 码点真实形态有说明
	require.NotEmpty(t, CodeBuddyBizCodeHint("11-128"))
	// 6 字符伪造形态（净化器产物）不应命中
	require.Empty(t, CodeBuddyBizCodeHint("11--128"))
}

// Scenario: 上游信封文案按**原值**取 code，字符串码不被 .Int() 吞成 0。
func TestCodeBuddyUpstreamEnvelopeMessageKeepsStringCode(t *testing.T) {
	t.Parallel()

	// 真实形态：code 是字符串
	got := CodeBuddyUpstreamEnvelopeMessage([]byte(`{"code":"11-128","msg":"Illegal API invocation from an unapproved channel"}`))
	require.Contains(t, got, "11-128", "字符串码必须原样回显")
	require.NotContains(t, got, "code 0")

	// 数字形态
	got = CodeBuddyUpstreamEnvelopeMessage([]byte(`{"code":12153,"msg":"session dead"}`))
	require.Contains(t, got, "12153")

	// 无 code / code=0 → 只回 msg
	assert.Equal(t, "ok", CodeBuddyUpstreamEnvelopeMessage([]byte(`{"code":0,"msg":"ok"}`)))

	// 无顶层 msg → 空串（非本形态）
	assert.Empty(t, CodeBuddyUpstreamEnvelopeMessage([]byte(`{"error":{"message":"x"}}`)))
}

// Scenario: 归一化键对 JSON 数字的浮点形态也成立（gjson 常给 float64）。
func TestCodeBuddyNormalizeBizCodeNumericForms(t *testing.T) {
	t.Parallel()

	for _, in := range []any{10001, int64(10001), float64(10001)} {
		require.Equal(t, "10001", codeBuddyNormalizeBizCode(in), "入参 %#v", in)
	}
	require.Equal(t, "11-128", codeBuddyNormalizeBizCode("11-128"))
	require.Equal(t, "11-128", codeBuddyNormalizeBizCode("  11-128  "))
	require.Equal(t, "", codeBuddyNormalizeBizCode(nil))
}
