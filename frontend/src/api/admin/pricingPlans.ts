/**
 * Admin Pricing Plans API endpoints
 * 定价套餐管理端 CRUD：套餐本体 + 外部「公开模型 -> 协议」条目（含销售定价）
 * 与内部路由层（group_id 层）一起以整体替换语义维护。
 * 列表响应不含 models/routes 明细（后端返回空数组），编辑前必须按 id 拉取全量详情。
 */

import { apiClient } from '../client'

/** 计费模式：token 按 token 计费（默认）/ per_request 按次 / image 图片 / video 视频生成 */
export type PricingPlanBillingMode = 'token' | 'per_request' | 'image' | 'video'

/** 模型协议：与账号凭据的 api_protocol 正交，决定套餐出站的转发协议。 */
export type PricingPlanProtocol = 'chat_completions' | 'messages' | 'responses'

export const PRICING_PLAN_PROTOCOLS: readonly PricingPlanProtocol[] = [
  'chat_completions',
  'messages',
  'responses'
]

/** 定价区间（token 区间 / 按次分层 / 图片分辨率分层）。仅透传保留，管理端暂不编辑。 */
export interface PlanPricingInterval {
  min_tokens: number
  max_tokens?: number | null
  tier_label: string
  input_price?: number | null
  output_price?: number | null
  cache_write_price?: number | null
  cache_read_price?: number | null
  input_multiplier?: number | null
  output_multiplier?: number | null
  cache_write_multiplier?: number | null
  cache_read_multiplier?: number | null
  per_request_price?: number | null
  sort_order?: number
}

/** 分时倍率配置（仅 token 模式支持）。仅透传保留，管理端暂不编辑。 */
export interface PlanTimePricing {
  timezone: string
  periods: { start_time: string; end_time: string; multiplier: number }[]
}

/** 套餐内单条「模型 -> 协议」条目的销售定价文档（JSONB）。 */
export interface PlanModelPricing {
  billing_mode: PricingPlanBillingMode | ''
  input_price?: number | null
  output_price?: number | null
  cache_write_price?: number | null
  cache_read_price?: number | null
  fast_multiplier?: number | null
  flex_multiplier?: number | null
  image_input_price?: number | null
  image_output_price?: number | null
  per_request_price?: number | null
  intervals?: PlanPricingInterval[]
  time_pricing?: PlanTimePricing | null
}

/** 外部「公开模型 -> 协议」条目的管理端响应（含销售定价配置）。 */
export interface AdminPricingPlanModel {
  id: number
  plan_id: number
  public_model: string
  protocol: string
  upstream_model: string
  direct: boolean
  allow_compatibility_fallback: boolean
  priority: number
  enabled: boolean
  notes: string
  pricing?: PlanModelPricing | null
  created_at: string
  updated_at: string
}

/** 内部路由层条目的管理端响应（含内部 group_id）。 */
export interface AdminPricingPlanRoute {
  id: number
  plan_id: number
  group_id: number
  priority: number
  enabled: boolean
  created_at: string
  updated_at: string
}

/** 套餐管理端响应（详情含 models/routes 全量明细；列表响应的明细为空数组）。 */
export interface AdminPricingPlan {
  id: number
  name: string
  title: string
  description: string
  status: 'active' | 'disabled'
  is_public: boolean
  sort_order: number
  created_at: string
  updated_at: string
  models: AdminPricingPlanModel[]
  routes: AdminPricingPlanRoute[]
}

/** 模型协议条目的创建/更新请求（整体替换语义，pricing 全量下发）。 */
export interface PricingPlanModelInput {
  public_model: string
  protocol?: string
  upstream_model?: string
  direct?: boolean
  allow_compatibility_fallback?: boolean
  priority?: number
  enabled?: boolean
  notes?: string
  pricing?: PlanModelPricing | null
}

/** 内部路由层条目的创建/更新请求。 */
export interface PricingPlanRouteInput {
  group_id: number
  priority?: number
  enabled?: boolean
}

/** 套餐创建/更新请求（整体替换语义：models/routes 全量下发，空数组即清空）。 */
export interface PricingPlanUpsertRequest {
  name: string
  title?: string
  description?: string
  status?: 'active' | 'disabled'
  is_public?: boolean
  sort_order?: number
  models?: PricingPlanModelInput[]
  routes?: PricingPlanRouteInput[]
}

/**
 * 列出定价套餐；includeDisabled=true 时包含停用套餐。
 * @returns 列表（models/routes 明细为空数组，编辑请用 getById 拉全量）
 */
export async function list(includeDisabled: boolean = false): Promise<AdminPricingPlan[]> {
  const { data } = await apiClient.get<AdminPricingPlan[]>('/admin/pricing-plans', {
    params: includeDisabled ? { include_disabled: true } : undefined
  })
  return data
}

/**
 * 返回套餐及其模型协议条目、路由层（管理端全量视图）。
 * @param id - 套餐 ID
 */
export async function getById(id: number): Promise<AdminPricingPlan> {
  const { data } = await apiClient.get<AdminPricingPlan>(`/admin/pricing-plans/${id}`)
  return data
}

/** 创建套餐（含模型协议条目与路由层）。 */
export async function create(request: PricingPlanUpsertRequest): Promise<AdminPricingPlan> {
  const { data } = await apiClient.post<AdminPricingPlan>('/admin/pricing-plans', request)
  return data
}

/** 整体替换套餐内容（含模型协议条目与路由层）。 */
export async function update(
  id: number,
  request: PricingPlanUpsertRequest
): Promise<AdminPricingPlan> {
  const { data } = await apiClient.put<AdminPricingPlan>(`/admin/pricing-plans/${id}`, request)
  return data
}

/** 删除套餐（软删除，含模型协议条目与路由层）。 */
export async function deletePlan(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/pricing-plans/${id}`)
  return data
}

export const pricingPlansAPI = {
  list,
  getById,
  create,
  update,
  delete: deletePlan
}

export default pricingPlansAPI