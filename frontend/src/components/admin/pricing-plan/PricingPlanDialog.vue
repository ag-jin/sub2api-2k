<template>
  <BaseDialog
    :show="show"
    :title="title"
    width="extra-wide"
    @close="handleClose"
  >
    <div v-if="detailLoading" class="flex items-center justify-center gap-2 py-12 text-sm text-gray-400">
      <LoadingSpinner size="sm" />
      {{ t('admin.pricingPlans.form.loadingDetail') }}
    </div>

    <form v-else id="pricing-plan-form" data-testid="pricing-plan-form" class="space-y-6" @submit.prevent="handleSubmit">
      <!-- 套餐本体 -->
      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label class="input-label">{{ t('admin.pricingPlans.form.name') }} <span class="text-red-500">*</span></label>
          <input v-model="form.name" data-testid="plan-name" type="text" required class="input" :placeholder="t('admin.pricingPlans.form.namePlaceholder')" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.pricingPlans.form.title') }}</label>
          <input v-model="form.title" type="text" class="input" :placeholder="t('admin.pricingPlans.form.titlePlaceholder')" />
        </div>
      </div>

      <div>
        <label class="input-label">{{ t('admin.pricingPlans.form.description') }}</label>
        <textarea v-model="form.description" rows="2" class="input resize-y" :placeholder="t('admin.pricingPlans.form.descriptionPlaceholder')"></textarea>
      </div>

      <div class="grid gap-4 sm:grid-cols-3">
        <div>
          <label class="input-label">{{ t('admin.pricingPlans.form.status') }}</label>
          <Select
            v-model="form.status"
            :options="statusOptions"
            :aria-label="t('admin.pricingPlans.form.status')"
          />
        </div>
        <div>
          <label class="input-label">{{ t('admin.pricingPlans.form.sortOrder') }}</label>
          <input v-model.number="form.sort_order" type="number" step="1" class="input" />
        </div>
        <div class="flex items-end pb-1">
          <label class="flex cursor-pointer items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
            <Toggle v-model="form.is_public" />
            {{ t('admin.pricingPlans.form.isPublic') }}
          </label>
        </div>
      </div>

      <!-- 外部模型协议条目（含销售定价） -->
      <section class="rounded-xl border border-gray-200 p-4 dark:border-dark-700">
        <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div>
            <h4 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t('admin.pricingPlans.form.modelsTitle') }}
              <span class="ml-1 text-xs font-normal text-gray-400">{{ t('admin.pricingPlans.form.modelsCount', { count: form.models.length }) }}</span>
            </h4>
            <p class="mt-0.5 text-xs text-gray-400">{{ t('admin.pricingPlans.form.modelsHint') }}</p>
          </div>
          <button type="button" data-testid="add-model" class="btn btn-secondary btn-sm" @click="addModel">
            <Icon name="plus" size="sm" />
            {{ t('admin.pricingPlans.form.addModel') }}
          </button>
        </div>

        <div v-if="form.models.length === 0" class="rounded-lg border border-dashed border-gray-200 py-6 text-center text-xs text-gray-400 dark:border-dark-700">
          {{ t('admin.pricingPlans.form.noModels') }}
        </div>

        <div
          v-for="(model, index) in form.models"
          :key="index"
          class="mb-3 rounded-lg border border-gray-200 p-4 last:mb-0 dark:border-dark-700"
          :data-testid="`model-entry-${index}`"
        >
          <div class="mb-3 flex items-center justify-between gap-2">
            <h5 class="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
              {{ t('admin.pricingPlans.form.modelEntry', { index: index + 1 }) }}
            </h5>
            <button type="button" data-testid="remove-model" class="btn btn-ghost btn-sm text-red-500 hover:bg-red-50 dark:hover:bg-red-900/20" @click="removeModel(index)">
              <Icon name="trash" size="sm" />
              {{ t('common.remove') }}
            </button>
          </div>

          <div class="grid gap-3 sm:grid-cols-3">
            <div>
              <label class="input-label">{{ t('admin.pricingPlans.form.publicModel') }} <span class="text-red-500">*</span></label>
              <input v-model="model.public_model" type="text" required class="input" :placeholder="t('admin.pricingPlans.form.publicModelPlaceholder')" />
            </div>
            <div>
              <label class="input-label">{{ t('admin.pricingPlans.form.protocol') }}</label>
              <Select
                v-model="model.protocol"
                :options="protocolOptions"
                :aria-label="t('admin.pricingPlans.form.protocol')"
                @update:model-value="normalizeCompatibilityFallback(model)"
              />
            </div>
            <div>
              <label class="input-label">{{ t('admin.pricingPlans.form.upstreamModel') }}</label>
              <input v-model="model.upstream_model" type="text" class="input" :placeholder="t('admin.pricingPlans.form.upstreamModelPlaceholder')" />
            </div>
          </div>

          <div class="mt-3 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <label class="flex cursor-pointer items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <Toggle v-model="model.direct" @update:model-value="normalizeCompatibilityFallback(model)" />
              {{ t('admin.pricingPlans.form.direct') }}
            </label>
            <label class="flex cursor-pointer items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <Toggle
                v-model="model.allow_compatibility_fallback"
                :disabled="model.direct || model.protocol !== 'chat_completions'"
              />
              {{ t('admin.pricingPlans.form.compatFallback') }}
            </label>
            <label class="flex cursor-pointer items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <Toggle v-model="model.enabled" />
              {{ t('admin.pricingPlans.form.enabled') }}
            </label>
            <div>
              <label class="input-label">{{ t('admin.pricingPlans.form.priority') }}</label>
              <input v-model.number="model.priority" type="number" step="1" class="input" />
            </div>
          </div>

          <div class="mt-3">
            <label class="input-label">{{ t('admin.pricingPlans.form.notes') }}</label>
            <input v-model="model.notes" type="text" class="input" :placeholder="t('admin.pricingPlans.form.notesPlaceholder')" />
          </div>

          <!-- 计费模式与价格字段 -->
          <div class="mt-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-800/60">
            <label class="input-label">{{ t('admin.pricingPlans.form.billingMode') }}</label>
            <div class="mt-1 grid gap-2 sm:grid-cols-4" data-testid="model-billing-mode">
              <button
                v-for="mode in billingModes"
                :key="mode.value"
                type="button"
                :data-testid="`billing-mode-${mode.value}`"
                :aria-pressed="model.billing_mode === mode.value"
                class="rounded-lg border px-2 py-1.5 text-left transition-colors"
                :class="model.billing_mode === mode.value ? 'border-primary-500 bg-primary-50 dark:bg-primary-900/30' : 'border-gray-200 dark:border-dark-700'"
                @click="selectBillingMode(model, mode.value)"
              >
                <span class="block text-xs font-semibold text-gray-800 dark:text-gray-200">{{ mode.label }}</span>
              </button>
            </div>

            <div class="mt-3 grid gap-3 sm:grid-cols-3">
              <template v-for="field in priceFieldsFor(model.billing_mode)" :key="field.key">
                <div>
                  <label class="input-label">{{ field.label }}</label>
                  <input
                    :data-testid="`price-${field.key}`"
                    :value="model[field.key] ?? ''"
                    type="number"
                    step="any"
                    min="0"
                    class="input"
                    :placeholder="field.placeholder"
                    @input="onPriceInput(model, field.key, $event)"
                  />
                </div>
              </template>
            </div>

            <p v-if="model.intervals.length > 0 || model.time_pricing" class="mt-2 text-xs text-gray-400">
              {{ t('admin.pricingPlans.form.pricingDetailPreserved') }}
            </p>
          </div>
        </div>
      </section>

      <!-- 内部路由层（仅管理端可见） -->
      <section class="rounded-xl border border-amber-200 bg-amber-50/40 p-4 dark:border-amber-500/20 dark:bg-amber-500/5">
        <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div>
            <h4 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t('admin.pricingPlans.form.routesTitle') }}
              <span class="ml-1 text-xs font-normal text-gray-400">{{ t('admin.pricingPlans.form.routesCount', { count: form.routes.length }) }}</span>
            </h4>
            <p class="mt-0.5 text-xs text-gray-400">{{ t('admin.pricingPlans.form.routesHint') }}</p>
          </div>
          <button type="button" data-testid="add-route" class="btn btn-secondary btn-sm" @click="addRoute">
            <Icon name="plus" size="sm" />
            {{ t('admin.pricingPlans.form.addRoute') }}
          </button>
        </div>

        <div v-if="form.routes.length === 0" class="rounded-lg border border-dashed border-amber-200 py-6 text-center text-xs text-gray-400 dark:border-amber-500/20">
          {{ t('admin.pricingPlans.form.noRoutes') }}
        </div>

        <div
          v-for="(route, index) in form.routes"
          :key="index"
          class="mb-3 rounded-lg border border-amber-200 bg-white p-4 last:mb-0 dark:border-amber-500/20 dark:bg-dark-800"
          :data-testid="`route-entry-${index}`"
        >
          <div class="grid gap-3 sm:grid-cols-[1fr_8rem_auto_auto] sm:items-end">
            <div>
              <label class="input-label">{{ t('admin.pricingPlans.form.group') }} <span class="text-red-500">*</span></label>
              <Select
                v-model="route.group_id"
                :options="groupOptions"
                :searchable="true"
                :error="!!routeGroupError(index)"
                :placeholder="t('admin.pricingPlans.form.groupPlaceholder')"
                :aria-label="t('admin.pricingPlans.form.group')"
              />
              <p v-if="routeGroupError(index)" class="mt-1 text-xs text-red-500" :data-testid="`route-group-error-${index}`">
                {{ routeGroupError(index) }}
              </p>
            </div>
            <div>
              <label class="input-label">{{ t('admin.pricingPlans.form.routePriority') }}</label>
              <input v-model.number="route.priority" type="number" step="1" class="input" />
              <p v-if="routePriorityError(index)" class="mt-1 text-xs text-red-500" :data-testid="`route-priority-error-${index}`">
                {{ routePriorityError(index) }}
              </p>
            </div>
            <label class="flex cursor-pointer items-center gap-2 pb-1 text-sm text-gray-700 dark:text-gray-300">
              <Toggle v-model="route.enabled" />
              {{ t('admin.pricingPlans.form.enabled') }}
            </label>
            <button type="button" data-testid="remove-route" class="btn btn-ghost btn-sm text-red-500 hover:bg-red-50 dark:hover:bg-red-900/20" @click="removeRoute(index)">
              <Icon name="trash" size="sm" />
              {{ t('common.remove') }}
            </button>
          </div>
        </div>
      </section>
    </form>

    <template #footer>
      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="handleClose">
          {{ t('common.cancel') }}
        </button>
        <button type="submit" form="pricing-plan-form" class="btn btn-primary" :disabled="saving || detailLoading" data-testid="save-plan">
          <LoadingSpinner v-if="saving" size="sm" class="mr-1" />
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { adminAPI } from '@/api/admin'
import {
  PRICING_PLAN_PROTOCOLS,
  type AdminPricingPlanModel,
  type AdminPricingPlanRoute,
  type PlanModelPricing,
  type PlanPricingInterval,
  type PlanTimePricing,
  type PricingPlanBillingMode,
  type PricingPlanModelInput,
  type PricingPlanRouteInput,
  type PricingPlanUpsertRequest
} from '@/api/admin/pricingPlans'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Icon from '@/components/icons/Icon.vue'

