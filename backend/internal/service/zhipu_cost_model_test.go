//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// zhipuTestTime 用显式 UTC+8 构造时刻，测试不依赖进程时区。
func zhipuTestTime(t *testing.T, year int, month time.Month, day, hour, min, sec int) time.Time {
	t.Helper()
	return time.Date(year, month, day, hour, min, sec, 0, time.FixedZone("UTC+8", 8*60*60))
}

// TestZhipuModelCostTableBuiltinsAndOverrides 内置两种模型系数（research §三），
// overrides 可覆盖内置或新增，且每次返回新 map（调用方改表不影响内置表）。
func TestZhipuModelCostTableBuiltinsAndOverrides(t *testing.T) {
	t.Parallel()

	builtin := ZhipuModelCostTable(nil)
	require.Equal(t, ZhipuModelCost{InputPerM: 2.3, CachedPerM: 0.56, OutputPerM: 8}, builtin[ZhipuModelGLM53Flash])
	require.Equal(t, ZhipuModelCost{InputPerM: 6.9, CachedPerM: 1.7, OutputPerM: 24}, builtin[ZhipuModelGLM53])
	require.Len(t, builtin, 2)

	builtin[ZhipuModelGLM53Flash] = ZhipuModelCost{InputPerM: 999}
	require.Equal(t,
		ZhipuModelCost{InputPerM: 2.3, CachedPerM: 0.56, OutputPerM: 8},
		ZhipuModelCostTable(nil)[ZhipuModelGLM53Flash],
		"内置表必须不共享可变状态")

	custom := ZhipuModelCostTable(map[string]ZhipuModelCost{
		ZhipuModelGLM53: {InputPerM: 7.1, CachedPerM: 1.8, OutputPerM: 25}, // 官方调价
		"glm-4.6":       {InputPerM: 1, CachedPerM: 0.2, OutputPerM: 3},    // 新增模型
	})
	require.Equal(t, ZhipuModelCost{InputPerM: 7.1, CachedPerM: 1.8, OutputPerM: 25}, custom[ZhipuModelGLM53])
	require.Equal(t, ZhipuModelCost{InputPerM: 1, CachedPerM: 0.2, OutputPerM: 3}, custom["glm-4.6"])
	require.Len(t, custom, 3)
}

// TestZhipuPeakFactorBoundaries 高峰时段 = 周一~五 14:00（含）–18:00（不含）UTC+8，其余 0.5。
func TestZhipuPeakFactorBoundaries(t *testing.T) {
	t.Parallel()

	// 日期与星期核对：2026-10-05 周一 / 2026-10-09 周五 / 2026-10-10 周六 / 2026-10-11 周日。
	cases := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"周一 00:00 闲时", zhipuTestTime(t, 2026, 10, 5, 0, 0, 0), 0.5},
		{"周一 13:59 闲时", zhipuTestTime(t, 2026, 10, 5, 13, 59, 0), 0.5},
		{"周一 14:00 高峰起始（含）", zhipuTestTime(t, 2026, 10, 5, 14, 0, 0), 1.0},
		{"周一 17:59:59 高峰末尾", zhipuTestTime(t, 2026, 10, 5, 17, 59, 59), 1.0},
		{"周一 18:00 高峰结束（不含）", zhipuTestTime(t, 2026, 10, 5, 18, 0, 0), 0.5},
		{"周五 13:59 闲时", zhipuTestTime(t, 2026, 10, 9, 13, 59, 0), 0.5},
		{"周五 14:00 高峰", zhipuTestTime(t, 2026, 10, 9, 14, 0, 0), 1.0},
		{"周五 17:59 高峰", zhipuTestTime(t, 2026, 10, 9, 17, 59, 0), 1.0},
		{"周五 18:00 闲时", zhipuTestTime(t, 2026, 10, 9, 18, 0, 0), 0.5},
		{"周五 23:59 闲时", zhipuTestTime(t, 2026, 10, 9, 23, 59, 0), 0.5},
		{"周六 10:00 闲时", zhipuTestTime(t, 2026, 10, 10, 10, 0, 0), 0.5},
		{"周六 14:00 高峰钟点仍闲时", zhipuTestTime(t, 2026, 10, 10, 14, 0, 0), 0.5},
		{"周日 15:00 闲时", zhipuTestTime(t, 2026, 10, 11, 15, 0, 0), 0.5},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.InDelta(t, tc.want, ZhipuPeakFactor(tc.at), 1e-12)
		})
	}
}

