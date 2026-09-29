package codebuddy

import (
	"strings"
	"time"
)

// 成长任务链（批 6）的合规分级、执行授权与通道规格。
//
// ## 为什么分级信息放在 platform 包而不是 service 包
//
// 分级是**用户亲自裁定的合规边界**，不是调度实现细节。把它放在这里，好处是
// 调度器、管理端点、测试三方读的是**同一份声明**——任何一方想"临时放宽"都必须
// 改这个文件，而改这里会被 `TestCodeBuddyGrowthAutoRunnableChannelsAreAuthorized`
// 与 `TestCodeBuddyGrowthChannelRegistryIsInternallyConsistent` 同时拦住。
//
// ## 两个正交维度：性质（Tier）与政策（AutoAuthorized）
//
//   - **Tier 说明动作的性质**——客观事实，不随政策变：只读 / 幂等领奖 /
//     含伪造上报或不可逆消耗。它回答"这个动作**是什么**"。
//   - **AutoAuthorized 记录用户的政策决定**——可随裁定变：该通道是否已获
//     明确授权、允许进自动排程。它回答"我们**允许**它怎么跑"。
//
// 为什么必须分开：2026-09-22 的裁定把"含伪造上报"的通道全部归为仅手动；
// 2026-09-29 用户重新裁定，授权活跃上报 / 领养 / 夜猫子 / 开学季进自动排程。
// **后者改变的是政策，不是性质**——领养的前置仍然只能靠伪造上报满足，
// 这个事实没有变，也不该因为它获准自动跑就被从 rationale 里抹掉。
// 把两者塞进同一个字段，就只能在"篡改事实陈述"与"无法放开政策"之间二选一。
//
// 分级语义：
//
//	preview  只读预览，不写上游                  → 性质上即可自动
//	claim    幂等领奖，重复调用无副作用           → 性质上即可自动
//	full     含**伪造活跃上报**语义或不可逆消耗   → 性质上需人担责；
//	                                               自动执行须 AutoAuthorized 显式授权
//
// ⚠️ `full` 单独一级的理由：它模拟"真实用户用了产品"这一事实，或造成不可逆
// 后果。自动跑它等于在无人要求的情况下批量生产伪造活跃记录；一旦被上游按风控
// 口径核对，受影响的是**整个账号池**，而不只是跑的那几个号。所以"是否允许自动"
// 必须是一个**显式的、可追溯的**决定（`AutoAuthorized` + `AutoAuthorization`
// 写明依据），而不是靠"没人记得为什么它不能自动"的默认沉默。

// GrowthTier 成长任务通道的三级合规分级。
type GrowthTier string

const (
	// GrowthTierPreview 只读预览：不发任何写请求。可自动。
	GrowthTierPreview GrowthTier = "preview"
	// GrowthTierClaim 幂等领奖：可自动。
	//
	// 定义（团队负责人 2026-09-23 明确）：**重复调用无副作用**。
	// 最坏情况是一次被上游幂等挡下的请求（如 409 duplicate、或只回 code=0 不带 data）。
	GrowthTierClaim GrowthTier = "claim"
	// GrowthTierFull 性质上**需人担责**：自动执行须逐条政策授权。
	//
	// 定义（团队负责人 2026-09-23 **扩展**，原定义只含"伪造上报"）：
	// **含伪造上报语义 *或* 不可逆消耗**。
	//
	// 判据：**该动作一旦执行就无法撤销 / 回退**——消耗次数、送出积分、
	// 提交不可逆请求。两类后果都需要有人在场担责，所以都不进自动排程。
	//
	// 为什么必须扩展：抽奖 `lottery/draw` **既不幂等**（每次都新 `client_token`
	// 以确保真抽，且次数不可恢复）、**又不含伪造上报**——按原定义它两头都不属于，
	// 会出现一个**分级空档**（既不能自动、也没有理由说它仅手动）。
	// 扩展后它归 full，且理由自洽：消耗不可逆。
	GrowthTierFull GrowthTier = "full"
)

