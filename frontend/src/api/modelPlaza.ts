/**
 * Model Plaza API（公开端点，可匿名访问）
 * 公开产品目录（PricingPlan catalog）：套餐(plan) → 模型(model) → 协议(protocol) 三级。
 * 只含公开价格展示字段，不含分组/专属倍率等内部定价字段。
 */

import { apiClient } from './client'
import type { BillingMode } from '@/constants/channel'
import type { UserPricingInterval, UserSupportedModelPricing } from './channels'

/** 目录定价的阶梯档位（与渠道计费口径一致，仅保留公开价字段）。 */
export interface CatalogPricingInterval {
  min_tokens: number
  max_tokens: number | null
  tier_label?: string
  input_price: number | null
  output_price: number | null
  cache_write_price: number | null
  cache_read_price: number | null
  per_request_price: number | null
}

/** 单个协议行的公开定价（USD；token 计费为每 token 单价，按次/按图为每单位单价）。 */
export interface CatalogPricing {
  input_price: number | null
  output_price: number | null
  cache_write_price: number | null
  cache_read_price: number | null
  image_input_price: number | null
  image_output_price: number | null
  per_request_price: number | null
  intervals: CatalogPricingInterval[]
}

/** 目录条目：一个协议的计价行。 */
export interface CatalogProtocol {
  /** 协议/接入方式标识，如 openai / anthropic / image。 */
  protocol: string
  /** 是否直连（true = 直连计价；false = 经站方中转）。 */
  direct: boolean
  billing_mode: BillingMode
  pricing: CatalogPricing | null
}

/** 目录模型条目：模型标识 + 展示名 + 各协议计价行。 */
export interface CatalogModel {
  /** 模型标识（对上游的模型名，如 gpt-5.6）。 */
  id: string
  /** 目录展示名。 */
  display_name: string
  protocols: CatalogProtocol[]
}

/** 套餐：稳定代号 + 名称 + 描述 + 模型清单。 */
export interface PricingPlan {
  /** 套餐代号（稳定标识，如 standard）。 */
  code: string
  name: string
  description: string
  models: CatalogModel[]
}

/** 产品目录响应。 */
export interface PricingCatalog {
  /** 管理员配置的全局价格说明（Markdown）。 */
  description: string
  plans: PricingPlan[]
}

/**
 * 旧版（分组视图）模型广场的公开类型，与上游保持一致导出。
 * 当前目录接口以套餐(plan)为顶层，这些类型不再由目录接口返回，
 * 仅为仍有引用旧形态的类型兼容保留。
 */

/** 官方参考价（USD per token，与计费目录同源；字段缺失 = 目录未覆盖）。 */
export interface PlazaOfficialPricing {
  input_price: number | null
  output_price: number | null
  /** 5m 缓存写入（= LiteLLM cache_creation）。 */
  cache_write_price: number | null
  /** 1h 缓存写入（LiteLLM cache_creation_above_1hr），多数模型缺失。 */
  cache_write_1h_price?: number | null
  cache_read_price: number | null
  /** 官方长上下文阶梯（多档模型才有），不受分组开关影响。 */
  intervals?: UserPricingInterval[]
}

/**
 * 多档时的计价基准：
 * - whole_request：整单按所在档单价计价（目录阶梯、渠道区间）；
 * - marginal：仅超出阈值的部分按该档单价计价（平台旧规则）。
 */
export type PlazaLongContextBasis = 'whole_request' | 'marginal'

/** 分时倍率时段：配置时区当天 [start_time, end_time) 内整单实付乘 multiplier。 */
export interface PlazaTimePricingPeriod {
  start_time: string
  end_time: string
  multiplier: number
}

/** 计费会生效的分时倍率（仅倍率 ≠ 1 的时段，已按开始时间升序）。 */
export interface PlazaTimePricing {
  /** IANA 时区名，如 Asia/Shanghai。 */
  timezone: string
  /** true 时时段仅周一至周五生效，周末整天按标准价计费。 */
  weekdays_only?: boolean
  periods: PlazaTimePricingPeriod[]
}

export interface PlazaModel {
  name: string
  platform: string
  /** 实收口径的展示定价：多档时 intervals 为各档绝对单价（已由计费服务折算）；均为标准时段价。 */
  pricing: UserSupportedModelPricing | null
  official_pricing: PlazaOfficialPricing | null
  /** 仅多档模型返回。 */
  long_context_basis?: PlazaLongContextBasis
  /** 仅配置了分时倍率的模型返回。 */
  time_pricing?: PlazaTimePricing
}

export interface ModelPlazaGroup {
  id: number
  name: string
  description: string
  platform: string
  /** 'standard' | 'subscription' */
  subscription_type: string
  rate_multiplier: number
  /** 登录且管理员为该用户配了专属倍率时返回；生效倍率 = user_rate ?? rate_multiplier。 */
  user_rate_multiplier?: number
  peak_rate_enabled: boolean
  peak_start: string
  peak_end: string
  peak_rate_multiplier: number
  is_exclusive: boolean
  /** 生图独立倍率：true 时图片计费模型的实付倍率取 image_rate_multiplier，不取分组/专属倍率。 */
  image_rate_independent: boolean
  image_rate_multiplier: number
  /** 分组是否启用长上下文阶梯计费；false 时实付列只展示最低档，官方阶梯仅供参考。 */
  long_context_pricing_enabled: boolean
  models: PlazaModel[]
}

export interface ModelPlazaResponse {
  /** 管理员配置的全局价格说明（Markdown）。 */
  description: string
  groups: ModelPlazaGroup[]
}

/** 获取产品目录。开关未启用时后端返回 404。 */
export async function getModelPlaza(options?: { signal?: AbortSignal }): Promise<PricingCatalog> {
  const { data } = await apiClient.get<PricingCatalog>('/model-plaza', {
    signal: options?.signal
  })
  return data
}

export const modelPlazaAPI = { getModelPlaza }

export default modelPlazaAPI