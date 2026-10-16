package domain

// 平台清单：平台的单一数据来源。"所有平台"类列表（分组平台、用户额度、组合
// 路由目标、调度快照、定价匹配等）与"某网关族"类列表都从这里派生；新增平台
// 登记一条即可在这些位置生效，无需逐处修改或新增数据库约束迁移。
//
// 依赖具体实现的能力（OAuth、渠道监控、余额 / Coding Plan 探测、用量窗口、
// Codex 模型目录等）仍由各自的代码显式列出，不在此处声明。

// PlatformGateway 标识平台请求由哪个网关服务处理。
type PlatformGateway string

const (
	// PlatformGatewayAnthropic 由 GatewayService 处理（Claude Messages 协议，以及 TypeSafe 的 /v1/systemone）。
	PlatformGatewayAnthropic PlatformGateway = "anthropic"
	// PlatformGatewayOpenAI 由 OpenAIGatewayService 处理（OpenAI、Grok 与多协议 API Key 供应商）。
	PlatformGatewayOpenAI PlatformGateway = "openai"
	// PlatformGatewayGemini 由 Gemini 兼容网关处理。
	PlatformGatewayGemini PlatformGateway = "gemini"
	// PlatformGatewayAntigravity 由 Antigravity 网关处理。
	PlatformGatewayAntigravity PlatformGateway = "antigravity"
)

// PlatformSpec 描述一个具体（非 composite）平台。
type PlatformSpec struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Gateway     PlatformGateway `json:"gateway"`
	// CNProvider 标识国产单一厂商（Kimi / 智谱 / DeepSeek / MiniMax），共享国产
	// 供应商的余额 / Coding Plan 等能力。
	CNProvider bool `json:"cn_provider"`
	// LiteLLMProvider 是定价目录中的 provider 名；空表示不支持从定价目录同步模型。
	LiteLLMProvider string `json:"litellm_provider,omitempty"`
	// nonCompositeTarget 标记只服务独立分组的本地存量平台（CodeBuddy 与 card 血统的
	// opencode）：可作账号 / 分组平台，但不参与组合分组的请求目标解析、定价回退与
	// 模型清单（合并前组合链路各处显式排除二者）。零值 false = 可作组合目标，新登记
	// 平台沿用上游语义。未导出：对外清单不新增字段，跨包判断用 IsCompositeTargetPlatform。
	nonCompositeTarget bool
}

// platformList 的顺序即平台的展示顺序。
var platformList = []PlatformSpec{
	{ID: PlatformAnthropic, DisplayName: "Anthropic", Gateway: PlatformGatewayAnthropic, LiteLLMProvider: "anthropic"},
	{ID: PlatformOpenAI, DisplayName: "OpenAI", Gateway: PlatformGatewayOpenAI, LiteLLMProvider: "openai"},
	{ID: PlatformGemini, DisplayName: "Gemini", Gateway: PlatformGatewayGemini, LiteLLMProvider: "gemini"},
	{ID: PlatformAntigravity, DisplayName: "Antigravity", Gateway: PlatformGatewayAntigravity, LiteLLMProvider: "anthropic"},
	{ID: PlatformGrok, DisplayName: "Grok", Gateway: PlatformGatewayOpenAI, LiteLLMProvider: "xai"},
	{ID: PlatformKimi, DisplayName: "Kimi", Gateway: PlatformGatewayOpenAI, CNProvider: true, LiteLLMProvider: "moonshot"},
	{ID: PlatformZhipu, DisplayName: "Zhipu GLM", Gateway: PlatformGatewayOpenAI, CNProvider: true, LiteLLMProvider: "zhipu"},
	{ID: PlatformDeepseek, DisplayName: "DeepSeek", Gateway: PlatformGatewayOpenAI, CNProvider: true, LiteLLMProvider: "deepseek"},
	// 本地存量平台：CodeBuddy（腾讯 Copilot，OpenAI Chat 协议）与 card 血统的
	// opencode 存量值（PlatformOpenCode，与 opencode_go 同一上游的旧取值）。
	// 二者经 OpenAI 网关转发（NormalizeOpenAICompatiblePlatform 保留原值、
	// IsOpenAICompatible 为真），但只服务独立分组，故 nonCompositeTarget。
	// 定价目录没有这两个 provider（LiteLLMProvider 留空）。
	{ID: PlatformOpenCode, DisplayName: "OpenCode", Gateway: PlatformGatewayOpenAI, nonCompositeTarget: true},
	{ID: PlatformCodeBuddy, DisplayName: "CodeBuddy", Gateway: PlatformGatewayOpenAI, nonCompositeTarget: true},
	{ID: PlatformMiniMax, DisplayName: "MiniMax", Gateway: PlatformGatewayOpenAI, CNProvider: true, LiteLLMProvider: "minimax"},
	{ID: PlatformOpenCodeGo, DisplayName: "OpenCode", Gateway: PlatformGatewayOpenAI, LiteLLMProvider: "opencode-go"},
	{ID: PlatformTypeSafe, DisplayName: "TypeSafe / Jev", Gateway: PlatformGatewayAnthropic, LiteLLMProvider: "typesafe"},
	{ID: PlatformCommandCode, DisplayName: "Command Code", Gateway: PlatformGatewayOpenAI},
	{ID: PlatformCline, DisplayName: "Cline", Gateway: PlatformGatewayOpenAI},
}