// AutoSchedulable 报告该**性质**是否本就允许自动执行（不含政策授权）。
//
// ⚠️ 这不是"是否进自动排程"的最终判据——那是 `AutoRunnable()`（性质 **或**
// 显式政策授权）。本函数只回答性质问题："这个动作是否天然无需人在场"。
// 用它去决定排程会把"用户已授权的 full 通道"错误地挡在外面。
//
// 这是**性质**的唯一判据来源：调度器与端点都不得自己判 `tier != GrowthTierFull`，
// 而是调 `AutoRunnable()`（它内部会用到本函数）。这样以后若新增一个级别，
// 只需改这里，不必去各处补条件。
func (t GrowthTier) IsValid() bool {
	switch t {
	case GrowthTierPreview, GrowthTierClaim, GrowthTierFull:
		return true
	default:
		return false
	}
}

// String 便于日志与端点回执展示分级（回执带上级别，运维一眼能确认跑的是什么）。
func (t GrowthTier) String() string { return string(t) }

func (t GrowthTier) AutoSchedulable() bool {
	switch t {
	case GrowthTierPreview, GrowthTierClaim:
		return true
	case GrowthTierFull:
		// 性质上需人担责。是否自动由通道的 AutoAuthorized 政策字段决定。
		return false
	default:
		// 未知分级一律**不自动**：fail-closed。新增分级时忘记更新本函数，
		// 后果是"该通道不进排程"（可发现、可补救），而不是"悄悄进了排程"。
		return false
	}
}

// 成长任务通道标识（平台功能注册键 + 调度分支键，两处共用，避免字符串漂移）。
//
// ⚠️ 分级按**动作性质**，不是按通道整体（团队负责人 2026-09-22 裁定）。
// 用户的三级分级定义的是"动作"（只读 / 幂等领奖 / 伪造上报或不可逆消耗），
// 所以同一业务域里性质不同的动作要**拆成不同的键**：猫猫旅行的
// 「查状态」(preview) /「派出·领奖」(claim) /「领养」(full) 是三件不同的事，
// 整条按 full 归档会让"旅行领奖"这个纯幂等动作白白失去自动化能力。
// —— 动作级键见下方分组。
const (
	// CodeBuddyGrowthChannelTravel 猫猫旅行（C 通道，**业务域聚合键**）。
	// 仅用于标识业务域；**排程请用下面的动作级键**。
	CodeBuddyGrowthChannelTravel = "travel"
	// CodeBuddyGrowthChannelStreak 连登奖励链的**幂等领奖部分**
	// （补签 / 兑换 / 礼包 / 补偿）。
	//
	// ⚠️ **抽奖已从本通道拆出**（见 CodeBuddyGrowthChannelLottery）：
	// 它不幂等（不可逆消耗），按扩展后的 full 定义归 full。
	// 把抽奖留在 claim 通道里会让整条通道的"可自动"结论变错。
	CodeBuddyGrowthChannelStreak = "streak"
	// CodeBuddyGrowthChannelNightCat 夜猫子（E 通道）。
	CodeBuddyGrowthChannelNightCat = "night_cat"
	// CodeBuddyGrowthChannelSchool 开学季（F 通道）。
	CodeBuddyGrowthChannelSchool = "school"
	// CodeBuddyGrowthChannelTrial 国际版 trial 加油包（G 通道）。
	CodeBuddyGrowthChannelTrial = "trial"
)

// 动作级通道键（"按动作拆"的落地）。
const (
	// CodeBuddyGrowthChannelTravelStatus 查旅行状态：只读。
	CodeBuddyGrowthChannelTravelStatus = "travel_status"
	// CodeBuddyGrowthChannelTravelRun 派出 + 到站领奖：幂等领奖。
	CodeBuddyGrowthChannelTravelRun = "travel_run"
	// CodeBuddyGrowthChannelAdopt 领养（buddy/first）：依赖伪造的 chat_5 门槛，
	// 且到达门槛后直接送 300 分。
	CodeBuddyGrowthChannelAdopt = "adopt"

	// CodeBuddyGrowthChannelLottery 抽奖（lottery/draw）：**不可逆消耗**。
	//
	// 从 streak 拆出来的理由（团队负责人 2026-09-23 裁定）：
	//   - 它**不幂等**：参考实现 `scheduler.go:637` 原文「client_token 每次 draw
	//     必须新键（security-relevant）」——该键的用途是**确保每次都真抽**；
	//   - 抽一次消耗一次次数且**不可恢复**；
	//   - 全仓无 `lotteryMu|lotteryDrew|drewToday` → 抽奖**没有**任何去重机制。
	//
	// 归 full 的判据是"**执行后不可撤销**"（扩展后的 full 定义），
	// 不是"含伪造上报"——它并不伪造上报。这个区别写在这里，
	// 免得下一个人看到"抽奖归 full"时以为是笔误。
	CodeBuddyGrowthChannelLottery = "lottery"
)

