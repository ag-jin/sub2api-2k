package codebuddy

// growth 域端点路径常量（批 6：成长任务链 C/D/E/F/G 五条通道）。
//
// ## 三个域，别混
//
// 本批涉及的上游主机有**三个**，混用会打到错的主机且测试看不出来
// （A5 已在 report 通道踩过一次：上报走 billing 域、streak 回读走 chat 域，
// 而 `testBaseURL` 会同时覆盖两边，导致"接错域"在测试里完全隐形）：
//
//	chat 域    copilot.tencent.com   → /activity/growth/*（旅行/连登/补签/兑换/抽奖）
//	billing 域 www.codebuddy.cn      → /v2/report、/billing/meter/{claim-gift,claim-compensation}、/billing/ide/trial
//	小程序门户 www.codebuddy.cn      → /portal/activity/school/*（见 CodeBuddySchool* 常量）
//
// ## 为什么 growth 域的路径**不做** realm 回落（L8 的边界）
//
// 参考实现 `internal/upstream/client.go:876-880` 原文：`/billing/meter/*` 族的
// "global 先无 /v2 再回落带 /v2"规律**仅作用于 get-user-resource / daily-checkin**，
// 且明确写着"report /v2/report 不参与，其他 billing 端点（growth 等）路径不含
// /billing/meter 前缀，走原常量不受影响"。
//
// 所以本文件里的路径一律**单元素**。这不是"忘了写回落"，是有意为之：
// 给不参与回落的端点加上回落候选，会在 global 账号上凭空多打一次注定 404 的请求。
// 回落的判定逻辑见 service 侧 `codeBuddyEndpointCandidates`——它按**显式白名单**
// 决定谁回落，而不是按路径前缀猜。

// --- C 猫猫旅行 + 领养（chat 域）---

// CodeBuddyGrowthStreakPath 连登端点。
//
// 与 A5 的 `CodeBuddyActivityStreakPath` 是**同一个上游端点**，所以这里显式做别名
// 而不是再写一遍字符串字面量——两份字面量一旦分叉（比如一方被改成带 /v2 前缀），
// 只会在线上表现为"其中一条链路莫名 404"。共用同一个常量，改名时一起改。
const CodeBuddyGrowthStreakPath = CodeBuddyActivityStreakPath

const (
	// CodeBuddyTravelStatusPath 查旅行状态：state(idle/traveling/arrived)、
	// daily_limit_reached、record_id、reward_credit。
	CodeBuddyTravelStatusPath = "/activity/growth/buddy/travel/status"
	// CodeBuddyTravelDepartPath 派出：{"location_id": n}。
	CodeBuddyTravelDepartPath = "/activity/growth/buddy/travel/depart"
	// CodeBuddyTravelClaimPath 领到站奖励：{"record_id": n}（**必须**带 record_id）。
	CodeBuddyTravelClaimPath = "/activity/growth/buddy/travel/claim"
	// CodeBuddyBuddyInfoPath 查猫档案：data.buddy 为 null 表示无猫。
	CodeBuddyBuddyInfoPath = "/activity/growth/buddy/info"
	// CodeBuddyBuddyFirstPath 领养第一只猫（无猫且过 chat_5 门槛时送 300 分）。
	CodeBuddyBuddyFirstPath = "/activity/growth/buddy/first"
	// CodeBuddyBuddyAgreementPath 同意领养协议（幂等，重复调用无副作用）。
	CodeBuddyBuddyAgreementPath = "/activity/growth/buddy/agreement"
)

// CodeBuddyTravelLocationID 默认派出地点。
//
// 取值 4（古镇客栈）：参考实现 `internal/scheduler/travel.go:16-17` 原文
// "travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，
// 无最优解"。也就是说这个数字**不影响收益**，只是需要一个合法值。
const CodeBuddyTravelLocationID = 4

// CodeBuddyBuddyThresholdMarker 领养门槛未达标的业务错误关键词。
//
// 依据：参考实现 `internal/upstream/travel.go:28` 原文
// `buddyTaskIncompleteMarker = "first_buddy task not completed yet"`，
// 在 HTTP 400 时出现（同文件:165 用 `strings.ToLower(ue.Msg)` 做包含匹配）。
//
// **这是预期行为，不是故障**：说明该账号的 `chat_5` 还没刷满 5 轮对话，
// 因此调用方应静默跳过并**当日不再重试**（门槛不会因重试而满足）。
const CodeBuddyBuddyThresholdMarker = "first_buddy task not completed yet"

// 旅行状态取值（data.state）。
const (
	CodeBuddyTravelStateIdle      = "idle"
	CodeBuddyTravelStateTraveling = "traveling"
	CodeBuddyTravelStateArrived   = "arrived"
)

// --- D 连登 / 补签 / 兑换 / 抽奖（chat 域）---

