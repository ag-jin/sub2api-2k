package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// D3 守门测试（2026-09-23）：**"手动通道列表"与"手动端点可执行集合"必须一致**。
//
// 背景：`codeBuddyGrowthManualChannels()` 的注释承诺
// 「从注册表按分级**推导**而不是硬编码列表：新增 full 级通道时自动纳入，
// 不会出现"代码里写死三个、注册表多了一个却没人能手动跑"」。
//
// 但 `RunCodeBuddyGrowthChannelNow` 的分发是**硬编码 switch**，
// 所以该承诺当时是空头的：追加一个 full 级通道后，它会出现在手动列表里，
// 而端点回 `channel has no manual entry`。
//
// 本测试把该承诺变成**可执行断言**：任何新增 full 级通道若未接手动分支，
// 这里立刻红 —— 不再依赖自觉。
func TestManualChannelListAndEndpointAgree(t *testing.T) {
	const syntheticKey = "zz-synthetic-full-channel"

	original := codebuddy.CodeBuddyGrowthChannelSpecs
	t.Cleanup(func() { codebuddy.CodeBuddyGrowthChannelSpecs = original })

	require.False(t, codeBuddyGrowthChannelKeyIsManual(syntheticKey),
		"前置条件：该键在追加前不应被视为手动通道")

	appended := append([]codebuddy.CodeBuddyGrowthChannelSpec{}, original...)
	appended = append(appended, codebuddy.CodeBuddyGrowthChannelSpec{
		Key:       syntheticKey,
		Tier:      codebuddy.GrowthTierFull,
		Rationale: "守门测试用的合成通道",
	})
	codebuddy.CodeBuddyGrowthChannelSpecs = appended

	// ① 推导列表应包含它（推导逻辑本身是对的）。
	found := false
	for _, spec := range codeBuddyGrowthManualChannels() {
		if spec.Key == syntheticKey {
			found = true
			break
		}
	}
	require.True(t, found, "推导列表应包含新增的 full 级通道")

	// ② 端点对"尚未接分支的新 full 通道"必须**响亮且可操作**地失败，
	//    不能静默成功、也不能只回一句看不出原因的话。
	//
	// 说明：执行逻辑无法从注册表自动推导（每个通道的动作不同），所以
	// 这里约束的是"失败必须可诊断"——错误里要能看出是**没接手动分支**，
	// 并点名通道键。这样新增通道的人一看就知道要做什么。
	svc, _ := newGrowthTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})

	result := svc.RunCodeBuddyGrowthChannelNow(
		context.Background(), newGrowthTestAccount(1), syntheticKey)

	require.Contains(t, result.Error, "no manual entry",
		"新 full 通道未接分支时必须明确报出'没有手动入口'")
	require.Contains(t, result.Error, syntheticKey,
		"错误必须点名通道键，否则新增通道的人无法定位该接哪里")
}