// CodeBuddyGrowthChannelSpec 一条成长通道的**分级契约**。
//
// 只承载合规相关的部分（键 + 性质 + 政策 + 理由）。展示文案与默认时间窗属呈现/调度
// 关注点，留在 service 侧的平台功能注册里（A4 已确立的形态）——本包不能反向
// import service 去复用 `service.TimeOfDay`，为它另造一个同形类型又会让两处
// 时间语义有漂移空间，所以干脆不在这里表达时间。
//
// Tier 是**性质声明**；调度器只认 `AutoRunnable()`（性质 **或** 政策授权）。
type CodeBuddyGrowthChannelSpec struct {
	// Key 通道标识（= 平台功能注册键）。
	Key string
	// Tier 合规分级（动作性质，客观事实，不随政策变）。
	Tier GrowthTier
	// Rationale 分级理由（写给人看，也是"为什么它是什么性质"的存档）。
	//
	// ⚠️ 即便某通道获准自动执行，这里**仍要如实写明它的性质**——
	// 获准自动不等于性质改变。抹掉"含伪造上报"这个事实会让下一个人
	// 失去判断依据（他可能以为自动跑它无风险）。
	Rationale string
	// AutoAuthorized 用户是否已**明确授权**本通道进自动排程。
	//
	// 只对 Tier=full 有独立意义：preview/claim 性质上即可自动，无需逐条授权。
	// full 通道必须显式置 true 才会被 `AutoRunnable()` 放行——这是"默认拒绝、
	// 授权才开"：新增一个 full 级通道时，它默认进不了排程，
	// 直到有人写下授权依据（AutoAuthorization）。
	AutoAuthorized bool
	// AutoAuthorization 授权依据（谁、何时、为什么）。
	//
	// 与 AutoAuthorized 配对：置位 true 却写不出依据，就是一条无法追溯的
	// "悄悄放开"。守它的是 `TestCodeBuddyGrowthAutoRunnableChannelsAreAuthorized`。
	// 对自带授权的通道（preview/claim）可留空。
	AutoAuthorization string
}

// AutoRunnable 报告本通道是否允许进自动排程（性质允许 **或** 政策已授权）。
//
// 这是**唯一**的判据来源：调度器、批量分发、管理端点都调它，
// 不得自己去比较 Tier 或读 AutoAuthorized。
//
// 语义是"或"而不是"且"：preview/claim 性质上就无需人在场（不必逐条授权）；
// full 通道性质上需要人担责，但用户可以**明确授权**它在无人时也跑。
func (s CodeBuddyGrowthChannelSpec) AutoRunnable() bool {
	return s.Tier.AutoSchedulable() || s.AutoAuthorized
}

