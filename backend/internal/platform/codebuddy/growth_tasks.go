package codebuddy

import (
	"strings"
	"time"
)

// 成长任务链（批 6）的三级合规分级与通道规格。
//
// ## 为什么分级信息放在 platform 包而不是 service 包
//
// 分级是**用户亲自裁定的合规边界**，不是调度实现细节。把它放在这里，好处是
// 调度器、管理端点、测试三方读的是**同一份声明**——任何一方想"临时放宽"都必须
// 改这个文件，而改这里会被 `TestCodeBuddyGrowthAutoSchedulableChannelsAreNeverFull`
// 与 `TestCodeBuddyGrowthChannelRegistryIsInternallyConsistent` 同时拦住。
//
// 分级语义（用户 2026-09-22 裁定）：
//
//	preview  只读预览，不写上游                    → 可自动
//	claim    幂等领奖，重复调用无副作用             → 可自动
//	full     含**伪造活跃上报**语义的活动行为        → 仅手动，不得进任何自动排程
//
// `full` 之所以要单独一级，是因为它模拟"真实用户用了产品"这一事实。自动跑它
// 等于在无人要求的情况下批量生产伪造活跃记录；一旦被上游按风控口径核对，
// 受影响的是**整个账号池**，而不只是跑的那几个号。所以它必须有一个人在场的
// 触发点（管理端点手动调用），并且在代码层面对自动排程**不可达**。

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
	// GrowthTierFull **仅手动**，自动排程必须绕开。
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

// AutoSchedulable 报告该分级是否允许出现在自动排程里。
//
// 这是**唯一**的判据来源：调度器不自己判 `tier != GrowthTierFull`，而是调这个
// 函数。这样以后若新增一个级别（例如把某类"只读但耗配额"的操作单列），
// 只需改这里，不必去调度器里补一个条件。
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
// 只承载合规相关的部分（键 + 分级 + 理由）。展示文案与默认时间窗属呈现/调度
// 关注点，留在 service 侧的平台功能注册里（A4 已确立的形态）——本包不能反向
// import service 去复用 `service.TimeOfDay`，为它另造一个同形类型又会让两处
// 时间语义有漂移空间，所以干脆不在这里表达时间。
//
// Tier 是**声明**；调度器只认声明，不认通道名。
type CodeBuddyGrowthChannelSpec struct {
	// Key 通道标识（= 平台功能注册键）。
	Key string
	// Tier 合规分级。
	Tier GrowthTier
	// Rationale 分级理由（写给人看，也是"为什么不能提升到可自动"的存档）。
	Rationale string
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
//   - streak（D，**幂等领奖部分**）：**claim**。补签（有卡才补、无卡静默）、
//     兑换（409 幂等）、礼包/补偿（有则领）——全部是幂等领奖。
//   - lottery（D-抽奖）：**full**。**不可逆消耗**——抽一次消耗一次次数且不可恢复，
//     且每次 draw 必须新 `client_token`（确保真抽），所以**不幂等**。
//     它**不含伪造上报**；归 full 的判据是「执行后不可撤销」（2026-09-23 扩展定义）。
//   - night_cat（E）：**full**。没有任何"领奖"端点，唯一的动作就是发
//     `chat_request_send`（mode=night）去点亮任务——**纯伪造活跃上报**。
//   - school（F）：**full**。同类：完成判据靠伪造 chat_request_send 循环上报
//     （`school_open_day_2026.py:592` 原文），`share-complete` 同样是伪造"已分享"。
//     结论：**要拿奖就得先点亮，点亮就得伪造**，无法只做 claim 那一半。
//   - trial（G）：**claim**。POST 一次幂等领取（已领返回 14051 幂等码）。
//
// ⚠️ 这张表里凡 `Tier == GrowthTierFull` 的通道，**不得**出现在任何自动排程中。
// 守它的是两件东西：AutoSchedulable() 的 fail-closed 语义，加上
// `CodeBuddyGrowthAutoSchedulableChannelKeys()` 这一**唯一**入口——
// 调度器只能通过后者取通道列表。
//
// ⚠️ **抽奖已单列为 full 级通道**（`lottery`，2026-09-23 裁定）：
// 判据是「执行后不可撤销」。**不要**把它挪回 streak——一条通道的级别取决于
// 其**最严**成员，挪回去会让整条 streak 通道的"可自动"结论变错。
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
			"——属「猫会自己出现」式的伪造事实",
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
			"抽奖并不伪造上报，别把它与 night_cat 混为一谈",
	},
	{
		Key:       CodeBuddyGrowthChannelNightCat,
		Tier:      GrowthTierFull,
		Rationale: "唯一动作是伪造 chat_request_send 点亮任务（无领奖端点）",
	},
	{
		Key:       CodeBuddyGrowthChannelSchool,
		Tier:      GrowthTierFull,
		Rationale: "完成判据靠伪造活跃上报循环触发，share-complete 亦为伪造；要拿奖必先点亮",
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

// CodeBuddyGrowthAutoSchedulableChannelKeys 返回**允许进自动排程**的通道键（顺序稳定）。
//
// 这是调度器取通道列表的**唯一入口**。调度器不得遍历 CodeBuddyGrowthChannelSpecs
// 自己过滤——一旦有人图省事写成 `for _, spec := range specs`，`full` 通道就会
// 被静默纳入排程（正是本任务要守住的那条线）。走这个函数，`full` 在类型层面
// 就进不来。
//
// 返回的是副本：调用方改它不会污染注册表。
func CodeBuddyGrowthAutoSchedulableChannelKeys() []string {
	keys := make([]string, 0, len(CodeBuddyGrowthChannelSpecs))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		if spec.Tier.AutoSchedulable() {
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
	tier, ok := CodeBuddyGrowthChannelTier(key)
	if !ok {
		return false
	}
	return tier.AutoSchedulable()
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
// 而对外契约不该跟着变。这里显式挑出"管理端需要知道的"四项。
type GrowthChannelSpecForAPI struct {
	Key string `json:"key"`
	// Tier 合规分级（preview / claim / full）。
	Tier string `json:"tier"`
	// AutoRunnable 是否允许自动排程（false = full 级，仅手动）。
	AutoRunnable bool `json:"auto_runnable"`
	// Rationale 分级理由（写给人看）。
	Rationale string `json:"rationale"`
}

// GrowthChannelSpecsForAPI 返回全部通道的对外形态（顺序与内部声明一致）。
func GrowthChannelSpecsForAPI() []GrowthChannelSpecForAPI {
	out := make([]GrowthChannelSpecForAPI, 0, len(CodeBuddyGrowthChannelSpecs))
	for _, spec := range CodeBuddyGrowthChannelSpecs {
		out = append(out, GrowthChannelSpecForAPI{
			Key:          spec.Key,
			Tier:         spec.Tier.String(),
			AutoRunnable: spec.Tier.AutoSchedulable(),
			Rationale:    spec.Rationale,
		})
	}
	return out
}