// TestZhipuPeakFactorTimezoneIndependent 同一瞬时的不同时区表示结果一致：
// 高/闲时判定只看 UTC+8，与 time.Time 携带的位置、进程 TZ 无关。
func TestZhipuPeakFactorTimezoneIndependent(t *testing.T) {
	t.Parallel()

	peakInstant := zhipuTestTime(t, 2026, 10, 5, 14, 0, 0) // 周一 14:00 UTC+8
	require.InDelta(t, 1.0, ZhipuPeakFactor(peakInstant.UTC()), 1e-12)
	require.InDelta(t, 1.0, ZhipuPeakFactor(peakInstant.In(time.FixedZone("Asia/Tokyo", 9*60*60))), 1e-12)
	require.InDelta(t, 1.0, ZhipuPeakFactor(peakInstant.In(time.FixedZone("America/New_York", -4*60*60))), 1e-12)

	// 纽约本地 02:00 = 06:00 UTC = 14:00 UTC+8：按本地钟判段会得 0.5，按 UTC+8 得 1.0。
	newYork := time.FixedZone("America/New_York", -4*60*60)
	require.InDelta(t, 1.0, ZhipuPeakFactor(time.Date(2026, 10, 5, 2, 0, 0, 0, newYork)), 1e-12)

	// 闲时同理：周一 05:59 UTC = 13:59 UTC+8。
	require.InDelta(t, 0.5, ZhipuPeakFactor(time.Date(2026, 10, 5, 5, 59, 0, 0, time.UTC)), 1e-12)
	require.InDelta(t, 0.5, ZhipuPeakFactor(time.Date(2026, 10, 5, 13, 59, 0, 0, newYork)), 1e-12)
}

// TestZhipuChannelFactorConstants 渠道系数：签名 V4 0.67 / 无签名 1.0；签名闲时合成系数 0.335。
func TestZhipuChannelFactorConstants(t *testing.T) {
	t.Parallel()

	require.InDelta(t, 0.67, ZhipuSignedChannelFactor, 1e-12)
	require.InDelta(t, 1.0, ZhipuUnsignedChannelFactor, 1e-12)
	require.InDelta(t, 0.335, ZhipuSignedChannelFactor*ZhipuOffPeakFactorValue, 1e-12)
}

