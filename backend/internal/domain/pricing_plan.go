package domain

// PricingPlan model protocol constants 描述定价套餐内每个公开模型的
// 上游 API 协议。与 account credentials 里的 api_protocol 正交：这里是
// 套餐层面的「模型 -> 协议」条目，供网关在按套餐出站时选择转发协议。
const (
	// PricingPlanProtocolChatCompletions 走 OpenAI Chat Completions 协议（默认）。
	PricingPlanProtocolChatCompletions = "chat_completions"
	// PricingPlanProtocolMessages 走原生 Anthropic /v1/messages 协议。
	PricingPlanProtocolMessages = "messages"
	// PricingPlanProtocolResponses 走 OpenAI Responses 协议。
	PricingPlanProtocolResponses = "responses"
)

// PlanModelPricing 是套餐内单条「模型 -> 协议」条目的定价文档
// （pricing_plan_models.pricing，JSONB）。字段形状与
// service.ChannelModelPricing 的定价部分一致（billing_mode 与全部价格
// 字段/区间），以便管理端/网关按既有定价语义读写；不含渠道/实体标识字段。
//
// 类型放在 domain 包是因为 ent schema（internal/domain 的下游）需要引用它做
// field.JSON 序列化；service 不能被 ent import（会造成循环依赖）。
type PlanModelPricing struct {
	BillingMode      string                     `json:"billing_mode"`
	InputPrice       *float64                   `json:"input_price"`
	OutputPrice      *float64                   `json:"output_price"`
	CacheWritePrice  *float64                   `json:"cache_write_price"`
	CacheReadPrice   *float64                   `json:"cache_read_price"`
	FastMultiplier   *float64                   `json:"fast_multiplier"`
	FlexMultiplier   *float64                   `json:"flex_multiplier"`
	ImageInputPrice  *float64                   `json:"image_input_price"`
	ImageOutputPrice *float64                   `json:"image_output_price"`
	PerRequestPrice  *float64                   `json:"per_request_price"`
	Intervals        []PlanModelPricingInterval `json:"intervals"`
	TimePricing      *PlanModelTimePricing      `json:"time_pricing,omitempty"`
}

// PlanModelPricingInterval 定价区间（token 区间 / 按次分层 / 图片分辨率分层），
// 与 service.PricingInterval 的定价字段一致。
type PlanModelPricingInterval struct {
	MinTokens            int      `json:"min_tokens"`
	MaxTokens            *int     `json:"max_tokens"`
	TierLabel            string   `json:"tier_label"`
	InputPrice           *float64 `json:"input_price"`
	OutputPrice          *float64 `json:"output_price"`
	CacheWritePrice      *float64 `json:"cache_write_price"`
	CacheReadPrice       *float64 `json:"cache_read_price"`
	InputMultiplier      *float64 `json:"input_multiplier"`
	OutputMultiplier     *float64 `json:"output_multiplier"`
	CacheWriteMultiplier *float64 `json:"cache_write_multiplier"`
	CacheReadMultiplier  *float64 `json:"cache_read_multiplier"`
	PerRequestPrice      *float64 `json:"per_request_price"`
	SortOrder            int      `json:"sort_order"`
}

// PlanModelTimePricing 定价的分时倍率配置，与 service.ChannelTimePricing 一致。
type PlanModelTimePricing struct {
	Timezone string                       `json:"timezone"`
	Periods  []PlanModelTimePricingPeriod `json:"periods"`
}

// PlanModelTimePricingPeriod 是秒级的左闭右开分时倍率区间，并兼容历史 HH:mm 数据。
type PlanModelTimePricingPeriod struct {
	StartTime  string  `json:"start_time"`
	EndTime    string  `json:"end_time"`
	Multiplier float64 `json:"multiplier"`
}
