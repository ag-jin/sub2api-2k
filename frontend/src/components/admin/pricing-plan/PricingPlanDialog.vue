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
          <button type="button" data-testid="add-model" class="btn btn-secondary btn-sm" @click="form.models.push(emptyModelRow())">
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
            <button type="button" data-testid="remove-model" class="btn btn-ghost btn-sm text-red-500 hover:bg-red-50 dark:hover:bg-red-900/20" @click="form.models.splice(index, 1)">
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
          <button type="button" data-testid="add-route" class="btn btn-secondary btn-sm" @click="form.routes.push(emptyRouteRow())">
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
            <button type="button" data-testid="remove-route" class="btn btn-ghost btn-sm text-red-500 hover:bg-red-50 dark:hover:bg-red-900/20" @click="form.routes.splice(index, 1)">
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
import { PRICING_PLAN_PROTOCOLS, type PricingPlanBillingMode } from '@/api/admin/pricingPlans'
import {
  buildPayload,
  createPlanForm,
  emptyModelRow,
  emptyRouteRow,
  groupConflictIndexes,
  modelToForm,
  normalizeCompatibilityFallback,
  onPriceInput,
  priorityConflictIndexes,
  routeToForm,
  selectBillingMode,
  type PriceKey
} from './planForm'
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

const detailLoading = ref(false)
const saving = ref(false)
const groups = ref<Awaited<ReturnType<typeof adminAPI.groups.getAll>>>([])

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
  key: PriceKey
  label: string
  placeholder: string
}

// 各计费模式的直白价格字段：token 用 token 单价/缓存/倍率，per_request/video
// 用按次价，image 用图片输入/输出价。区间/分时暂不提供编辑（保留后端明细）。
function priceFieldsFor(mode: PricingPlanBillingMode | ''): PriceFieldDef[] {
  const mk = (
    key: PriceKey,
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

const groupConflicts = computed(() => groupConflictIndexes(form.value.routes))
const priorityConflicts = computed(() => priorityConflictIndexes(form.value.routes))

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
    // 先取新建默认形态，再用详情整体覆盖（与 createPlanForm 同一套键）
    form.value = {
      ...createPlanForm(),
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

async function handleSubmit() {
  if (hasRouteErrors()) return
  saving.value = true
  try {
    const payload = buildPayload(form.value)
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