// CodeBuddyGrowthChannelSpecs 全部成长通道的声明式规格（顺序稳定，供注册与遍历）。
//
// ## 逐通道分级与理由（用户裁定的落地）
//
// **分级按动作性质**（团队负责人 2026-09-22 裁定）——同一业务域里性质不同的
// 动作拆成不同键，不整条按最严级别归档：
//
//   - travel_status（C-查状态）：**preview**。只读。
//   - travel_run（C-派出+领奖）：**claim**。派出每日限一次、领奖按 record_id，
//     上游均幂等，重复调用无副作用。
//   - adopt（C-领养）：**full**。前置 `chat_5` 只能靠 `/v2/report` 伪造 5 轮对话
//     满足，且到达门槛后直接送 300 分——"猫会自己出现"不是真实发生的事。
//     **2026-09-29 用户授权自动执行**（见 AutoAuthorization）。
//   - streak（D，**幂等领奖部分**）：**claim**。补签（有卡才补、无卡静默）、
//     兑换（409 幂等）、礼包/补偿（有则领）——全部是幂等领奖。
//   - lottery（D-抽奖）：**full**。**不可逆消耗**——抽一次消耗一次次数且不可恢复，
//     且每次 draw 必须新 `client_token`（确保真抽），所以**不幂等**。
//     它**不含伪造上报**；归 full 的判据是「执行后不可撤销」（2026-09-23 扩展定义）。
//     **仍为仅手动**：用户 2026-09-29 明确排除（一次调用会抽光全部次数，
//     无人看管时不可接受）。
//   - night_cat（E）：**full**。没有任何"领奖"端点，唯一的动作就是发
//     `chat_request_send`（mode=night）去点亮任务——**纯伪造活跃上报**。
//     **2026-09-29 用户授权自动执行**（见 AutoAuthorization）。
//   - school（F）：**full**。同类：完成判据靠伪造 chat_request_send 循环上报
//     （`school_open_day_2026.py:592` 原文），`share-complete` 同样是伪造"已分享"。
//     结论：**要拿奖就得先点亮，点亮就得伪造**，无法只做 claim 那一半。
//     **2026-09-29 用户授权自动执行**（见 AutoAuthorization）。
//   - trial（G）：**claim**。POST 一次幂等领取（已领返回 14051 幂等码）。
//
// 自动执行的判据是 `AutoRunnable()`（性质天然可自动 **或** 已获政策授权）。
// 守它的两件东西：`AutoRunnable()` 的"或"语义（full 需显式授权），
// 加上 `CodeBuddyGrowthAutoRunnableChannelKeys()` 这一**唯一**入口——
// 调度器只能通过后者取通道列表（不得遍历本表自己过滤）。
//
// 授权缺口由 `TestCodeBuddyGrowthAutoRunnableChannelsAreAuthorized` 兜底：
// full 通道若要进自动排程，必须同时写明 AutoAuthorization 依据。
//
// ⚠️ **抽奖已单列为 full 级通道**（`lottery`，2026-09-23 裁定）：
// 判据是「执行后不可撤销」。**不要**把它挪回 streak——一条通道的级别取决于
// 其**最严**成员，挪回去会让整条 streak 通道的"可自动"结论变错。
// 它也**仍然未获授权**（用户 2026-09-29 明确排除自动抽奖）。
var CodeBuddyGrowthChannelSpecs = []CodeBuddyGrowthChannelSpec{
	{
		Key:       CodeBuddyGrowthChannelTravelStatus,
		Tier:      GrowthTierPreview,
		Rationale: "只读查询旅行状态，不写上游",
	},
	{
		Key:       CodeBuddyGrowthChannelTravelRun,
		Tier:      GrowthTierClaim,
		Rationale: "派出（每日限一次）与到站领奖（按 record_id）均幂等，重复调用无副作用",
	},
	{
		Key:  CodeBuddyGrowthChannelAdopt,
		Tier: GrowthTierFull,
		Rationale: "前置 chat_5 只能靠伪造活跃上报满足，且达标后直接送 300 分" +
			"——属「猫会自己出现」式的伪造事实。性质未因授权而改变：本通道自动跑时" +
			"仍会先生成伪造活跃记录来满足前置。",
		AutoAuthorized: true,
		AutoAuthorization: "用户 2026-09-29 裁定「开活跃上报+领养/夜猫/开学季」，" +
			"授权本通道进自动排程；抽奖明确排除。",
	},
	{
		Key:  CodeBuddyGrowthChannelStreak,
		Tier: GrowthTierClaim,
		Rationale: "补签（上游对 target_date 幂等）、兑换（409 幂等）、" +
			"礼包/补偿（有则领）——全部是幂等领奖，重复调用无副作用。" +
			"⚠️ 抽奖已拆出为独立通道（见 lottery）",
	},
	{
		Key:  CodeBuddyGrowthChannelLottery,
		Tier: GrowthTierFull,
		Rationale: "**不可逆消耗**：抽一次消耗一次次数且不可恢复；" +
			"每次 draw 必须新 client_token（scheduler.go:637「security-relevant」= 确保每次都真抽），" +
			"所以它**不幂等**。归 full 的判据是「执行后不可撤销」，不是伪造上报——" +
			"抽奖并不伪造上报，别把它与 night_cat 混为一谈。" +
			"**未获自动授权**：一次调用会抽光当前全部次数且不可恢复，" +
			"无人看管时不可接受（用户 2026-09-29 明确排除）。",
		// AutoAuthorized 保持 false：见上方 Rationale 与用户裁定。
	},
	{
		Key:  CodeBuddyGrowthChannelNightCat,
		Tier: GrowthTierFull,
		Rationale: "唯一动作是伪造 chat_request_send 点亮任务（无领奖端点）。" +
			"性质未因授权而改变：自动跑时仍在生成伪造活跃记录。",
		AutoAuthorized: true,
		AutoAuthorization: "用户 2026-09-29 裁定「开活跃上报+领养/夜猫/开学季」，" +
			"授权本通道进自动排程；抽奖明确排除。" +
			"执行仍受夜间窗口（23:00–08:00 CST）与当日去重双重约束。",
	},
	{
		Key:  CodeBuddyGrowthChannelSchool,
		Tier: GrowthTierFull,
		Rationale: "完成判据靠伪造活跃上报循环触发，share-complete 亦为伪造；要拿奖必先点亮。" +
			"性质未因授权而改变：自动跑时仍在生成伪造活跃记录与伪造分享事实。",
		AutoAuthorized: true,
		AutoAuthorization: "用户 2026-09-29 裁定「开活跃上报+领养/夜猫/开学季」，" +
			"授权本通道进自动排程；抽奖明确排除。" +
			"活动下线（上游 41000 等）时静默跳过，不重试、不记账号故障。",
	},
	{
		Key:       CodeBuddyGrowthChannelTrial,
		Tier:      GrowthTierClaim,
		Rationale: "一次性幂等领取（已领返回 14051 幂等码）",
	},
}

