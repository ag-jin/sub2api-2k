/**
 * 定价套餐表单的数据模型与请求装配（纯函数，无 i18n / 组件依赖）。
 *
 * PricingPlanDialog.vue 持有 form 状态与副作用（拉取详情、保存、错误提示），
 * 本模块只负责行结构的创建 / 回填 / 校验与 upsert 请求体组装，
 * 逻辑可不挂载组件单独推演。
 */

import type {
  AdminPricingPlanModel,
  AdminPricingPlanRoute,
  PlanModelPricing,
  PlanPricingInterval,
  PlanTimePricing,
  PricingPlanBillingMode,
  PricingPlanModelInput,
  PricingPlanRouteInput,
  PricingPlanUpsertRequest
} from '@/api/admin/pricingPlans'

/** 行内可编辑的价格字段（token 单价 / 缓存价 / 倍率 / 按次价）。 */
export type PriceKey =
  | 'input_price'
  | 'output_price'
  | 'cache_write_price'
  | 'cache_read_price'
  | 'fast_multiplier'
  | 'flex_multiplier'
  | 'image_input_price'
  | 'image_output_price'
  | 'per_request_price'

export interface ModelFormRow {
  public_model: string
  protocol: string
  upstream_model: string
  direct: boolean
  allow_compatibility_fallback: boolean
  priority: number
  enabled: boolean
  notes: string
  billing_mode: PricingPlanBillingMode | ''
  input_price: number | null
  output_price: number | null
  cache_write_price: number | null
  cache_read_price: number | null
  fast_multiplier: number | null
  flex_multiplier: number | null
  image_input_price: number | null
  image_output_price: number | null
  per_request_price: number | null
  // 后端明细（区间/分时）仅保留不编辑，保存时原样带回
  intervals: PlanPricingInterval[]
  time_pricing: PlanTimePricing | null
}

export interface RouteFormRow {
  group_id: number | null
  priority: number
  enabled: boolean
}

/** 表单行的来源字段（详情行，或新建时的默认空源）。 */
type ModelSource = Pick<
  AdminPricingPlanModel,
  | 'public_model'
  | 'protocol'
  | 'upstream_model'
  | 'direct'
  | 'allow_compatibility_fallback'
  | 'priority'
  | 'enabled'
  | 'notes'
  | 'pricing'
>

// 新建条目的默认空源：显式默认值 direct/enabled 默认 true、priority 默认 100、token 计费
const EMPTY_MODEL_SOURCE: ModelSource = {
  public_model: '',
  protocol: '',
  upstream_model: '',
  direct: true,
  allow_compatibility_fallback: false,
  priority: 100,
  enabled: true,
  notes: '',
  pricing: null
}

export function emptyModelRow(): ModelFormRow {
  return modelToForm(EMPTY_MODEL_SOURCE)
}

export function emptyRouteRow(): RouteFormRow {
  return {
    group_id: null,
    priority: 0,
    enabled: true
  }
}

export function createPlanForm() {
  return {
    name: '',
    title: '',
    description: '',
    status: 'active' as 'active' | 'disabled',
    is_public: false,
    sort_order: 0,
    models: [] as ModelFormRow[],
    routes: [] as RouteFormRow[]
  }
}

export type PlanForm = ReturnType<typeof createPlanForm>

// 价格输入：空串转 null，保持表单状态为 number | null（避免 v-model.number 的空串污染）
export function onPriceInput(row: ModelFormRow, key: PriceKey, event: Event) {
  const raw = (event.target as HTMLInputElement).value
  row[key] = raw === '' ? null : Number(raw)
}

/** 切换计费模式：分时仅 token 模式支持，切走时丢弃以免后端校验失败。 */
export function selectBillingMode(row: ModelFormRow, mode: PricingPlanBillingMode | '') {
  row.billing_mode = mode
  if (mode !== 'token') {
    row.time_pricing = null
  }
}

/** 直连或非 chat_completions 协议时，兼容回退必须关闭。 */
export function normalizeCompatibilityFallback(row: ModelFormRow) {
  if (row.direct || row.protocol !== 'chat_completions') {
    row.allow_compatibility_fallback = false
  }
}

