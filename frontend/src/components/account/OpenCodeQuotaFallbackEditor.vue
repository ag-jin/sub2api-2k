<template>
  <section
    class="rounded-lg border border-gray-200 dark:border-dark-600"
    data-testid="opencode-quota-fallback-editor"
  >
    <div class="flex items-start justify-between gap-4 p-4" :class="{ 'pb-0': enabled }">
      <div class="min-w-0">
        <label class="input-label mb-0" :for="toggleId">
          {{ t('admin.accounts.openCodeQuotaFallback.title') }}
        </label>
        <p class="mt-1 text-xs leading-4 text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.openCodeQuotaFallback.hint') }}
        </p>
      </div>
      <Toggle
        :id="toggleId"
        :model-value="enabled"
        :disabled="disabled"
        :aria-label="t('admin.accounts.openCodeQuotaFallback.title')"
        data-testid="opencode-quota-fallback-toggle"
        @update:model-value="setEnabled"
      />
    </div>

    <div v-if="enabled" class="space-y-4 p-4 pt-3">
      <div>
        <label class="input-label" :for="thresholdId">
          {{ t('admin.accounts.openCodeQuotaFallback.threshold') }}
        </label>
        <div class="flex max-w-xs items-center gap-2">
          <input
            :id="thresholdId"
            :value="threshold"
            type="number"
            min="1"
            max="100"
            step="1"
            inputmode="numeric"
            class="input w-24"
            :disabled="disabled"
            data-testid="opencode-quota-fallback-threshold"
            @change="setThreshold(($event.target as HTMLInputElement).value)"
          />
          <span class="text-sm text-gray-600 dark:text-gray-300">%</span>
        </div>
        <p class="input-hint mt-1">{{ t('admin.accounts.openCodeQuotaFallback.thresholdHint') }}</p>
      </div>

      <div class="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs leading-4 text-amber-800 dark:border-amber-800 dark:bg-amber-900/30 dark:text-amber-200">
        <div class="flex gap-2">
          <Icon name="exclamationTriangle" size="sm" class="mt-0.5 shrink-0" />
          <p>{{ t('admin.accounts.openCodeQuotaFallback.freePolicyNotice') }}</p>
        </div>
      </div>

      <div>
        <div class="mb-2 grid gap-2 text-xs font-medium text-gray-600 dark:text-gray-400 sm:grid-cols-[minmax(0,1fr)_1.25rem_minmax(0,1fr)_2rem]">
          <span>{{ t('admin.accounts.openCodeQuotaFallback.sourceColumn') }}</span>
          <span class="hidden sm:block" aria-hidden="true"></span>
          <span>{{ t('admin.accounts.openCodeQuotaFallback.targetColumn') }}</span>
          <span class="hidden sm:block" aria-hidden="true"></span>
        </div>

        <div v-if="rows.length > 0" class="space-y-2">
          <div v-for="row in rows" :key="row.uid" class="rounded-lg border border-gray-200 p-2.5 dark:border-dark-600">
            <div class="grid items-start gap-2 sm:grid-cols-[minmax(0,1fr)_1.25rem_minmax(0,1fr)_2rem]">
              <div class="min-w-0">
                <input
                  :value="row.source"
                  type="text"
                  class="input min-w-0 font-mono text-sm"
                  :list="sourceOptionsId"
                  :disabled="disabled"
                  :placeholder="t('admin.accounts.openCodeQuotaFallback.sourcePlaceholder')"
                  :aria-label="t('admin.accounts.openCodeQuotaFallback.sourceColumn')"
                  :aria-invalid="Boolean(row.sourceError)"
                  :data-testid="`opencode-quota-fallback-source-${rowTestId(row)}`"
                  @input="onSourceInput(row, $event)"
                  @blur="commitSource(row)"
                  @keydown.enter="onSourceEnter(row, $event)"
                />
              </div>
              <div class="hidden justify-center pt-2.5 text-gray-400 dark:text-dark-400 sm:flex" aria-hidden="true">
                <Icon name="arrowRight" size="sm" />
              </div>
              <div class="min-w-0">
                <Select
                  :model-value="row.target"
                  :options="replacementOptions(row)"
                  :disabled="disabled"
                  :error="Boolean(rowError(row))"
                  searchable
                  creatable
                  clearable
                  :placeholder="t('admin.accounts.openCodeQuotaFallback.targetPlaceholder')"
                  :search-placeholder="t('admin.accounts.openCodeQuotaFallback.targetPlaceholder')"
                  :empty-text="t('admin.accounts.openCodeQuotaFallback.candidatesEmpty')"
                  :aria-label="t('admin.accounts.openCodeQuotaFallback.targetColumn')"
                  :data-testid="`opencode-quota-fallback-target-${rowTestId(row)}`"
                  @update:model-value="setTarget(row, $event)"
                />
              </div>
              <button
                type="button"
                class="flex h-9 w-9 items-center justify-center self-start rounded-lg text-red-500 transition-colors hover:bg-red-50 hover:text-red-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-red-900/20"
                :disabled="disabled"
                :aria-label="t('admin.accounts.openCodeQuotaFallback.removeRow')"
                :data-testid="`opencode-quota-fallback-remove-${rowTestId(row)}`"
                @click="removeMapping(row)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
            <p v-if="rowError(row)" class="mt-1.5 text-xs leading-4 text-red-700 dark:text-red-300">
              {{ rowError(row) }}
            </p>
          </div>
        </div>

        <datalist :id="sourceOptionsId" data-testid="opencode-quota-fallback-source-options">
          <option v-for="option in normalizedModelOptions" :key="option" :value="option" />
        </datalist>

        <div
          v-if="rows.length === 0"
          class="rounded-lg border border-dashed border-gray-200 bg-gray-50 px-3 py-4 text-center text-xs leading-4 text-gray-500 dark:border-dark-600 dark:bg-dark-800/60 dark:text-gray-400"
          data-testid="opencode-quota-fallback-empty"
        >
          {{ normalizedModelOptions.length > 0
            ? t('admin.accounts.openCodeQuotaFallback.mappingHint')
            : t('admin.accounts.openCodeQuotaFallback.noSourceOptions') }}
        </div>

        <div class="mt-2 flex flex-wrap items-center gap-2">
          <button
            type="button"
            class="btn btn-secondary btn-sm"
            :disabled="disabled || candidatesState === 'loading'"
            data-testid="opencode-quota-fallback-sync-targets"
            @click="emit('sync-targets')"
          >
            <Icon name="refresh" size="xs" class="mr-1.5" :class="{ 'animate-spin': candidatesState === 'loading' }" />
            {{ t('admin.accounts.openCodeQuotaFallback.candidatesRefresh') }}
          </button>
          <button
            type="button"
            class="inline-flex min-h-9 items-center gap-2 rounded-lg border border-dashed border-gray-300 px-3 py-1.5 text-sm text-gray-600 transition-colors hover:border-gray-400 hover:text-gray-700 disabled:cursor-not-allowed disabled:opacity-50 dark:border-dark-500 dark:text-gray-400 dark:hover:border-dark-400 dark:hover:text-gray-300"
            :disabled="disabled"
            data-testid="opencode-quota-fallback-add"
            @click="addMapping"
          >
            <Icon name="plus" size="sm" />
            {{ t('admin.accounts.openCodeQuotaFallback.addRow') }}
          </button>
        </div>

        <p v-if="candidatesState === 'loading'" class="mt-2 text-xs text-gray-500 dark:text-gray-400" aria-live="polite">
          {{ t('admin.accounts.openCodeQuotaFallback.candidatesLoading') }}
        </p>
        <p v-else-if="candidatesState === 'error'" class="mt-2 text-xs text-amber-700 dark:text-amber-300" aria-live="polite">
          {{ t('admin.accounts.openCodeQuotaFallback.candidatesError') }}
        </p>
      </div>

      <p
        v-if="error || configurationError"
        class="flex gap-2 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs leading-4 text-red-700 dark:border-red-900/60 dark:bg-red-900/20 dark:text-red-300"
        role="alert"
        data-testid="opencode-quota-fallback-validation"
      >
        <Icon name="exclamationCircle" size="sm" class="mt-0.5 shrink-0" />
        <span>{{ error || configurationError }}</span>
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import {
  createOpenCodeQuotaFallbackConfig,
  isOpenCodeQuotaFallbackSourceTaken,
  normalizeOpenCodeQuotaFallbackConfig,
  parseOpenCodeQuotaFallbackConfig,
  validateOpenCodeQuotaFallbackConfig,
  type OpenCodeQuotaFallbackCandidate,
  type OpenCodeQuotaFallbackConfig,
  type OpenCodeQuotaFallbackSubmitError
} from './openCodeQuotaFallback'