// CodeBuddyGrowthChannelSpecByKey 按通道键取规格。
func CodeBuddyGrowthChannelSpecByKey(key string) (CodeBuddyGrowthChannelSpec, bool) {
	trimmed := strings.TrimSpace(key)
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.Key == trimmed {
			return spec, true
		}
	}
	return CodeBuddyGrowthChannelSpec{}, false
}

// CodeBuddyGrowthChannelTier 取通道的合规分级；未知通道返回 (空, false)。
func CodeBuddyGrowthChannelTier(key string) (GrowthTier, bool) {
	spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
	if !ok {
		return "", false
	}
	return spec.Tier, true
}

// CodeBuddyGrowthAutoRunnableChannelKeys 返回**允许进自动排程**的通道键（顺序稳定）。
//
// 这是调度器取通道列表的**唯一入口**。调度器不得遍历 CodeBuddyGrowthChannelSpecs
// 自己过滤——一旦有人图省事写成 `for _, spec := range specs`，未经授权的 `full`
// 通道就会被静默纳入排程（正是本任务要守住的那条线）。走这个函数，
// 未授权的 `full` 在类型层面就进不来。
//
// 判据是 `AutoRunnable()`（性质天然可自动 **或** 已获政策授权），**不是**
// `Tier.AutoSchedulable()`——后者只看性质，会把已授权的 full 通道漏掉。
//
// 返回的是副本：调用方改它不会污染注册表。
func CodeBuddyGrowthAutoRunnableChannelKeys() []string {
	keys := make([]string, 0, len(CodeBuddyGrowthChannelSpecs))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.AutoRunnable() {
			keys = append(keys, spec.Key)
		}
	}
	return keys
}

// CodeBuddyGrowthChannelKeyAllowsAutoSchedule 报告某个通道键是否允许进自动排程。
//
// 未注册的通道键返回 false（fail-closed）：调度器遇到不认识的通道时不停下，
// 而不是"不认识就照跑"。
func CodeBuddyGrowthChannelKeyAllowsAutoSchedule(key string) bool {
	spec, ok := CodeBuddyGrowthChannelSpecByKey(key)
	if !ok {
		return false
	}
	return spec.AutoRunnable()
}

// CodeBuddyGrowthAuthorizedFullChannelKeys 返回**已获授权自动执行**的 full 级通道键。
//
// 用途：让"哪些 full 通道被放开了"成为可枚举的事实，供守卫测试逐个点名核对，
// 也供运维核对授权面。未获授权的 full 通道（当前只有 lottery）不会出现在这里。
func CodeBuddyGrowthAuthorizedFullChannelKeys() []string {
	keys := make([]string, 0, len(CodeBuddyGrowthChannelSpecs))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.Tier == GrowthTierFull && spec.AutoAuthorized {
			keys = append(keys, spec.Key)
		}
	}
	return keys
}

// CodeBuddyGrowthNightWindowChannelKeys 需要**夜间窗口**才能执行的通道键。
//
// 语义：这些通道的动作只在夜间时段（23:00–08:00 CST，跨零点）对上有效——
// 夜猫子任务的判据含 `mode=night`，日间发出去上游不认（通道内部也会自查窗口
// 并返回 skip）。所以调度器把它们从日间批次里摘出来，单独在夜间窗口跑。
//
// ⚠️ 与授权是**两件事**：列在这里不代表已授权自动（授权看 `AutoAuthorized`）。
// 调度器取列表时仍要经 `AutoRunnable()` 过滤——两个条件是"与"关系，
// 夜窗集合只能**进一步收窄**，不可能放进未授权的通道。
var CodeBuddyGrowthNightWindowChannelKeys = []string{
	CodeBuddyGrowthChannelNightCat,
}

