package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
)

// 成长任务链的上游端点解析（批 6，A6）。
//
// ## 这个文件存在的唯一理由：把"哪个端点走哪个域"变成**显式白名单**
//
// realm 回落的规律（L8）是：`/billing/meter/*` 族里**只有两个端点**参与
// global 的"先无 /v2、404 再回落带 /v2"，而且 **CN 的候选序列只有一个元素**。
// 参考实现 `internal/upstream/client.go:876-890` 是用**两个独立函数**
// （`billingMeterPaths` / `checkinMeterPaths`）表达这件事的，不是一条前缀规则。
//
// 为什么不能用前缀判别：本批的 `/billing/meter/claim-gift` 与
// `/billing/meter/claim-compensation` **路径里确实带该前缀**，却明确**不参与**回落
// （参考实现走 `claimBillingCredit` → 固定 base + 固定 path）。写成
// `strings.HasPrefix(path, "/billing/meter")` 会把这两个端点也塞进回落族，
// 在 global 账号上凭空多打一次注定 404 的请求——而这类错误在测试里看不出来
// （`testBaseURL` 同时覆盖所有域，候选多一个也照样"通过"）。
//
// 所以：**按端点身份白名单**，不按路径形状。新增端点时若想让它回落，必须
// 在这里显式加一行，并在注释里写清依据。

// codeBuddyEndpointDomain 端点所属域名族。
type codeBuddyEndpointDomain int

const (
	// codeBuddyDomainChat chat 域（copilot.tencent.com / www.workbuddy.ai）：
	// growth 域的全部 `/activity/growth/*` 端点。
	codeBuddyDomainChat codeBuddyEndpointDomain = iota
	// codeBuddyDomainBilling billing 域（www.codebuddy.cn / www.workbuddy.ai）：
	// `/v2/report`、`/billing/meter/{claim-gift,claim-compensation}`、`/billing/ide/trial`。
	codeBuddyDomainBilling
	// codeBuddyDomainSchoolMiniApp 小程序门户域（CN 固定 www.codebuddy.cn）：
	// `/portal/activity/school/*`。**不按 realm 分**——参考实现里 school 脚本
	// 直接打 CN 门户（`scripts/school_open_day_2026.py:46`），全球版无该活动。
	codeBuddyDomainSchoolMiniApp
)

// codeBuddyGrowthEndpoint 一个已解析的上游端点：域 + 路径候选序列。
type codeBuddyGrowthEndpoint struct {
	Domain codeBuddyEndpointDomain
	// Paths 按尝试顺序排列的路径候选。**单元素表示不参与 realm 回落**。
	Paths []string
}

// codeBuddyEndpointCandidate 由路径解析出的候选（域 + 具体路径），供调用方拼接 base。
type codeBuddyEndpointCandidate struct {
	Domain codeBuddyEndpointDomain
	Path   string
}

// codeBuddyGrowthEndpoints 端点身份 → 解析规则的**唯一**白名单。
//
// 键是 platform 包里的路径常量（= 端点身份）。用常量当键而不是字符串字面量，
// 是为了让"常量改了但白名单没改"变成编译错误。
var codeBuddyGrowthEndpoints = map[string]codeBuddyGrowthEndpoint{
	// --- chat 域：growth 全族，单路径不回落 ---
	codebuddy.CodeBuddyGrowthStreakPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyGrowthStreakPath},
	},
	codebuddy.CodeBuddyGrowthHeatmapPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyGrowthHeatmapPath},
	},
	codebuddy.CodeBuddyGrowthMakeupCardUsePath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyGrowthMakeupCardUsePath},
	},
	codebuddy.CodeBuddyGrowthRedeemPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyGrowthRedeemPath},
	},
	codebuddy.CodeBuddyGrowthLotteryChancesPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyGrowthLotteryChancesPath},
	},
	codebuddy.CodeBuddyGrowthLotteryDrawPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyGrowthLotteryDrawPath},
	},
	codebuddy.CodeBuddyTravelStatusPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyTravelStatusPath},
	},
	codebuddy.CodeBuddyTravelDepartPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyTravelDepartPath},
	},
	codebuddy.CodeBuddyTravelClaimPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyTravelClaimPath},
	},
	codebuddy.CodeBuddyBuddyInfoPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyBuddyInfoPath},
	},
	codebuddy.CodeBuddyBuddyFirstPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyBuddyFirstPath},
	},
	codebuddy.CodeBuddyBuddyAgreementPath: {
		Domain: codeBuddyDomainChat,
		Paths:  []string{codebuddy.CodeBuddyBuddyAgreementPath},
	},

	// --- billing 域：三个都**不**参与 realm 路径回落 ---
	//
	// 别看到 `/billing/meter/` 前缀就想加候选：回落族是显式列表，只有
	// get-user-resource 与 daily-checkin 在名单里（那两条由既有实现按 realm 走
	// CodeBuddyUserResourcePaths / CodeBuddyDailyCheckinPaths，不走本文件）。
	codebuddy.CodeBuddyClaimGiftPath: {
		Domain: codeBuddyDomainBilling,
		Paths:  []string{codebuddy.CodeBuddyClaimGiftPath},
	},
	codebuddy.CodeBuddyClaimCompensationPath: {
		Domain: codeBuddyDomainBilling,
		Paths:  []string{codebuddy.CodeBuddyClaimCompensationPath},
	},
	codebuddy.CodeBuddyTrialPath: {
		Domain: codeBuddyDomainBilling,
		Paths:  []string{codebuddy.CodeBuddyTrialPath},
	},

	// --- 小程序门户域：CN 固定，不按 realm 分 ---
	codebuddy.CodeBuddySchoolTasksPath: {
		Domain: codeBuddyDomainSchoolMiniApp,
		Paths:  []string{codebuddy.CodeBuddySchoolTasksPath},
	},
	codebuddy.CodeBuddySchoolConfigPath: {
		Domain: codeBuddyDomainSchoolMiniApp,
		Paths:  []string{codebuddy.CodeBuddySchoolConfigPath},
	},
	codebuddy.CodeBuddySchoolWheelDrawPath: {
		Domain: codeBuddyDomainSchoolMiniApp,
		Paths:  []string{codebuddy.CodeBuddySchoolWheelDrawPath},
	},
	codebuddy.CodeBuddySchoolShareCompletePath: {
		Domain: codeBuddyDomainSchoolMiniApp,
		Paths:  []string{codebuddy.CodeBuddySchoolShareCompletePath},
	},
}