/** 详情行 → 表单行（编辑模式按 id 拉全量详情后回填）。 */
export function modelToForm(m: ModelSource): ModelFormRow {
  const p = m.pricing
  return {
    public_model: m.public_model,
    protocol: m.protocol || 'chat_completions',
    upstream_model: m.upstream_model,
    direct: m.direct,
    allow_compatibility_fallback: m.allow_compatibility_fallback,
    priority: m.priority,
    enabled: m.enabled,
    notes: m.notes,
    billing_mode: p?.billing_mode || 'token',
    input_price: p?.input_price ?? null,
    output_price: p?.output_price ?? null,
    cache_write_price: p?.cache_write_price ?? null,
    cache_read_price: p?.cache_read_price ?? null,
    fast_multiplier: p?.fast_multiplier ?? null,
    flex_multiplier: p?.flex_multiplier ?? null,
    image_input_price: p?.image_input_price ?? null,
    image_output_price: p?.image_output_price ?? null,
    per_request_price: p?.per_request_price ?? null,
    intervals: p?.intervals ?? [],
    time_pricing: p?.time_pricing ?? null
  }
}

export function routeToForm(r: AdminPricingPlanRoute): RouteFormRow {
  return {
    group_id: r.group_id,
    priority: r.priority,
    enabled: r.enabled
  }
}

// 组装 pricing 文档：只下发当前计费模式的字段 + 保留的区间/分时明细。
// 分时仅 token 模式支持，切换到其他模式时丢弃以免后端校验失败。
export function buildPricingPayload(row: ModelFormRow): PlanModelPricing {
  const pricing: PlanModelPricing = { billing_mode: row.billing_mode }
  switch (row.billing_mode) {
    case 'per_request':
    case 'image':
    case 'video':
      pricing.per_request_price = row.per_request_price
      break
    case 'token':
    case '':
    default:
      pricing.input_price = row.input_price
      pricing.output_price = row.output_price
      pricing.cache_write_price = row.cache_write_price
      pricing.cache_read_price = row.cache_read_price
      pricing.fast_multiplier = row.fast_multiplier
      pricing.flex_multiplier = row.flex_multiplier
      if (row.time_pricing) pricing.time_pricing = row.time_pricing
      break
  }
  if (row.intervals.length > 0) {
    pricing.intervals = row.intervals
  }
  return pricing
}

// 数值输入兜底：v-model.number 空串时保持字符串，发送前统一归一化
function normalizeInt(v: unknown, fallback: number): number {
  if (typeof v === 'number' && Number.isFinite(v)) return v
  if (typeof v === 'string' && v.trim() !== '') {
    const n = Number(v)
    if (Number.isFinite(n)) return n
  }
  return fallback
}

/** upsert 请求体（整体替换语义：models/routes 全量下发）。 */
export function buildPayload(form: PlanForm): PricingPlanUpsertRequest {
  const models: PricingPlanModelInput[] = form.models.map((m) => ({
    public_model: m.public_model,
    protocol: m.protocol,
    upstream_model: m.upstream_model,
    direct: m.direct,
    allow_compatibility_fallback: m.allow_compatibility_fallback,
    priority: normalizeInt(m.priority, 100),
    enabled: m.enabled,
    notes: m.notes,
    pricing: buildPricingPayload(m)
  }))
  const routes: PricingPlanRouteInput[] = form.routes.map((r) => ({
    group_id: r.group_id as number,
    priority: normalizeInt(r.priority, 0),
    enabled: r.enabled
  }))
  return {
    name: form.name,
    title: form.title,
    description: form.description,
    status: form.status,
    is_public: form.is_public,
    sort_order: normalizeInt(form.sort_order, 0),
    models,
    routes
  }
}

// 客户端重复校验：同一套餐内 group 与 priority 均不可重复；冲突行（含首行）全部标红，方便一眼定位。
// 收集冲突行下标：key 相同的行 ≥2 时这些行全部计入；included=false 的行不参与。
function conflictIndexes<V>(
  routes: RouteFormRow[],
  keyOf: (route: RouteFormRow) => V,
  included: (route: RouteFormRow) => boolean = () => true
): Set<number> {
  const byKey = new Map<V, number[]>()
  routes.forEach((route, i) => {
    if (!included(route)) return
    const value = keyOf(route)
    const arr = byKey.get(value) ?? []
    arr.push(i)
    byKey.set(value, arr)
  })
  const conflicts = new Set<number>()
  for (const arr of byKey.values()) {
    if (arr.length > 1) arr.forEach((i) => conflicts.add(i))
  }
  return conflicts
}

/** 未选分组（group_id 为空）的行不参与重复校验。 */
export function groupConflictIndexes(routes: RouteFormRow[]): Set<number> {
  return conflictIndexes(routes, (r) => r.group_id, (r) => r.group_id != null)
}

export function priorityConflictIndexes(routes: RouteFormRow[]): Set<number> {
  return conflictIndexes(routes, (r) => r.priority)
}