// CodeBuddyGrowthIsNightWindowChannel 报告通道是否属夜间窗口专属。
func CodeBuddyGrowthIsNightWindowChannel(key string) bool {
	trimmed := strings.TrimSpace(key)
	for _, k := range CodeBuddyGrowthNightWindowChannelKeys {
		if k == trimmed {
			return true
		}
	}
	return false
}

// CodeBuddyGrowthDaytimeAutoRunnableChannelKeys 返回**日间窗口**可跑的通道键：
// 已授权自动 **且** 不属夜间窗口专属。
//
// 这是日间批次的取列表入口（夜间批次用 `CodeBuddyGrowthNightAutoRunnableChannelKeys`）。
// 两者都从 `AutoRunnable()` 出发，所以未授权的 full 通道在**两个入口都进不来**。
func CodeBuddyGrowthDaytimeAutoRunnableChannelKeys() []string {
	keys := make([]string, 0, len(CodeBuddyGrowthChannelSpecs))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.AutoRunnable() && !CodeBuddyGrowthIsNightWindowChannel(spec.Key) {
			keys = append(keys, spec.Key)
		}
	}
	return keys
}

// CodeBuddyGrowthNightAutoRunnableChannelKeys 返回**夜间窗口**可跑的通道键：
// 已授权自动 **且** 属夜间窗口专属。
func CodeBuddyGrowthNightAutoRunnableChannelKeys() []string {
	keys := make([]string, 0, len(CodeBuddyGrowthNightWindowChannelKeys))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.AutoRunnable() && CodeBuddyGrowthIsNightWindowChannel(spec.Key) {
			keys = append(keys, spec.Key)
		}
	}
	return keys
}

// --- F 通道「活动下线」语义 ---

// codeBuddyGrowthOfflineBizCodes 活动下线/未开启的上游业务码。
//
// ⚠️ 按**上游返回的错误语义**判断，不硬编码活动名（任务书 §6.5 的硬要求）：
// 上游对"活动未开始或已结束"回 41000，对"任务暂不可领取（未完成或已领）"回
// 40901。两者都表示**活动本身不在可服务状态**，不是账号故障。
//
// 依据：参考实现 workbuddy2api `scripts/school_open_day_2026.py:_interpret_failure`
// 原文——`41000 → "41000 活动未开始或已结束"`；`40901 → "40901 任务暂不可领取
// （未完成或已领）"`。该文件同时把这两种归为 **非 auth 失败**（返回 is_auth=False），
// 即"不应据此惩罚账号"。
//
// ⚠️ 该名单**只含活动生命周期**相关的码。参考实现同文件里另有两个码
// （83400 验证码已过期 / 50300 学生认证服务暂不可用）——那是"需人工完成某环节"，
// 语义不同（活动是活的，缺的是真人动作），所以拆成下面独立的
// `codeBuddyGrowthManualOnlyBizCodes`，不混进"下线"这一类。
// 两者在调用方的处理动作相同（静默跳过、不重试、不记故障），但**日志文案与
// 归类不能混**——把"要人工认证"说成"活动下线"会让运维直接放弃排查。
var codeBuddyGrowthOfflineBizCodes = map[int]string{
	41000: "活动未开始或已结束（活动已下线）",
	40901: "任务暂不可领取（未完成或已领）",
}

// CodeBuddyGrowthOfflineReason 报告某个业务码是否表示"活动不可服务"，是的返回可读原因。
//
// 用途：F 通道识别到下线后**静默跳过**——不重试、不记账号故障、不刷 WARN。
// 返回非空串即"应当静默"。
//
// 注意入参是**数值**码：字符串形态（如 "11-128"）由调用方先用
// CodeBuddyNumericBizCode 解出 ok 标志，解不出时不走本函数（未知码按普通失败处理
// 更安全——详见调用点注释）。
func CodeBuddyGrowthOfflineReason(numericCode int) string {
	return codeBuddyGrowthOfflineBizCodes[numericCode]
}

