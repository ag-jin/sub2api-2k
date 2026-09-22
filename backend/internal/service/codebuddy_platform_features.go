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
		},
	})
}
