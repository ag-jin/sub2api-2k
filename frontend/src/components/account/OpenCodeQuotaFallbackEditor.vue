<template>
  <section
    class="rounded-lg border border-gray-200 dark:border-dark-600"
    data-testid="opencode-quota-fallback-editor"
  >
    <div class="flex items-start justify-between gap-4 p-4" :class="{ 'pb-0': config.enabled }">
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
        :model-value="config.enabled"
        :disabled="disabled"
        :aria-label="t('admin.accounts.openCodeQuotaFallback.title')"
        data-testid="opencode-quota-fallback-toggle"
        @update:model-value="setEnabled"
      />
    </div>

    <div v-if="config.enabled" class="space-y-4 p-4 pt-3">
      <div>
        <label class="input-label" :for="thresholdId">
          {{ t('admin.accounts.openCodeQuotaFallback.threshold') }}
        </label>
        <div class="flex max-w-xs items-center gap-2">
          <input
            :id="thresholdId"
            :value="config.threshold_percent"
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
          <div v-for="row in rows" :key="row.source" class="rounded-lg border border-gray-200 p-2.5 dark:border-dark-600">
            <div class="grid items-start gap-2 sm:grid-cols-[minmax(0,1fr)_1.25rem_minmax(0,1fr)_2rem]">
              <div class="min-w-0 rounded-md bg-gray-50 px-2.5 py-2 font-mono text-sm text-gray-800 dark:bg-dark-800 dark:text-gray-200" :title="row.source">
                <span class="block truncate">{{ row.source }}</span>
              </div>
              <div class="hidden justify-center pt-2.5 text-gray-400 dark:text-dark-400 sm:flex" aria-hidden="true">
                <Icon name="arrowRight" size="sm" />
              </div>
              <div class="min-w-0">
                <Select
                  :model-value="row.target"
                  :options="replacementOptions(row.source, row.target)"
                  :disabled="disabled"
                  :error="Boolean(rowError(row.source, row.target))"
                  searchable
                  creatable
                  clearable
                  :placeholder="t('admin.accounts.openCodeQuotaFallback.targetPlaceholder')"
                  :search-placeholder="t('admin.accounts.openCodeQuotaFallback.targetPlaceholder')"
                  :empty-text="t('admin.accounts.openCodeQuotaFallback.candidatesEmpty')"
                  :aria-label="t('admin.accounts.openCodeQuotaFallback.targetColumn')"
                  :data-testid="`opencode-quota-fallback-target-${row.source}`"
                  @update:model-value="setTarget(row.source, $event)"
                />
              </div>
              <button
                type="button"
                class="flex h-9 w-9 items-center justify-center self-start rounded-lg text-red-500 transition-colors hover:bg-red-50 hover:text-red-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-red-900/20"
                :disabled="disabled"
                :aria-label="t('admin.accounts.openCodeQuotaFallback.removeRow')"
                :data-testid="`opencode-quota-fallback-remove-${row.source}`"
                @click="removeMapping(row.source)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
            <p v-if="rowError(row.source, row.target)" class="mt-1.5 text-xs leading-4 text-red-700 dark:text-red-300">
              {{ rowError(row.source, row.target) }}
            </p>
          </div>
        </div>

        <div
          v-else
          class="rounded-lg border border-dashed border-gray-200 bg-gray-50 px-3 py-4 text-center text-xs leading-4 text-gray-500 dark:border-dark-600 dark:bg-dark-800/60 dark:text-gray-400"
          data-testid="opencode-quota-fallback-empty"
        >
          {{ modelOptions.length > 0
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
            :disabled="disabled || availableSources.length === 0"
            data-testid="opencode-quota-fallback-add"
            @click="addMapping"
          >
            <Icon name="plus" size="sm" />
            {{ t('admin.accounts.openCodeQuotaFallback.addRow') }}
          </button>
        </div>

        <p v-if="candidatesState === 'loading'" class="mt-2 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.openCodeQuotaFallback.candidatesLoading') }}
        </p>
        <p v-else-if="candidatesState === 'error'" class="mt-2 text-xs text-amber-700 dark:text-amber-300">
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
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import {
  createOpenCodeQuotaFallbackConfig,
  normalizeOpenCodeQuotaFallbackConfig,
  parseOpenCodeQuotaFallbackConfig,
  validateOpenCodeQuotaFallbackConfig,
  type OpenCodeQuotaFallbackCandidate,
  type OpenCodeQuotaFallbackConfig
} from './openCodeQuotaFallback'

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

const config = computed<OpenCodeQuotaFallbackConfig>(() =>
  parseOpenCodeQuotaFallbackConfig(props.modelValue) ?? createOpenCodeQuotaFallbackConfig()
)

const normalizedModelOptions = computed(() => uniqueNonEmpty(props.modelOptions))

// Preserve persisted rows that became absent from the latest normal-model list so opening and saving the modal
// cannot silently erase an administrator's configuration. They remain visibly stale/editable until removed.
const rows = computed(() => {
  const configuredSources = Object.keys(config.value.model_mapping)
  const sourceOrder = uniqueNonEmpty([...normalizedModelOptions.value, ...configuredSources])
  return sourceOrder
    .filter(source => Object.prototype.hasOwnProperty.call(config.value.model_mapping, source))
    .map(source => ({ source, target: config.value.model_mapping[source] ?? '' }))
})

const availableSources = computed(() => normalizedModelOptions.value.filter(
  source => !Object.prototype.hasOwnProperty.call(config.value.model_mapping, source)
))

const candidateByID = computed(() => {
  const result = new Map<string, OpenCodeQuotaFallbackCandidate>()
  for (const candidate of props.candidates) {
    const id = candidate.id?.trim()
    if (id && !result.has(id)) result.set(id, { ...candidate, id })
  }
  return result
})

const configurationError = computed(() => {
  const validation = validateOpenCodeQuotaFallbackConfig(config.value)
  if (!validation) return ''
  return t(`admin.accounts.openCodeQuotaFallback.errors.${validation}`)
})

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

function update(next: Partial<OpenCodeQuotaFallbackConfig>) {
  emit('update:modelValue', normalizeOpenCodeQuotaFallbackConfig({
    enabled: config.value.enabled,
    threshold_percent: config.value.threshold_percent,
    model_mapping: { ...config.value.model_mapping },
    ...next
  }))
}

function setEnabled(enabled: boolean) {
  // Disabling is intentionally non-destructive: threshold and mappings remain available for a later re-enable.
  update({ enabled })
}

function setThreshold(rawValue: string) {
  update({ threshold_percent: parseInputThreshold(rawValue) })
}

function addMapping() {
  const source = availableSources.value[0]
  if (!source) return
  update({ model_mapping: { ...config.value.model_mapping, [source]: '' } })
}

function removeMapping(source: string) {
  const mapping = { ...config.value.model_mapping }
  delete mapping[source]
  update({ model_mapping: mapping })
}

function setTarget(source: string, value: string | number | boolean | null) {
  const target = typeof value === 'string' ? value.trim() : ''
  update({ model_mapping: { ...config.value.model_mapping, [source]: target } })
}

function replacementOptions(source: string, selectedTarget: string): SelectOption[] {
  const options: SelectOption[] = []
  for (const candidate of candidateByID.value.values()) {
    const isUnavailable = candidate.capability === 'unavailable'
    const isSameAsSource = candidate.id === source
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
  if (selectedTarget && !candidateByID.value.has(selectedTarget)) {
    options.unshift({ value: selectedTarget, label: selectedTarget })
  }
  return options
}

function rowError(source: string, target: string): string {
  if (!target || source === target) return t('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')
  return ''
}
</script>