interface EditableRow {
  uid: number
  /** 已提交到 model_mapping 的 key；空字符串表示尚未提交的草稿行。 */
  persistedSource: string
  /** 输入框中的文本，失焦/回车后才提交；编辑期间允许与 persistedSource 不同。 */
  source: string
  target: string
  sourceError: '' | 'sourceRequired' | 'duplicateSource'
}

const props = withDefaults(defineProps<{
  modelValue: OpenCodeQuotaFallbackConfig
  /** Normal, real upstream IDs. Client aliases must never appear in this list or as mapping keys. */
  modelOptions: string[]
  candidates?: OpenCodeQuotaFallbackCandidate[]
  candidatesState?: 'idle' | 'loading' | 'ready' | 'error'
  /** A translated submission/API error supplied by the parent modal. */
  error?: string | null
  disabled?: boolean
}>(), {
  candidates: () => [],
  candidatesState: 'idle',
  error: null,
  disabled: false
})

const emit = defineEmits<{
  (e: 'update:modelValue', value: OpenCodeQuotaFallbackConfig): void
  /** Parent refreshes the current upstream/catalog target list; the editor makes no request itself. */
  (e: 'sync-targets'): void
}>()

const { t } = useI18n()
const toggleId = 'opencode-quota-fallback-enabled'
const thresholdId = 'opencode-quota-fallback-threshold'
const sourceOptionsId = `opencode-quota-fallback-source-options-${useId()}`