const (
	// CodeBuddyGrowthHeatmapPath 活跃地图：data.cells[]{date,score}，score==0 判漏签。
	CodeBuddyGrowthHeatmapPath = "/activity/growth/heatmap"
	// CodeBuddyGrowthMakeupCardUsePath 对指定日期补签：{"target_date":"2006-01-02"}。
	CodeBuddyGrowthMakeupCardUsePath = "/activity/growth/makeup-cards/use"
	// CodeBuddyGrowthRedeemPath 里程碑兑换：{"tier":"7d|14d|28d","client_token":"<新键>"}。
	CodeBuddyGrowthRedeemPath = "/activity/growth/redeem"
	// CodeBuddyGrowthLotteryChancesPath 抽奖次数余额：data.balance。
	CodeBuddyGrowthLotteryChancesPath = "/activity/growth/lottery/chances"
	// CodeBuddyGrowthLotteryDrawPath 抽一次奖：{"client_token":"<新键>"}。
	CodeBuddyGrowthLotteryDrawPath = "/activity/growth/lottery/draw"
)

// --- D 礼包 / 补偿（billing 域）---

const (
	// CodeBuddyClaimGiftPath 新手礼包（每号一次，已领返回业务错误）。
	//
	// ⚠️ 路径**确实**含 `/billing/meter/` 前缀，但**不参与** realm 回落族：
	// 参考实现 growth_bonus.go:123 走的是 `claimBillingCredit` → `billingJSON`
	// （固定 base + 固定 path），而不是 `billingMeterJSON(paths 候选序列)`。
	// 回落族是**显式列表**（billingMeterPaths / checkinMeterPaths 两个函数各管一个
	// 端点），不是按前缀匹配的规则。别看到前缀就加候选——理由见 service 侧
	// `codeBuddyEndpointCandidates` 的注释。
	CodeBuddyClaimGiftPath = "/billing/meter/claim-gift"
	// CodeBuddyClaimCompensationPath 活动补偿（有则领，无则业务错误）。
	CodeBuddyClaimCompensationPath = "/billing/meter/claim-compensation"
)

// --- G 国际版 trial（billing 域）---

// CodeBuddyTrialPath 一次性 trial 加油包（**仅 global** 账号有该端点）。
const CodeBuddyTrialPath = "/billing/ide/trial"

// --- 上游业务码：幂等 / 正常态判定 ---

// CodeBuddyTrialAlreadyClaimedBizCode trial 已领过的幂等码。
//
// 依据：参考实现 `internal/upstream/trial.go:18-21` 原文——"幂等码 14051 = 已领过
// （视为正常，非错误）"，且给出两种指纹形态（`code=14051` 与 `"code":14051`，
// 分别对应 HTTP 200 带业务码、以及 HTTP ≥400 把原始 body 塞进 Msg 两种路径）。
const CodeBuddyTrialAlreadyClaimedBizCode = 14051

// CodeBuddyTrialAlreadyClaimedMarker 幂等码的**文本**兜底指纹。
//
// 为什么需要文本形态：上游可能用 HTTP 4xx 承载该幂等码（参考实现专门为这种形态
// 准备了 `"code":14051` 这个 marker）。此时我们解信封拿到的 code 可能仍是 14051
// （数字），但若上游把它塞进 msg 文本里，就只有靠文本匹配才能认出幂等。
// 两条路径都覆盖，才叫"已领不算失败"。
const CodeBuddyTrialAlreadyClaimedMarker = "14051"

// --- 幂等键（client_token）---

// CodeBuddyGrowthClientTokenPrefixRedeem 兑换幂等键前缀。
// 形态 `<prefix>-<tier>-<uuid>`（参考实现是 `redeem-<tier>-<uuid>`，此处保持可读前缀）。
const CodeBuddyGrowthClientTokenPrefixRedeem = "wb2api-redeem"

// CodeBuddyGrowthClientTokenPrefixDraw 抽奖幂等键前缀。
const CodeBuddyGrowthClientTokenPrefixDraw = "wb2api-draw"

// CodeBuddyGrowthClientTokenPrefixSchoolDraw 开学季转盘幂等键前缀。
const CodeBuddyGrowthClientTokenPrefixSchoolDraw = "wb2api-school-draw"

// CodeBuddyGrowthRedeemTiers 连登里程碑档位（由高到低，挑最高可领档）。
//
// 依据：参考实现 growth_reward.go:9-10 原文——"7d/14d/28d 三个档位，同月每档各可领
// 一次"。**顺序很重要**：挑档时从高到低找第一个"已达标且未领"的档，这样连跳两天
// 时优先领高价值档（参考实现 growthEligibleTier 即此顺序）。
var CodeBuddyGrowthRedeemTiers = []string{"28d", "14d", "7d"}