// codeBuddySchoolPrefixPaths school 域带 task_code 的模板路径（前缀段），
// 单独列出因为它们是**动态拼接**的，无法直接当 map 键。
//
// 处理方式：调用方用 CodeBuddySchoolTaskViewedPath/ClaimPath 拼出完整路径后传入，
// 本函数用**后缀白名单**（`/viewed`、`/claim` + 前缀匹配）识别。这是本文件里
// 唯一一处"按形状识别"，所以把它单独圈在这里并写清判据，不混进上面的 map。
var codeBuddySchoolDynamicSuffixes = []string{"/viewed", "/claim"}

// codeBuddyEndpointCandidates 解析端点 → 候选序列（按尝试顺序）。
//
// 返回 ok=false 表示该路径**不在白名单里**——调用方必须视为编程错误并停手，
// **不能**回落成"当作 chat 域单路径发出去"。未知端点意味着"有人加了新端点但没
// 声明它的域"，静默按默认域发送会把请求打到错的主机（而 realm 差异会让它在
// CN 上"恰好能通"，直到某天接 global 账号才炸）。
func codeBuddyEndpointCandidates(path string) ([]codeBuddyEndpointCandidate, bool) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, false
	}

	// 先查静态白名单。
	if endpoint, ok := codeBuddyGrowthEndpoints[trimmed]; ok {
		candidates := make([]codeBuddyEndpointCandidate, 0, len(endpoint.Paths))
		for _, p := range endpoint.Paths {
			candidates = append(candidates, codeBuddyEndpointCandidate{Domain: endpoint.Domain, Path: p})
		}
		return candidates, true
	}

	// 再查 school 动态路径（`{prefix}/tasks/{code}/{suffix}`）。
	if strings.HasPrefix(trimmed, codebuddy.CodeBuddySchoolPortalPrefix+"/tasks/") {
		for _, suffix := range codeBuddySchoolDynamicSuffixes {
			if strings.HasSuffix(trimmed, suffix) {
				// task_code 段不能为空（`/tasks//viewed` 这种是拼接 bug）。
				code := strings.TrimSuffix(strings.TrimPrefix(trimmed, codebuddy.CodeBuddySchoolPortalPrefix+"/tasks/"), suffix)
				if strings.TrimSpace(code) == "" {
					return nil, false
				}
				return []codeBuddyEndpointCandidate{{Domain: codeBuddyDomainSchoolMiniApp, Path: trimmed}}, true
			}
		}
	}
	return nil, false
}

// codeBuddyGrowthDomainBase 取某域在给定账号下的 base URL。
//
// realm 只对 chat / billing 两域有意义：school 门户域全球版无该活动，
// 恒用 CN 门户（见 codeBuddyDomainSchoolMiniApp 的注释）。
// codeBuddyGrowthTestBase 测试注入的 base 覆盖（生产恒为空）。
//
// 为什么需要它：A5/A6 之前几批的测试都靠 `testBaseURL` 把请求引到 httptest，
// 但 growth 层的 base 由本函数按**域**解析（三个域三套），单个 testBaseURL
// 无法表达"chat 域走某个 httptest"这种区分。**没有这个缝的直接后果**：
// 测试会打到真实上游（实测表现为 http=401）——既不可测，又会在 CI 里
// 真发外部请求。
//
// 只在测试里 `SetCodeBuddyGrowthTestBase` 设置；生产代码路径不碰它。
var codeBuddyGrowthTestBase string

// SetCodeBuddyGrowthTestBase 注入 growth 层的测试 base（仅测试调用）。
func SetCodeBuddyGrowthTestBase(base string) {
	codeBuddyGrowthTestBase = strings.TrimRight(strings.TrimSpace(base), "/")
}

func codeBuddyGrowthDomainBase(domain codeBuddyEndpointDomain, account *Account) string {
	if codeBuddyGrowthTestBase != "" {
		return codeBuddyGrowthTestBase
	}
	switch domain {
	case codeBuddyDomainChat:
		return CodeBuddyChatBase(account)
	case codeBuddyDomainBilling:
		return CodeBuddyBillingBase(account)
	case codeBuddyDomainSchoolMiniApp:
		return codebuddy.CodeBuddySchoolMiniAppBase
	default:
		return ""
	}
}