const config = computed<OpenCodeQuotaFallbackConfig>(() =>
  parseOpenCodeQuotaFallbackConfig(props.modelValue) ?? createOpenCodeQuotaFallbackConfig()
)

const enabled = ref(config.value.enabled)
const threshold = ref(config.value.threshold_percent)

let nextRowUid = 1

function createRow(persistedSource = '', target = ''): EditableRow {
  return {
    uid: nextRowUid++,
    persistedSource,
    source: persistedSource,
    target,
    sourceError: ''
  }
}

const rows = ref<EditableRow[]>(
  Object.entries(config.value.model_mapping).map(([source, target]) => createRow(source, target))
)

const normalizedModelOptions = computed(() => uniqueNonEmpty(props.modelOptions))

const candidateByID = computed(() => {
  const result = new Map<string, OpenCodeQuotaFallbackCandidate>()
  for (const candidate of props.candidates) {
    const id = candidate.id?.trim()
    if (id && !result.has(id)) result.set(id, { ...candidate, id })
  }
  return result
})

const configurationError = computed(() => {
  const validation = validateOpenCodeQuotaFallbackConfig({
    enabled: enabled.value,
    threshold_percent: threshold.value,
    model_mapping: mappingFromRows()
  })
  if (!validation) return ''
  return t(`admin.accounts.openCodeQuotaFallback.errors.${validation}`)
})

function configSignature(value: OpenCodeQuotaFallbackConfig): string {
  const mapping = Object.keys(value.model_mapping)
    .sort()
    .map(source => [source, value.model_mapping[source] ?? ''])
  return JSON.stringify([value.enabled === true, value.threshold_percent, mapping])
}

function mappingFromRows(): Record<string, string> {
  const entries: Array<[string, string]> = []
  for (const row of rows.value) {
    if (!row.persistedSource) continue
    entries.push([row.persistedSource, row.target])
  }
  return Object.fromEntries(entries)
}

