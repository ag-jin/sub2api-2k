package codebuddy

import (
	"strings"
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

// --- A6 P5：三级分级的调度边界（按动作拆）---

// Scenario：full 级动作**只能是**领养 / 夜猫子 / 开学季；旅行领奖必须可自动。
//
// 这是团队负责人 2026-09-22 的裁定：分级按**动作性质**，不按通道整体。
// 若有人把 travel_run（纯幂等领奖）也归成 full，自动排程会白白失去旅行领奖；
// 反过来若把 adopt / night_cat / school 降级成 claim，伪造上报就会进自动排程
// ——**那是用户裁定的合规红线**。
func TestCodeBuddyGrowthFullTierIsExactlyTheForgeryActions(t *testing.T) {
	needManual := map[string]bool{
		CodeBuddyGrowthChannelAdopt:    true,
		CodeBuddyGrowthChannelNightCat: true,
		CodeBuddyGrowthChannelSchool:   true,
	}

	autoKeys := CodeBuddyGrowthAutoSchedulableChannelKeys()
	autoSet := map[string]bool{}
	for _, key := range autoKeys {
		autoSet[key] = true
	}

	for key := range needManual {
		if autoSet[key] {
			t.Errorf("%s 是 full 级（含伪造活跃上报语义），不得出现在自动排程里", key)
		}
		if !CodeBuddyGrowthChannelKeyAllowsAutoSchedule(key) {
			continue // 期望如此
		}
		t.Errorf("%s 不应允许自动调度", key)
	}
}

// Scenario：纯幂等领奖的旅行动作**必须可自动**（不能被整条通道连坐）。
func TestCodeBuddyGrowthTravelRunStaysAutoSchedulable(t *testing.T) {
	for _, key := range []string{
		CodeBuddyGrowthChannelTravelStatus,
		CodeBuddyGrowthChannelTravelRun,
		CodeBuddyGrowthChannelTrial,
		CodeBuddyGrowthChannelStreak,
	} {
		if !CodeBuddyGrowthChannelKeyAllowsAutoSchedule(key) {
			t.Errorf("%s 是只读或幂等领奖，应允许自动调度（按动作分级，勿连坐）", key)
		}
	}
}

// Scenario：未注册的通道键 fail-closed（不认识的通道不照跑）。
func TestCodeBuddyGrowthUnknownChannelIsNeverAutoSchedulable(t *testing.T) {
	for _, key := range []string{"", "nonexistent", "travel "} {
		if CodeBuddyGrowthChannelKeyAllowsAutoSchedule(key) {
			t.Errorf("未知通道 %q 必须 fail-closed（不允许自动调度）", key)
		}
	}
}

// Scenario：全表自洽——每个 spec 的分级合法、键唯一、rationale 非空。
func TestCodeBuddyGrowthChannelRegistryIsInternallyConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if seen[spec.Key] {
			t.Fatalf("通道键重复：%q（重复会让分级互相覆盖）", spec.Key)
		}
		seen[spec.Key] = true
		if !spec.Tier.IsValid() {
			t.Errorf("通道 %s 的分级 %q 非法", spec.Key, spec.Tier)
		}
		if strings.TrimSpace(spec.Rationale) == "" {
			t.Errorf("通道 %s 缺少分级理由（合规裁定要求逐条标明）", spec.Key)
		}
	}
	// AutoSchedulable 必须与 full 的补集一致（防止有人只改一边）。
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		want := spec.Tier != GrowthTierFull
		if spec.Tier.AutoSchedulable() != want {
			t.Errorf("通道 %s：Tier=%s 但 AutoSchedulable()=%v，两者不一致",
				spec.Key, spec.Tier, spec.Tier.AutoSchedulable())
		}
	}
}

// --- A6 P6：注释里点名的守门测试（此前注释引用了它，但测试并不存在）---

// Scenario：`AutoSchedulableChannelKeys()` 返回的**每个**通道都不得是 full 级。
//
// ⚠️ 这个测试名此前被 `growth_tasks.go` 的注释引用（"改这里会被
// TestCodeBuddyGrowthAutoSchedulableChannelsAreNeverFull 拦住"），
// **但测试文件里并没有它**——即注释承诺了一道不存在的防线。
// 这是本项目反复出现的一类问题（码表 `11-128` 被"测过"实则求值错、
// A5 的"间隔"从未被断言）：**读注释的人会以为边界已被守住**。
// 本次把它真正补上。
//
// 断言三层：
//  1. 返回列表里每一项的 Tier 都不是 full；
//  2. 反向：所有 full 级通道都不在列表里（防"过滤写反了返回空集"）；
//  3. 列表非空（防"返回空集"这种最隐蔽的假绿）。
func TestCodeBuddyGrowthAutoSchedulableChannelsAreNeverFull(t *testing.T) {
	autoKeys := CodeBuddyGrowthAutoSchedulableChannelKeys()

	requireNotEmpty(t, autoKeys)

	autoSet := map[string]bool{}
	for _, key := range autoKeys {
		autoSet[key] = true
		spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
		requireTrue(t, ok, "自动通道 %s 未在注册表里", key)
		if spec.Tier == GrowthTierFull {
			t.Errorf("full 级通道 %s（%s）不得出现在自动排程列表里",
				key, spec.Rationale)
		}
	}

	// 反向：每一个 full 级通道都必须**不在**自动列表里。
	fullCount := 0
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.Tier != GrowthTierFull {
			continue
		}
		fullCount++
		if autoSet[spec.Key] {
			t.Errorf("full 级通道 %s 混进了自动排程列表", spec.Key)
		}
	}
	requireTrue(t, fullCount > 0,
		"注册表里应有 full 级通道（否则本测试失去意义——它靠 full 的存在来验证过滤）")
}

// requireNotEmpty / requireTrue 本地断言（避免为两条断言引入额外 import）。
func requireNotEmpty(t *testing.T, values []string) {
	t.Helper()
	if len(values) == 0 {
		t.Fatal("自动可调度通道列表为空——要么注册表空了，要么过滤写反了（假绿高发区）")
	}
}

func requireTrue(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