// TestZhipuEffectiveCost 期望积分：R4 真机实测向量（research §四）+ 四组合 + 双模型系数。
// 闲时 = 0.5，高峰（周一 14:00 UTC+8）= 1.0。
func TestZhipuEffectiveCost(t *testing.T) {
	t.Parallel()

	offPeak := zhipuTestTime(t, 2026, 10, 5, 13, 0, 0) // 周一 13:00 UTC+8
	peak := zhipuTestTime(t, 2026, 10, 5, 14, 0, 0)    // 周一 14:00 UTC+8

	cases := []struct {
		name   string
		model  string
		in     float64
		cached float64
		out    float64
		at     time.Time
		signed bool
		want   float64
	}{
		// R4a 无签名实测（2026-10-06 13:55 UTC+8，闲时）：输入 1.1500/万、输出 4.0000/万。
		{"flash 输入1万 无签名闲时 = 1.15", ZhipuModelGLM53Flash, 10000, 0, 0, offPeak, false, 1.1500},
		{"flash 输出1万 无签名闲时 = 4.00", ZhipuModelGLM53Flash, 0, 0, 10000, offPeak, false, 4.0000},
		// R4b 签名 V4 实测：输入 0.7705/万、输出 2.6800/万（原价 ×0.67 分毫不差）。
		{"flash 输入1万 签名闲时 = 0.7705", ZhipuModelGLM53Flash, 10000, 0, 0, offPeak, true, 0.7705},
		{"flash 输出1万 签名闲时 = 2.68", ZhipuModelGLM53Flash, 0, 0, 10000, offPeak, true, 2.6800},
		// 四组合（高峰 = 闲时 ×2）：无/有签名 × 高/闲时。
		{"flash 输入1万 无签名高峰 = 2.3", ZhipuModelGLM53Flash, 10000, 0, 0, peak, false, 2.3},
		{"flash 输入1万 签名高峰 = 1.541", ZhipuModelGLM53Flash, 10000, 0, 0, peak, true, 1.541},
		{"flash 输出1万 无签名高峰 = 8", ZhipuModelGLM53Flash, 0, 0, 10000, peak, false, 8},
		{"flash 输出1万 签名高峰 = 5.36", ZhipuModelGLM53Flash, 0, 0, 10000, peak, true, 5.36},
		// 缓存价 0.56/万（research §三）。
		{"flash 缓存1万 无签名闲时 = 0.28", ZhipuModelGLM53Flash, 0, 10000, 0, offPeak, false, 0.28},
		{"flash 缓存1万 无签名高峰 = 0.56", ZhipuModelGLM53Flash, 0, 10000, 0, peak, false, 0.56},
		// glm-5.3 系数 6.9 / 1.7 / 24（research §三）。
		{"glm-5.3 输入1万 无签名高峰 = 6.9", ZhipuModelGLM53, 10000, 0, 0, peak, false, 6.9},
		{"glm-5.3 缓存1万 无签名高峰 = 1.7", ZhipuModelGLM53, 0, 10000, 0, peak, false, 1.7},
		{"glm-5.3 输出1万 无签名高峰 = 24", ZhipuModelGLM53, 0, 0, 10000, peak, false, 24},
		{"glm-5.3 输出1万 签名闲时 = 8.04", ZhipuModelGLM53, 0, 0, 10000, offPeak, true, 8.04},
		// R4 大请求黄金值（手工推导：523037×2.3/1e4×0.5 = 60.149255，票面记作 60.1492）。
		{"R4a 523037入 无签名闲时 = 60.149255", ZhipuModelGLM53Flash, 523037, 0, 0, offPeak, false, 60.149255},
		// 含输出：523037×2.3/1e4×0.5 + 203×8/1e4×0.5 = 60.149255 + 0.0812。
		{"R4a 523037入/203出 无签名闲时", ZhipuModelGLM53Flash, 523037, 0, 203, offPeak, false, 60.230455},
		// 签名：523038×2.3/1e4×0.5×0.67 + 500×8/1e4×0.5×0.67 = 40.3000779 + 0.134。
		{"R4b 523038入/500出 签名闲时", ZhipuModelGLM53Flash, 523038, 0, 500, offPeak, true, 40.4340779},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.InDelta(t, tc.want, ZhipuEffectiveCost(tc.model, tc.in, tc.cached, tc.out, tc.at, tc.signed), 1e-9)
		})
	}
}

// TestZhipuEffectiveCostZeroAndUnknown 空 usage → 0；未知模型 → 0（显式零值策略，锁定实现选择）。
func TestZhipuEffectiveCostZeroAndUnknown(t *testing.T) {
	t.Parallel()

	peak := zhipuTestTime(t, 2026, 10, 5, 14, 0, 0)
	require.Zero(t, ZhipuEffectiveCost(ZhipuModelGLM53Flash, 0, 0, 0, peak, false))
	require.Zero(t, ZhipuEffectiveCost(ZhipuModelGLM53Flash, 0, 0, 0, peak, true))
	require.Zero(t, ZhipuEffectiveCost(ZhipuModelGLM53, 0, 0, 0, zhipuTestTime(t, 2026, 10, 10, 15, 0, 0), true))

	// 未知模型：返回 0（design M5 签名无 error 返回位；下游以 >0 判基准是否可用）。
	require.Zero(t, ZhipuEffectiveCost("glm-4.6", 10000, 0, 10000, peak, false))
	require.Zero(t, ZhipuEffectiveCost("", 10000, 0, 0, peak, true))
	require.Zero(t, ZhipuEffectiveCost("GLM-5.3-FLASH", 10000, 0, 0, peak, false), "模型 ID 精确匹配，不做大小写归一")

	// 周末高峰钟点仍闲时，叠加签名：flash 输入1万 = 2.3×0.5×0.67 = 0.7705。
	saturday := zhipuTestTime(t, 2026, 10, 10, 15, 0, 0)
	require.InDelta(t, 0.7705, ZhipuEffectiveCost(ZhipuModelGLM53Flash, 10000, 0, 0, saturday, true), 1e-9)

	// overrides 只作用于表本身：不改变 ZhipuEffectiveCost 的内置表口径（该函数无 overrides 入参）。
	table := ZhipuModelCostTable(map[string]ZhipuModelCost{"glm-4.6": {InputPerM: 1}})
	require.Contains(t, table, "glm-4.6")
	require.Zero(t, ZhipuEffectiveCost("glm-4.6", 10000, 0, 0, peak, false))
}