// codeBuddyGrowthManualOnlyBizCodes 需要**人工完成真实业务动作**的上游码。
//
// 与"活动下线"分开成一类：它们的共同点是**上游没有故障、账号也没问题**，
// 缺的是"真人在产品里点一下/认证一下"。所以既不该重试（重试一万次也不会
// 变成做过），也不该记成账号故障（否则会把好号标记成坏的）。
//
// 依据：参考实现 `scripts/school_open_day_2026.py` 的 `_interpret_failure` 与
// 任务表 `manual` 类别（原文把 student-verify 标为"人工环节（学生认证/验证码/
// 审核），--run 一律跳过"）。
var codeBuddyGrowthManualOnlyBizCodes = map[int]string{
	83400: "验证码已过期（该环节需人工完成）",
	50300: "学生认证服务暂不可用（该环节需人工完成）",
}

// CodeBuddyGrowthManualOnlyReason 报告某码是否表示"该环节需人工完成"。
// 返回非空即应静默跳过：不重试、不记账号故障。
func CodeBuddyGrowthManualOnlyReason(numericCode int) string {
	return codeBuddyGrowthManualOnlyBizCodes[numericCode]
}

// --- E 通道：夜猫子时段的夜间事件形状 ---

// CodeBuddyNightCatEventMode 夜猫事件 mode 字段值。
//
// 依据：参考实现 `scripts/task_runner.py:_event_for_kind` 原文
// `mode = "night" if kind == "cat" else "craft"`——普通 chat 用 craft，
// 夜猫子用 night。**不是笔误**：夜猫任务的判据含 mode 维度。
const CodeBuddyNightCatEventMode = "night"

// CodeBuddyNightCatModelID / CodeBuddyNightCatModelName 夜猫事件所用模型字段。
//
// 依据同上：`task_runner.py` 里 `kind in ("glmchat", "cat")` 共用
// GLM-5.2 的 model_id/name（该端点只记活跃，不校验模型一致性）。
const (
	CodeBuddyNightCatModelID   = "glm-5.2"
	CodeBuddyNightCatModelName = "GLM-5.2"
)

// CodeBuddyNightCatTargetCount 夜猫子任务需要的事件条数（参考实现 target=3）。
const CodeBuddyNightCatTargetCount = 3

// NewCodeBuddyNightCatEvent 构造一条夜猫子（`black_cat`）事件。
//
// 与 NewCodeBuddyActivityEvent 同形状（同一个 `chat_request_send` 事件），
// 差异只在两个字段：Mode="night"、模型字段换成 GLM-5.2。**复用同一构造函数**
// 再覆写，而不是另抄一份完整字段表——参考实现 `scripts/task_runner.py:_event_for_kind`
// 里两者（`kind in ("chat","glmchat","cat")` 分支）本来就共用同一份字段列表，
// 只是按 kind 换 mode 与模型；抄第二份必然与第一份漂移。
//
// ⚠️ 上游边界（读代码得出的推断，**未执行验证**）：`requestId` 本实现给的是
// `<conversationId>-r<i>`（`CodeBuddyActivityRequestID` 的形态），而参考实现
// `task_runner.py` 在这一分支里 `"requestId": cid` 直接复用 conversationId。
// 服务端按实现注释"不校验一致性"，故本实现沿用 A5 已上线且已验证的 requestId
// 形态，不为一个未经证实的细节再引入一条分支。若将来实测该端点对 requestId
// 有额外要求，改这一处即可。
//
// 归属分级：本函数产出的事件用于 `full` 级通道（纯伪造活跃上报，无领奖端点），
// 调用方**只能**从手动入口到达。
func NewCodeBuddyNightCatEvent(userID, conversationID, requestID string, now time.Time) CodeBuddyActivityEvent {
	event := NewCodeBuddyActivityEvent(userID, conversationID, requestID, now)
	event.Mode = CodeBuddyNightCatEventMode
	event.RequestModelID = CodeBuddyNightCatModelID
	event.RequestModelName = CodeBuddyNightCatModelName
	return event
}

// --- F 通道：开学季（小程序域）端点 ---

// CodeBuddySchoolMiniAppBase CN 小程序门户域。
//
// ⚠️ 与本包其它域**都不同**：school 走 `https://www.codebuddy.cn/portal/activity/school`
// （参考实现 `scripts/school_open_day_2026.py:46` 原文
// `SCHOOL = CLOUD_AGENT + "/portal/activity/school"`，其中 `CLOUD_AGENT =
// "https://www.codebuddy.cn"`）。而 growth 域走 chat base、report 走 billing base
// ——三个域三套，混用会 404。
const CodeBuddySchoolMiniAppBase = "https://www.codebuddy.cn"

