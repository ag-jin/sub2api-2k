/**
 * Model Plaza API（公开端点，可匿名访问）
 * 公开产品目录（PricingPlan catalog）：套餐(plan) → 模型(model) → 协议(protocol) 三级。
 * 只含公开价格展示字段，不含分组/专属倍率等内部定价字段。
 */

import { apiClient } from './client'
import type { BillingMode } from '@/constants/channel'

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

/** 获取产品目录。开关未启用时后端返回 404。 */
export async function getModelPlaza(options?: { signal?: AbortSignal }): Promise<PricingCatalog> {
  const { data } = await apiClient.get<PricingCatalog>('/model-plaza', {
    signal: options?.signal
  })
  return data
}

export const modelPlazaAPI = { getModelPlaza }

export default modelPlazaAPI