package service

import "time"

// CodeBuddy 平台的平台级功能注册（A4 批 4.0/4.1）。
//
// 注册动作放在 service 包内、而不是 platform/codebuddy：那边不能反向 import
// service（会成环，见 00-shared 的依赖方向约定）。平台归属仍用平台名标识，
// 注册表本身不认识 codebuddy，多平台可以并存。

const (
	// CodeBuddyCheckinFeatureKey 自动签到功能标识。
	CodeBuddyCheckinFeatureKey = "checkin"

	// CodeBuddyAutoCheckinStartHour/Minute 默认窗口起点 09:00；End 11:00。
	// 对齐参考实现（REF-B tencent.py）的白天档双签到时段。
	CodeBuddyAutoCheckinStartHour   = 9
	CodeBuddyAutoCheckinStartMinute = 0
	CodeBuddyAutoCheckinEndHour     = 11
	CodeBuddyAutoCheckinEndMinute   = 0

	// CodeBuddyActivityFeatureKey 活跃上报功能标识（A5 批 5.4）。
	CodeBuddyActivityFeatureKey = "activity"

	// CodeBuddyActivityDefaultStartHour 默认上报时点 10 点（对齐参考实现
	// activity_hours=[10]）。
	//
	// 为什么不是"10:00 整这一个时刻"：窗口语义（而非时点语义）才能容忍进程重启——
	// 参考实现用一次性定时器算 nextFire，重启在 10:30 就会把当天跳过去；
	// 窗口模式下 10:30 起来仍能补报当天。因此这里把"10 点"表达为
	// **10:00–11:00 这个小时窗口**，语义上仍是"每号每天 1 次、10 点档"。
	CodeBuddyActivityDefaultStartHour   = 10
	CodeBuddyActivityDefaultStartMinute = 0
	CodeBuddyActivityDefaultEndHour     = 11
	CodeBuddyActivityDefaultEndMinute   = 0

	// CodeBuddyGrowthFeatureKey 成长任务链功能标识（A6 批 P5）。
	//
	// ⚠️ 该开关**只覆盖自动可跑的那部分**（preview / claim 级通道）。
	// `full` 级通道（领养 / 夜猫子 / 开学季点亮）不受它控制——那些只能手动触发，
	// 手动端点不读这个开关（人明确要求执行，不该被自动排程的开关挡住）。
	CodeBuddyGrowthFeatureKey = "growth"

	// CodeBuddyGrowthDefaultStartHour 默认成长链窗口起点 09:00（对齐参考实现
	// 旅行排程 travel_hours=[9,21] 的白天档）；到 11:00 结束，给连登链多步动作留余量。
	CodeBuddyGrowthDefaultStartHour   = 9
	CodeBuddyGrowthDefaultStartMinute = 0
	CodeBuddyGrowthDefaultEndHour     = 11
	CodeBuddyGrowthDefaultEndMinute   = 0
)

// codeBuddyTimeZone 积分/签到口径统一按 UTC+8（与 A2 的积分到期解析同源）。
// 用 FixedZone 而不是 time.LoadLocation("Asia/Shanghai")：容器里可能没有 tzdata，
// 那会让时区加载失败并静默退回 UTC，导致窗口整天错位。
var codeBuddyTimeZone = time.FixedZone("UTC+8", 8*60*60)

func init() {
	RegisterPlatformFeatureSpec(PlatformFeatureSpec{
		Platform: PlatformCodeBuddy,
		Features: []PlatformFeatureDefinition{
			{
				Key:  CodeBuddyCheckinFeatureKey,
				Kind: PlatformFeatureTimeRange,
				// Title/Description 直接下发到设置页（前端 feature.title || feature.key
				// 兜底）。留空会让用户看到裸 key，所以必须给文案。
				Title: "每日签到",
				Description: "在设定时间段内自动为账号签到一次，补回积分。" +
					"默认为关闭：开启后会对上游发出签到请求。",
				// 默认关闭：签到是对上游的写操作（会真实改动账号积分状态），
				// 按"显式开启"原则，没被管理员打开前调度器一个请求都不发。
				EnabledByDefault: false,
				Timezone:         codeBuddyTimeZone,
				DefaultStart: TimeOfDay{
					Hour:   CodeBuddyAutoCheckinStartHour,
					Minute: CodeBuddyAutoCheckinStartMinute,
				},
				DefaultEnd: TimeOfDay{
					Hour:   CodeBuddyAutoCheckinEndHour,
					Minute: CodeBuddyAutoCheckinEndMinute,
				},
			},
			{
				Key:   CodeBuddyActivityFeatureKey,
				Kind:  PlatformFeatureTimeRange,
				Title: "活跃上报",
				Description: "在设定时间段内为每个账号上报一次对话活跃（每号每天 1 次），" +
					"点亮连登并补满领养猫所需的对话门槛。默认为关闭。",
				// 默认关闭：活跃上报同样是对上游的写操作（会改动账号连登状态），
				// 与签到一个原则——没显式开启前一个请求都不发。
				EnabledByDefault: false,
				Timezone:         codeBuddyTimeZone,
				DefaultStart: TimeOfDay{
					Hour:   CodeBuddyActivityDefaultStartHour,
					Minute: CodeBuddyActivityDefaultStartMinute,
				},
				DefaultEnd: TimeOfDay{
					Hour:   CodeBuddyActivityDefaultEndHour,
					Minute: CodeBuddyActivityDefaultEndMinute,
				},
			},
			{
				Key:   CodeBuddyGrowthFeatureKey,
				Kind:  PlatformFeatureTimeRange,
				Title: "成长任务",
				Description: "在设定时间段内自动执行**幂等领奖类**成长动作" +
					"（旅行派出与领奖、连登补签/兑换/礼包/补偿、国际版 trial）。" +
					"含伪造活跃上报语义的动作（领养、夜猫子、开学季点亮）**不在此列**，" +
					"仅可手动触发。默认为关闭。",
				// 默认关闭：这些都是对上游的写操作。
				EnabledByDefault: false,
				Timezone:         codeBuddyTimeZone,
				DefaultStart: TimeOfDay{
					Hour:   CodeBuddyGrowthDefaultStartHour,
					Minute: CodeBuddyGrowthDefaultStartMinute,
				},
				DefaultEnd: TimeOfDay{
					Hour:   CodeBuddyGrowthDefaultEndHour,
					Minute: CodeBuddyGrowthDefaultEndMinute,
				},
			},
		},
	})
}
