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

// Scenario：`full` 级动作的**性质**只能是领养 / 夜猫子 / 开学季 / 抽奖；
// 旅行领奖必须保持 claim。
//
// 这是团队负责人 2026-09-22 的裁定：分级按**动作性质**，不按通道整体。
// 若有人把 travel_run（纯幂等领奖）也归成 full，自动排程会白白失去旅行领奖；
// 反过来若把 adopt / night_cat / school / lottery 的**性质**降级成 claim，
// 就会抹掉"它们在伪造上报 / 造成不可逆消耗"这个事实——那是篡改事实陈述。
//
// ⚠️ 注意本测试断言的是 Tier（性质），**不是**能否自动跑。
// 2026-09-29 用户授权 adopt / night_cat / school 进自动排程，改变的是**政策**
// （`AutoAuthorized`），性质没变。所以这三个现在"性质 full 但可自动"——
// 本测试必须仍然要求它们 `Tier == full`，否则放开政策时顺手把性质也改了，
// 下一个人就再也看不出这些动作在伪造什么。
func TestCodeBuddyGrowthFullTierIsExactlyTheForgeryActions(t *testing.T) {
	mustRemainFull := []string{
		CodeBuddyGrowthChannelAdopt,
		CodeBuddyGrowthChannelNightCat,
		CodeBuddyGrowthChannelSchool,
		CodeBuddyGrowthChannelLottery,
	}
	for _, key := range mustRemainFull {
		spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
		if !ok {
			t.Errorf("通道 %s 未注册", key)
			continue
		}
		if spec.Tier != GrowthTierFull {
			t.Errorf("通道 %s 的性质必须保持 full（实际 %s）。依据：%s",
				key, spec.Tier, spec.Rationale)
		}
		// full 的性质判据本身必须拒绝自动——授权不改变性质判断函数。
		if spec.Tier.AutoSchedulable() {
			t.Errorf("通道 %s 是 full 级，其 Tier.AutoSchedulable() 必须为 false", key)
		}
	}
	// 旅行领奖必须仍在 claim（不可连坐）。
	travel, ok := CodeBuddyGrowthChannelSpecByKey(CodeBuddyGrowthChannelTravelRun)
	if !ok {
		t.Fatal("travel_run 未注册")
	}
	if travel.Tier != GrowthTierClaim {
		t.Errorf("travel_run 是纯幂等领奖，性质应为 claim，实际 %s", travel.Tier)
	}
}

// Scenario：**授权面的守门**——只有已获用户明确授权的 full 通道才能自动跑，
// 且每条授权必须写明依据。
//
// 这是 2026-09-29 用户裁定后的政策边界（原裁定为"full 全部仅手动"）：
//
//	开活跃上报 + 领养 / 夜猫子 / 开学季；**抽奖保留手动**。
//
// 本测试钉住三件事，任一条被改都会红：
//  1. 授权集合**恰好**是这三个（新增未授权的 full 通道混进来会红）；
//  2. 每条被授权的 full 通道都有非空 `AutoAuthorization`（防"悄悄放开"）；
//  3. **lottery 不得被授权**——它是用户明确排除的那一个。
func TestCodeBuddyGrowthAutoRunnableChannelsAreAuthorized(t *testing.T) {
	wantAuthorized := map[string]bool{
		CodeBuddyGrowthChannelAdopt:    true,
		CodeBuddyGrowthChannelNightCat: true,
		CodeBuddyGrowthChannelSchool:   true,
	}

	got := CodeBuddyGrowthAuthorizedFullChannelKeys()
	gotSet := map[string]bool{}
	for _, key := range got {
		gotSet[key] = true
		if !wantAuthorized[key] {
			t.Errorf("full 通道 %s 被授权自动执行，但不在用户裁定的授权集合内"+
				"（2026-09-29 裁定：开活跃上报+领养/夜猫/开学季，抽奖排除）", key)
		}
	}
	for key := range wantAuthorized {
		if !gotSet[key] {
			t.Errorf("full 通道 %s 应已获授权自动执行（用户 2026-09-29 裁定）", key)
		}
		spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
		if !ok {
			t.Errorf("通道 %s 未注册", key)
			continue
		}
		if !spec.AutoAuthorized {
			t.Errorf("通道 %s 应 AutoAuthorized=true", key)
		}
		if strings.TrimSpace(spec.AutoAuthorization) == "" {
			t.Errorf("通道 %s 已授权自动但没有写明依据（AutoAuthorization 为空）——"+
				"授权必须可追溯，否则就是悄悄放开", key)
		}
	}

	// 抽奖：用户明确排除，**必须**仍未授权。
	lottery, ok := CodeBuddyGrowthChannelSpecByKey(CodeBuddyGrowthChannelLottery)
	if !ok {
		t.Fatal("lottery 未注册")
	}
	if lottery.AutoAuthorized {
		t.Error("lottery 不得获自动授权：一次调用抽光全部次数且不可恢复，" +
			"无人看管时不可接受（用户 2026-09-29 明确排除）")
	}
	if lottery.AutoRunnable() {
		t.Error("lottery 不得进自动排程")
	}
	if CodeBuddyGrowthChannelKeyAllowsAutoSchedule(CodeBuddyGrowthChannelLottery) {
		t.Error("lottery 不得允许自动调度")
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
	//
	// 注意：这里断言的是 **Tier 级别的性质函数**（full 恒 false），
	// 与"通道能否自动跑"（`AutoRunnable()` = 性质 或 授权）是两件事。
	// 授权放开后这条不变——授权不该回头去改性质判断。
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		want := spec.Tier != GrowthTierFull
		if spec.Tier.AutoSchedulable() != want {
			t.Errorf("通道 %s：Tier=%s 但 AutoSchedulable()=%v，两者不一致",
				spec.Key, spec.Tier, spec.Tier.AutoSchedulable())
		}
	}
	// AutoAuthorized=true 的通道必须带依据；这是与上一条配对的结构约束。
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.AutoAuthorized && strings.TrimSpace(spec.AutoAuthorization) == "" {
			t.Errorf("通道 %s 标了 AutoAuthorized 但没写 AutoAuthorization 依据", spec.Key)
		}
	}
}