// CodeBuddySchoolPortalPrefix 小程序门户前缀（与 base 拼接成 prefix path）。
const CodeBuddySchoolPortalPrefix = "/portal/activity/school"

// CodeBuddySchoolTasksPath 任务列表（只读）：GET，响应 data.tasks + data.in_period。
const CodeBuddySchoolTasksPath = CodeBuddySchoolPortalPrefix + "/tasks"

// CodeBuddySchoolConfigPath 抽奖盘面 + 余额（只读）：GET，响应 data.chance.balance。
const CodeBuddySchoolConfigPath = CodeBuddySchoolPortalPrefix + "/config"

// CodeBuddySchoolWheelDrawPath 转盘抽奖：POST {"draw_uuid": <随机 uuid>}。
const CodeBuddySchoolWheelDrawPath = CodeBuddySchoolPortalPrefix + "/wheel/draw"

// CodeBuddySchoolTaskViewedPath 拼接"接任务"路径：POST {prefix}/tasks/{code}/viewed。
func CodeBuddySchoolTaskViewedPath(taskCode string) string {
	return CodeBuddySchoolPortalPrefix + "/tasks/" + taskCode + "/viewed"
}

// CodeBuddySchoolTaskClaimPath 拼接"领任务奖励"路径：POST {prefix}/tasks/{code}/claim。
func CodeBuddySchoolTaskClaimPath(taskCode string) string {
	return CodeBuddySchoolPortalPrefix + "/tasks/" + taskCode + "/claim"
}

// CodeBuddySchoolShareCompletePath 分享任务完成上报：POST {prefix}/tasks/share-complete。
const CodeBuddySchoolShareCompletePath = CodeBuddySchoolPortalPrefix + "/tasks/share-complete"

// CodeBuddySchoolClientPlatform 小程序请求头 `X-Client-Platform` 取值。
//
// 依据：参考实现 task_runner.py 任务表注释原文——"小程序成长任务（growth 域小程序
// 限定）：列表/accept/claim 均需 X-Client-Platform: miniprogram"。
const CodeBuddySchoolClientPlatform = "miniprogram"

// GrowthChannelSpecForAPI 通道规格的**对外形态**（管理端点渲染用）。
//
// 与内部 `CodeBuddyGrowthChannelSpec` 分开：内部结构以后可能要加调度相关字段，
// 而对外契约不该跟着变。这里显式挑出"管理端需要知道的"几项。
type GrowthChannelSpecForAPI struct {
	Key string `json:"key"`
	// Tier 合规分级（preview / claim / full）= 动作**性质**。
	Tier string `json:"tier"`
	// AutoRunnable 是否允许自动排程（性质天然可自动，或已获政策授权）。
	AutoRunnable bool `json:"auto_runnable"`
	// AutoAuthorized 该通道是否**因政策授权**才可自动（即 full 级被放开）。
	//
	// 与 AutoRunnable 分开暴露，是为让管理端能如实区分"本来就无需人在场"
	// 与"性质需人担责、但用户已授权放开"——两者的风险画像不同，界面不该
	// 把它们显示成同一种状态。
	AutoAuthorized bool `json:"auto_authorized"`
	// AutoAuthorization 授权依据（谁、何时、为什么）；未授权为空。
	AutoAuthorization string `json:"auto_authorization,omitempty"`
	// Rationale 分级理由（写给人看）。
	Rationale string `json:"rationale"`
}

// GrowthChannelSpecsForAPI 返回全部通道的对外形态（顺序与内部声明一致）。
func GrowthChannelSpecsForAPI() []GrowthChannelSpecForAPI {
	out := make([]GrowthChannelSpecForAPI, 0, len(CodeBuddyGrowthChannelSpecs))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		out = append(out, GrowthChannelSpecForAPI{
			Key:               spec.Key,
			Tier:              spec.Tier.String(),
			AutoRunnable:      spec.AutoRunnable(),
			AutoAuthorized:    spec.AutoAuthorized,
			AutoAuthorization: spec.AutoAuthorization,
			Rationale:         spec.Rationale,
		})
	}
	return out
}
