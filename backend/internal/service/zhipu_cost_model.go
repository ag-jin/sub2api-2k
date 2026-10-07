package service

// 智谱（Zhipu / bigmodel Coding Plan）上游成本模型——纯函数深模块，无 IO、无全局可变状态。
//
// 事实来源：.scratch/zcode-150-research/FINAL-REPORT.md §三/§四（三组真机结算差分实验）。
// 积分（credits）公式：
//
//	积分 = (输入token×InputPerM + 缓存token×CachedPerM + 输出token×OutputPerM) / 1e4
//	     × 时段系数（闲时 0.5 / 高峰 周一~五 14:00-18:00 UTC+8 记 1.0）
//	     × 渠道系数（签名 V4 = 0.67；无签名 / 纯 API key = 1.0）
//
// 协议漂移或官方调价时只改本文件常量区（表也可经 ZhipuModelCostTable 的 overrides 覆盖）。

import "time"

// 模型 ID（上游 model 字段原样匹配）。
const (
	ZhipuModelGLM53Flash = "glm-5.3-flash"
	ZhipuModelGLM53      = "glm-5.3"
)

// ZhipuModelCost 每万 token 的积分单价。
type ZhipuModelCost struct {
	InputPerM  float64
	CachedPerM float64
	OutputPerM float64
}

// 内置系数表（research §三）。
var zhipuBuiltinModelCosts = map[string]ZhipuModelCost{
	ZhipuModelGLM53Flash: {InputPerM: 2.3, CachedPerM: 0.56, OutputPerM: 8},
	ZhipuModelGLM53:      {InputPerM: 6.9, CachedPerM: 1.7, OutputPerM: 24},
}

// ZhipuModelCostTable 返回内置系数表副本，overrides 覆盖内置值或新增模型。
// 每次调用返回新 map，调用方修改不会污染内置表。
func ZhipuModelCostTable(overrides map[string]ZhipuModelCost) map[string]ZhipuModelCost {
	out := make(map[string]ZhipuModelCost, len(zhipuBuiltinModelCosts)+len(overrides))
	for model, cost := range zhipuBuiltinModelCosts {
		out[model] = cost
	}
	for model, cost := range overrides {
		out[model] = cost
	}
	return out
}

// 时段系数（research §三/§四）。
const (
	ZhipuPeakFactorValue    = 1.0
	ZhipuOffPeakFactorValue = 0.5
)

// zhipuChinaZone 是智谱口径的北京时间固定时区（UTC+8，中国无夏令时）：高峰时段判定
// （本文件 ZhipuPeakFactor）与上游用量自然日归一
// （zhipu_account_monitor_service.go 的 zhipuCreditUsageWindow）共用同一事实源；
// 固定偏移保证结果只由传入时刻决定，与进程时区、运行环境 tzdata 无关。
var zhipuChinaZone = time.FixedZone("UTC+8", 8*60*60)

// ZhipuPeakFactor 返回时段系数：周一~五 14:00（含）–18:00（不含）UTC+8 → 1.0，其余（含周末全天）→ 0.5。
func ZhipuPeakFactor(at time.Time) float64 {
	local := at.In(zhipuChinaZone)
	switch local.Weekday() {
	case time.Saturday, time.Sunday:
		return ZhipuOffPeakFactorValue
	}
	if hour := local.Hour(); hour >= 14 && hour < 18 {
		return ZhipuPeakFactorValue
	}
	return ZhipuOffPeakFactorValue
}

// 渠道系数（research §三/§四）：签名 V4（X-Client-* 十件套 + 握手私钥）→ 0.67；无签名 / 纯 API key → 1.0。
const (
	ZhipuSignedChannelFactor   = 0.67
	ZhipuUnsignedChannelFactor = 1.0
)

// 单价单位为「每万 token 的积分」，与积分口径的换算分母。
const zhipuTokensPerCreditUnit = 1e4

// ZhipuEffectiveCost 返回请求的期望积分消耗（上游结算口径，非平台对用户计费）：
// (输入×InputPerM + 缓存×CachedPerM + 输出×OutputPerM)/1e4 × 时段系数 × 渠道系数。
//
// 未知模型策略（二选一，锁定为显式零值）：返回 0 表示"无已知成本基准"。
// 选择理由：design M5 的签名只有 float64 返回值，加 error 会改已批准接口；下游（27 L2 对账）
// 以返回值做分母前必须先判 `> 0`，零值即"跳过该行"。表外模型需估算时经 ZhipuModelCostTable
// 的 overrides 显式登记。注意 0 与"全零 usage"不可区分，调用方以 usage 是否为空自行判别。
//
// 本函数只查内置表；overrides 只影响 ZhipuModelCostTable 的返回值（供 26/27 自带基准）。
func ZhipuEffectiveCost(model string, in, cached, out float64, at time.Time, signed bool) float64 {
	cost, ok := zhipuBuiltinModelCosts[model]
	if !ok {
		return 0
	}
	channelFactor := ZhipuUnsignedChannelFactor
	if signed {
		channelFactor = ZhipuSignedChannelFactor
	}
	credits := (in*cost.InputPerM + cached*cost.CachedPerM + out*cost.OutputPerM) / zhipuTokensPerCreditUnit
	return credits * ZhipuPeakFactor(at) * channelFactor
}