interface Props {
  show: boolean
  /** null = 新建；非 null = 按 id 拉取全量详情后整体替换更新 */
  planId: number | null
}

const props = defineProps<Props>()
const emit = defineEmits<{
  (e: 'close'): void
  (e: 'saved'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

interface ModelFormRow {
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

interface RouteFormRow {
  group_id: number | null
  priority: number
  enabled: boolean
}

const detailLoading = ref(false)
const saving = ref(false)
const groups = ref<Awaited<ReturnType<typeof adminAPI.groups.getAll>>>([])

// 显式默认值：direct/enabled 默认 true，priority 默认 100
function emptyModelRow(): ModelFormRow {
  return {
    public_model: '',
    protocol: 'chat_completions',
    upstream_model: '',
    direct: true,
    allow_compatibility_fallback: false,
    priority: 100,
    enabled: true,
    notes: '',
    billing_mode: 'token',
    input_price: null,
    output_price: null,
    cache_write_price: null,
    cache_read_price: null,
    fast_multiplier: null,
    flex_multiplier: null,
    image_input_price: null,
    image_output_price: null,
    per_request_price: null,
    intervals: [],
    time_pricing: null
  }
}

function emptyRouteRow(): RouteFormRow {
  return {
    group_id: null,
    priority: 0,
    enabled: true
  }
}

function createPlanForm() {
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

const form = ref(createPlanForm())

const title = computed(() =>
  props.planId == null
    ? t('admin.pricingPlans.createTitle')
    : t('admin.pricingPlans.editTitle', { name: form.value.name || '' })
)

const statusOptions = computed<SelectOption[]>(() => [
  { value: 'active', label: t('admin.pricingPlans.status.active') },
  { value: 'disabled', label: t('admin.pricingPlans.status.disabled') }
])

const protocolOptions = computed<SelectOption[]>(() =>
  PRICING_PLAN_PROTOCOLS.map((p) => ({ value: p, label: p }))
)

const billingModes = computed(() => [
  { value: 'token' as const, label: t('admin.pricingPlans.form.billingModes.token') },
  { value: 'per_request' as const, label: t('admin.pricingPlans.form.billingModes.perRequest') },
  { value: 'image' as const, label: t('admin.pricingPlans.form.billingModes.image') },
  { value: 'video' as const, label: t('admin.pricingPlans.form.billingModes.video') }
])

interface PriceFieldDef {
  key: keyof Pick<
    ModelFormRow,
    | 'input_price'
    | 'output_price'
    | 'cache_write_price'
    | 'cache_read_price'
    | 'fast_multiplier'
    | 'flex_multiplier'
    | 'image_input_price'
    | 'image_output_price'
    | 'per_request_price'
  >
  label: string
  placeholder: string
}

// 各计费模式的直白价格字段：token 用 token 单价/缓存/倍率，per_request/video
// 用按次价，image 用图片输入/输出价。区间/分时暂不提供编辑（保留后端明细）。
function priceFieldsFor(mode: PricingPlanBillingMode | ''): PriceFieldDef[] {
  const mk = (
    key: PriceFieldDef['key'],
    labelKey: string,
    placeholderKey: string
  ): PriceFieldDef => ({
    key,
    label: t(`admin.pricingPlans.form.prices.${labelKey}`),
    placeholder: t(`admin.pricingPlans.form.prices.${placeholderKey}`)
  })
  switch (mode) {
    case 'per_request':
      return [mk('per_request_price', 'perRequest', 'perRequestPlaceholder')]
    case 'image':
      return [mk('per_request_price', 'image', 'imagePlaceholder')]
    case 'video':
      return [mk('per_request_price', 'video', 'videoPlaceholder')]
    case 'token':
    case '':
    default:
      return [
        mk('input_price', 'input', 'pricePlaceholder'),
        mk('output_price', 'output', 'pricePlaceholder'),
        mk('cache_write_price', 'cacheWrite', 'pricePlaceholder'),
        mk('cache_read_price', 'cacheRead', 'pricePlaceholder'),
        mk('fast_multiplier', 'fastMultiplier', 'multiplierPlaceholder'),
        mk('flex_multiplier', 'flexMultiplier', 'multiplierPlaceholder')
      ]
  }
}

const groupOptions = computed<SelectOption[]>(() =>
  groups.value.map((g) => ({
    value: g.id,
    label: g.platform ? `${g.name} (${g.platform})` : g.name
  }))
)

function addModel() {
  form.value.models.push(emptyModelRow())
}

function removeModel(index: number) {
  form.value.models.splice(index, 1)
}

function selectBillingMode(row: ModelFormRow, mode: PricingPlanBillingMode | '') {
  row.billing_mode = mode
  if (mode !== 'token') {
    row.time_pricing = null
  }
}

function normalizeCompatibilityFallback(row: ModelFormRow) {
  if (row.direct || row.protocol !== 'chat_completions') {
    row.allow_compatibility_fallback = false
  }
}

// 价格输入：空串转 null，保持表单状态为 number | null（避免 v-model.number 的空串污染）
function onPriceInput(row: ModelFormRow, key: PriceFieldDef['key'], event: Event) {
  const raw = (event.target as HTMLInputElement).value
  row[key] = raw === '' ? null : Number(raw)
}

function addRoute() {
  form.value.routes.push(emptyRouteRow())
}

function removeRoute(index: number) {
  form.value.routes.splice(index, 1)
}

// 客户端重复校验：同一套餐内 group 与 priority 均不可重复。
// 冲突行（含首行）全部标红，方便一眼定位。
function groupConflictIndexes(): Set<number> {
  const byGroup = new Map<number, number[]>()
  form.value.routes.forEach((r, i) => {
    if (r.group_id == null) return
    const arr = byGroup.get(r.group_id) ?? []
    arr.push(i)
    byGroup.set(r.group_id, arr)
  })
  const conflicts = new Set<number>()
  for (const arr of byGroup.values()) {
    if (arr.length > 1) arr.forEach((i) => conflicts.add(i))
  }
  return conflicts
}

function priorityConflictIndexes(): Set<number> {
  const byPriority = new Map<number, number[]>()
  form.value.routes.forEach((r, i) => {
    const arr = byPriority.get(r.priority) ?? []
    arr.push(i)
    byPriority.set(r.priority, arr)
  })
  const conflicts = new Set<number>()
  for (const arr of byPriority.values()) {
    if (arr.length > 1) arr.forEach((i) => conflicts.add(i))
  }
  return conflicts
}

const groupConflicts = computed(() => groupConflictIndexes())
const priorityConflicts = computed(() => priorityConflictIndexes())

function routeGroupError(index: number): string {
  if (form.value.routes[index].group_id == null) {
    return t('admin.pricingPlans.form.routeGroupRequired')
  }
  if (groupConflicts.value.has(index)) {
    return t('admin.pricingPlans.form.duplicateGroup')
  }
  return ''
}

function routePriorityError(index: number): string {
  if (priorityConflicts.value.has(index)) {
    return t('admin.pricingPlans.form.duplicatePriority')
  }
  return ''
}

function hasRouteErrors(): boolean {
  return form.value.routes.some((_, i) => routeGroupError(i) !== '' || routePriorityError(i) !== '')
}

// 编辑模式：按 id 拉全量详情再回填（列表响应不含 models/routes 明细）
async function loadDetail() {
  if (props.planId == null) return
  detailLoading.value = true
  try {
    const detail = await adminAPI.pricingPlans.getById(props.planId)
    form.value = {
      name: detail.name,
      title: detail.title,
      description: detail.description,
      status: detail.status,
      is_public: detail.is_public,
      sort_order: detail.sort_order,
      models: detail.models.map(modelToForm),
      routes: detail.routes.map(routeToForm)
    }
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.pricingPlans.loadDetailError')))
    emit('close')
  } finally {
    detailLoading.value = false
  }
}

function modelToForm(m: AdminPricingPlanModel): ModelFormRow {
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

function routeToForm(r: AdminPricingPlanRoute): RouteFormRow {
  return {
    group_id: r.group_id,
    priority: r.priority,
    enabled: r.enabled
  }
}

// 组装 pricing 文档：只下发当前计费模式的字段 + 保留的区间/分时明细。
// 分时仅 token 模式支持，切换到其他模式时丢弃以免后端校验失败。
function buildPricingPayload(row: ModelFormRow): PlanModelPricing {
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

function buildPayload(): PricingPlanUpsertRequest {
  const models: PricingPlanModelInput[] = form.value.models.map((m) => ({
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
  const routes: PricingPlanRouteInput[] = form.value.routes.map((r) => ({
    group_id: r.group_id as number,
    priority: normalizeInt(r.priority, 0),
    enabled: r.enabled
  }))
  return {
    name: form.value.name,
    title: form.value.title,
    description: form.value.description,
    status: form.value.status,
    is_public: form.value.is_public,
    sort_order: normalizeInt(form.value.sort_order, 0),
    models,
    routes
  }
}

async function handleSubmit() {
  if (hasRouteErrors()) return
  saving.value = true
  try {
    const payload = buildPayload()
    if (props.planId == null) {
      await adminAPI.pricingPlans.create(payload)
    } else {
      await adminAPI.pricingPlans.update(props.planId, payload)
    }
    emit('saved')
    emit('close')
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.pricingPlans.saveError')))
  } finally {
    saving.value = false
  }
}

function handleClose() {
  if (saving.value) return
  emit('close')
}

// 加载分组选择项（内部路由层从 adminAPI.groups.getAll 选取）
async function loadGroups() {
  try {
    groups.value = await adminAPI.groups.getAll()
  } catch {
    // 分组加载失败不阻塞表单，保存时后端会兜底校验 group_id
  }
}

watch(
  () => [props.show, props.planId] as const,
  ([show]) => {
    if (!show) return
    form.value = createPlanForm()
    if (props.planId != null) {
      void loadDetail()
    }
  },
  { immediate: true }
)

loadGroups()
</script>