function normalizedLocalConfig(): OpenCodeQuotaFallbackConfig {
  return normalizeOpenCodeQuotaFallbackConfig({
    enabled: enabled.value,
    threshold_percent: threshold.value,
    model_mapping: mappingFromRows()
  })
}

function localSignature(): string {
  return configSignature(normalizedLocalConfig())
}

watch(
  () => props.modelValue,
  (value) => {
    const incoming = parseOpenCodeQuotaFallbackConfig(value) ?? createOpenCodeQuotaFallbackConfig()
    // 自身 emit 的回显/同值刷新：本地已一致，保留输入框中尚未提交的草稿文本。
    if (configSignature(incoming) === localSignature()) return
    // 其余外部变更一律以父级 v-model 为准（父级在账号/平台上下文变化时用组件 key 重建本组件，
    // 未提交的草稿行因此只在同一挂载上下文内保留）。
    applyExternalConfig(incoming)
  }
)

function applyExternalConfig(incoming: OpenCodeQuotaFallbackConfig) {
  enabled.value = incoming.enabled
  threshold.value = incoming.threshold_percent
  const entries = Object.entries(incoming.model_mapping)
  const incomingBySource = new Map(entries)
  const seen = new Set<string>()
  const nextRows: EditableRow[] = []
  for (const row of rows.value) {
    if (!row.persistedSource) {
      // 未提交的草稿行不受外部刷新影响。
      nextRows.push({ ...row })
      continue
    }
    const target = incomingBySource.get(row.persistedSource)
    if (target === undefined) continue
    seen.add(row.persistedSource)
    // 保留仍在编辑中的 source 文本；target 以外部配置为准。
    nextRows.push({ ...row, target, sourceError: '' })
  }
  for (const [source, target] of entries) {
    if (seen.has(source)) continue
    nextRows.push(createRow(source, target))
  }
  rows.value = nextRows
}

function uniqueNonEmpty(values: string[] | undefined): string[] {
  const seen = new Set<string>()
  const result: string[] = []
  for (const value of values ?? []) {
    const normalized = typeof value === 'string' ? value.trim() : ''
    if (normalized && !seen.has(normalized)) {
      seen.add(normalized)
      result.push(normalized)
    }
  }
  return result
}

function parseInputThreshold(value: string): number {
  // Preserve finite invalid input (for example 1.5, 0, or 101) so the shared validator can
  // surface the frozen contract error rather than silently changing the administrator's value.
  return Number(value)
}

function emitFromRows() {
  emit('update:modelValue', normalizedLocalConfig())
}

/** 关闭/关闭态保存时的显式草稿回退：保留已提交行（先提交未失焦的 source），丢弃草稿行。 */
function revertUncommittedDrafts() {
  for (const row of rows.value) {
    if (row.persistedSource && row.persistedSource !== row.source.trim()) commitSource(row)
  }
  rows.value = rows.value.filter(row => row.persistedSource)
}

function setEnabled(value: boolean) {
  // 关闭是显式的“草稿回退”动作：关闭按钮始终可用，也不会在保存时静默丢行。
  if (!value) revertUncommittedDrafts()
  // Disabling is intentionally non-destructive for persisted threshold/mappings.
  enabled.value = value
  emitFromRows()
}

function setThreshold(rawValue: string) {
  threshold.value = parseInputThreshold(rawValue)
  emitFromRows()
}

function addMapping() {
  if (props.disabled) return
  // 新行只创建本地草稿：source/target 都不从目录或普通映射中预选。
  rows.value.push(createRow())
}

function removeMapping(row: EditableRow) {
  rows.value = rows.value.filter(candidate => candidate.uid !== row.uid)
  if (row.persistedSource) emitFromRows()
}

function onSourceInput(row: EditableRow, event: Event) {
  row.source = (event.target as HTMLInputElement).value
  row.sourceError = ''
}