var platformIndex = func() map[string]int {
	index := make(map[string]int, len(platformList))
	for i, spec := range platformList {
		index[spec.ID] = i
	}
	return index
}()

// Platforms 返回全部具体平台（副本），按展示顺序。
func Platforms() []PlatformSpec {
	out := make([]PlatformSpec, len(platformList))
	copy(out, platformList)
	return out
}

// LookupPlatform 返回具体平台的登记信息；composite 与未知值返回 false。
func LookupPlatform(id string) (PlatformSpec, bool) {
	i, ok := platformIndex[id]
	if !ok {
		return PlatformSpec{}, false
	}
	return platformList[i], true
}

// IsConcretePlatform 报告 id 是否为已登记的具体平台（不含 composite）。
func IsConcretePlatform(id string) bool {
	_, ok := platformIndex[id]
	return ok
}

// IsGroupPlatform 报告 id 是否可作为分组平台（具体平台或 composite）。
func IsGroupPlatform(id string) bool {
	return id == PlatformComposite || IsConcretePlatform(id)
}

// ConcretePlatformIDs 返回全部具体平台标识，按展示顺序。
func ConcretePlatformIDs() []string {
	return PlatformIDsWhere(func(PlatformSpec) bool { return true })
}

// PlatformIDsWhere 返回满足条件的具体平台标识，按展示顺序。
func PlatformIDsWhere(match func(PlatformSpec) bool) []string {
	out := make([]string, 0, len(platformList))
	for _, spec := range platformList {
		if match(spec) {
			out = append(out, spec.ID)
		}
	}
	return out
}

// compositePrecedenceHead 是组合分组查找顺序的固定前缀（历史顺序：Gemini 先于 OpenAI）。
var compositePrecedenceHead = []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI}

// CompositePrecedencePlatformIDs 返回按平台依次回退时的顺序：组合分组在请求目标
// 解析前逐平台查找定价 / 模型映射（先命中者生效）、拼接默认模型列表，以及调度快照
// 遍历。沿用历史顺序 anthropic、gemini、openai，其余按展示顺序，新登记的平台追加在
// 末尾；只服务独立分组的本地存量平台（见 PlatformSpec.nonCompositeTarget）不参与。
func CompositePrecedencePlatformIDs() []string {
	out := make([]string, 0, len(platformList))
	seen := make(map[string]struct{}, len(platformList))
	for _, id := range compositePrecedenceHead {
		if IsCompositeTargetPlatform(id) {
			out = append(out, id)
			seen[id] = struct{}{}
		}
	}
	for _, spec := range platformList {
		if _, ok := seen[spec.ID]; ok {
			continue
		}
		if !IsCompositeTargetPlatform(spec.ID) {
			continue
		}
		out = append(out, spec.ID)
	}
	return out
}

// IsCompositeTargetPlatform 报告 id 是否可作为组合分组的请求目标（组合路由、
// 定价 / 模型映射回退与模型清单）。本地存量平台 CodeBuddy 与 card 血统的 opencode
// 只服务独立分组，返回 false；composite 本身与未登记值同样返回 false。
// 账号 / 分组合法性仍用 IsConcretePlatform。
func IsCompositeTargetPlatform(id string) bool {
	spec, ok := LookupPlatform(id)
	return ok && !spec.nonCompositeTarget
}

// UsesOpenAIGateway 报告平台是否由 OpenAI 网关处理。
func UsesOpenAIGateway(id string) bool {
	spec, ok := LookupPlatform(id)
	return ok && spec.Gateway == PlatformGatewayOpenAI
}

// IsCNProviderPlatform 报告平台是否为国产单一厂商。
func IsCNProviderPlatform(id string) bool {
	spec, ok := LookupPlatform(id)
	return ok && spec.CNProvider
}