// --- A6 P6 / 2026-09-29 政策更新：自动列表的守门测试 ---

// Scenario：自动排程列表里的**每一项**都必须是"性质可自动"或"已显式授权"的，
// 且未授权的 full 通道一个都不能混进来。
//
// ⚠️ 本测试此前叫 `TestCodeBuddyGrowthAutoSchedulableChannelsAreNeverFull`，
// 断言"列表里不得有任何 full"。2026-09-29 用户授权 adopt/night_cat/school 后，
// 那条断言与政策直接冲突——但它**不能**简单删掉或放宽成"什么都行"，
// 否则就失去守门作用（"未授权的 full 混进来"将无人拦）。
// 所以改成断言**授权条件**：列表里每一项的 `AutoRunnable()` 必须为真，
// 且 full 项必须 `AutoAuthorized`。
//
// 断言四层：
//  1. 列表里每一项都满足 `AutoRunnable()`（性质 或 授权）；
//  2. 其中 full 级的必须 `AutoAuthorized=true`（授权是唯一放行理由）；
//  3. 反向：未授权的通道（含 lottery）不在列表里；
//  4. 列表非空（防"返回空集"这种最隐蔽的假绿）。
func TestCodeBuddyGrowthAutoRunnableChannelsAreNeverUnauthorized(t *testing.T) {
	autoKeys := CodeBuddyGrowthAutoRunnableChannelKeys()

	requireNotEmpty(t, autoKeys)

	autoSet := map[string]bool{}
	for _, key := range autoKeys {
		autoSet[key] = true
		spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
		requireTrue(t, ok, "自动通道 %s 未在注册表里", key)
		if !spec.AutoRunnable() {
			t.Errorf("通道 %s 出现在自动排程列表里，但 AutoRunnable() 为 false",
				key)
		}
		// full 级出现在这里，唯一正当理由是"已授权"。
		if spec.Tier == GrowthTierFull && !spec.AutoAuthorized {
			t.Errorf("full 级通道 %s 未获授权却出现在自动排程列表里", key)
		}
	}

	// 反向：每一个**未授权**（AutoRunnable 为假）的通道都必须不在列表里。
	unauthorizedCount := 0
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.AutoRunnable() {
			continue
		}
		unauthorizedCount++
		if autoSet[spec.Key] {
			t.Errorf("未授权通道 %s 混进了自动排程列表", spec.Key)
		}
	}
	requireTrue(t, unauthorizedCount > 0,
		"注册表里应有未授权通道（否则本测试失去意义——它靠未授权项的存在来验证过滤）")

	// 逐个点名：这几个**必须**在自动列表里（用户 2026-09-29 授权）。
	for _, key := range []string{
		CodeBuddyGrowthChannelAdopt,
		CodeBuddyGrowthChannelNightCat,
		CodeBuddyGrowthChannelSchool,
	} {
		if !autoSet[key] {
			t.Errorf("通道 %s 已获用户授权自动执行，应在自动排程列表里。"+
				"依据：%s", key, mustAuthorization(key))
		}
	}

	// 而抽奖**必须不在**（用户明确排除）。点名版把理由打出来，
	// 让改错的人当场看到依据，而不是只看到"该通道在列表里"。
	if autoSet[CodeBuddyGrowthChannelLottery] {
		t.Errorf("通道 %s 不得进自动排程。依据：%s",
			CodeBuddyGrowthChannelLottery, mustAuthorization(CodeBuddyGrowthChannelLottery))
	}
}

// mustAuthorization 取通道的授权/分级依据文本（供断言失败信息用）。
func mustAuthorization(key string) string {
	spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
	if !ok {
		return "（通道未注册）"
	}
	if spec.AutoAuthorization != "" {
		return spec.AutoAuthorization
	}
	return spec.Rationale
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