function onSourceEnter(row: EditableRow, event: KeyboardEvent) {
  if (event.isComposing || event.keyCode === 229) return
  // 编辑器位于创建/编辑表单内：回车只提交当前行，不能触发外层表单的隐式提交。
  event.preventDefault()
  event.stopPropagation()
  commitSource(row)
}

function commitSource(row: EditableRow) {
  if (props.disabled) return
  const next = row.source.trim()
  if (!next) {
    row.source = row.persistedSource
    row.sourceError = row.persistedSource ? 'sourceRequired' : ''
    return
  }
  if (row.persistedSource === next) {
    row.source = next
    row.sourceError = ''
    return
  }
  if (isSourceTakenByOtherRow(row, next)) {
    // 重复时不改写任何已提交 key：输入框回退到该行当前持久值，界面与将保存的 key 保持一致。
    row.source = row.persistedSource
    row.sourceError = 'duplicateSource'
    return
  }
  row.source = next
  row.sourceError = ''
  row.persistedSource = next
  emitFromRows()
}

function isSourceTakenByOtherRow(row: EditableRow, source: string): boolean {
  const taken = rows.value
    .filter(candidate => candidate.uid !== row.uid)
    .map(candidate => candidate.source.trim() || candidate.persistedSource)
  return isOpenCodeQuotaFallbackSourceTaken(source, taken)
}

function setTarget(row: EditableRow, value: string | number | boolean | null) {
  const target = typeof value === 'string' ? value.trim() : ''
  if (row.target === target) return
  row.target = target
  // 草稿行在 source 提交前只保留本地 target，不写入旧 key。
  if (row.persistedSource) emitFromRows()
}

function replacementOptions(row: EditableRow): SelectOption[] {
  const source = row.source.trim() || row.persistedSource
  const options: SelectOption[] = []
  for (const candidate of candidateByID.value.values()) {
    const isUnavailable = candidate.capability === 'unavailable'
    const isSameAsSource = Boolean(source) && candidate.id === source
    const label = candidate.label && candidate.label !== candidate.id
      ? `${candidate.label} (${candidate.id})`
      : candidate.id
    options.push({
      value: candidate.id,
      label: candidate.capability === 'unknown'
        ? `${label} · ${t('admin.accounts.openCodeQuotaFallback.candidateUnknown')}`
        : label,
      disabled: isUnavailable || isSameAsSource
    })
  }
  // A saved custom/stale ID stays visible, even when it is no longer in the latest catalog.
  if (row.target && !candidateByID.value.has(row.target)) {
    options.unshift({ value: row.target, label: row.target })
  }
  return options
}

function rowError(row: EditableRow): string {
  if (row.sourceError) {
    return t(`admin.accounts.openCodeQuotaFallback.errors.${row.sourceError}`)
  }
  if (!row.persistedSource) return ''
  if (!row.target || row.target === row.persistedSource) {
    return t('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')
  }
  return ''
}

function rowTestId(row: EditableRow): string {
  return row.persistedSource || `draft-${row.uid}`
}

/**
 * 父级表单提交前的门禁：先提交未失焦的 source 文本，再拒绝未落成 key 的草稿行
 * （典型：只选了 target 的新行），避免 mappingFromRows() 忽略草稿导致父级静默丢行。
 * 关闭态不在此门禁内：setEnabled(false) 已显式回收草稿，关闭操作始终可完成。
 */
function validateForSubmit(): OpenCodeQuotaFallbackSubmitError | null {
  if (!enabled.value) {
    // 关闭态保存：显式回退草稿（不强迫用户补完空行），已提交行照常保存。
    revertUncommittedDrafts()
    return null
  }
  for (const row of rows.value) {
    if (row.persistedSource !== row.source.trim()) commitSource(row)
  }
  for (const row of rows.value) {
    if (row.sourceError) return row.sourceError
  }
  const draftRow = rows.value.find(row => !row.persistedSource)
  if (draftRow) {
    draftRow.sourceError = 'sourceRequired'
    return 'sourceRequired'
  }
  return validateOpenCodeQuotaFallbackConfig(normalizedLocalConfig())
}

defineExpose({ validateForSubmit })
</script>
