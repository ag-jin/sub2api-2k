<template>
  <div class="space-y-3">
    <!-- 一级:套餐 -->
    <div class="flex items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.planLabel') }}
      </span>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(plan === 'all')"
          @click="$emit('update:plan', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="pl in plans"
          :key="`plan-${pl.code}`"
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="plan === pl.code ? 'chip-tinted-active' : 'chip-tinted'"
          :disabled="!planEnabled(pl.code)"
          @click="$emit('update:plan', pl.code)"
        >
          {{ pl.name }}
        </button>
      </div>
    </div>

    <!-- 二级:协议(当前套餐组合下无结果的置灰) -->
    <div class="flex items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.protocolLabel') }}
      </span>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(protocol === 'all')"
          @click="$emit('update:protocol', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="p in protocols"
          :key="`protocol-${p}`"
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="protocol === p ? 'chip-tinted-active' : 'chip-tinted'"
          :style="{ '--chip-accent': platformAccentColor(p) }"
          :disabled="!protocolEnabled(p)"
          @click="$emit('update:protocol', p)"
        >
          {{ p }}
        </button>
      </div>
    </div>

    <!-- 三级:模型搜索(展示名/模型标识,纯前端过滤) -->
    <div class="flex flex-wrap items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.modelLabel') }}
      </span>
      <div class="relative w-full sm:w-72">
        <Icon
          name="search"
          size="sm"
          class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-500"
        />
        <input
          :value="search"
          type="text"
          :placeholder="t('modelPlaza.filters.searchPlaceholder')"
          class="input rounded-lg py-1.5 pl-9 pr-9"
          @input="$emit('update:search', ($event.target as HTMLInputElement).value)"
        />
        <button
          v-if="search"
          type="button"
          aria-label="clear-search"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-400 transition-colors hover:text-gray-600 dark:text-dark-500 dark:hover:text-gray-300"
          @click="$emit('update:search', '')"
        >
          <Icon name="x" size="xs" class="h-3.5 w-3.5" />
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { platformAccentColor } from '@/utils/platformColors'
import type { PricingPlan } from '@/api/modelPlaza'

const props = defineProps<{
  /** 数据中出现的全量套餐(含模型/协议),置灰联动由此推导。 */
  plans: PricingPlan[]
  /** 数据中出现的协议(去重排序后)。 */
  protocols: string[]
  plan: string
  protocol: string
  /** 模型名/标识搜索词(纯前端过滤)。 */
  search: string
}>()

defineEmits<{
  'update:plan': [value: string]
  'update:protocol': [value: string]
  'update:search': [value: string]
}>()

const { t } = useI18n()

/**
 * 两个维度互为约束(faceted):某选项可点 ⟺ 在「另一维」当前选择下仍有协议行命中;
 * 模型搜索不参与置灰(只是行级过滤)。「全部」永远可点,可点项组合恒有结果。
 */
function planEnabled(code: string): boolean {
  return props.plans.some(
    (pl) =>
      pl.code === code &&
      pl.models.some((m) =>
        m.protocols.some((p) => props.protocol === 'all' || p.protocol === props.protocol)
      )
  )
}

function protocolEnabled(p: string): boolean {
  return props.plans.some(
    (pl) =>
      (props.plan === 'all' || pl.code === props.plan) &&
      pl.models.some((m) => m.protocols.some((row) => row.protocol === p))
  )
}

function chipClass(active: boolean): string {
  return active
    ? 'bg-gradient-to-r from-primary-500 to-primary-600 text-white shadow-sm shadow-primary-500/30'
    : 'bg-white text-gray-600 ring-1 ring-inset ring-gray-200 enabled:hover:bg-gray-50 enabled:hover:text-gray-900 enabled:hover:ring-gray-300 dark:bg-dark-800/60 dark:text-dark-300 dark:ring-dark-700 dark:enabled:hover:bg-dark-800 dark:enabled:hover:text-white'
}
</script>

<style scoped>
/* 协议 chip 的配色统一从 --chip-accent(协议主色)派生,新增协议无需扩展样式。
   激活态与非激活态在模板上互斥挂载,避免选择器优先级互相覆盖。 */
.chip-tinted {
  color: var(--chip-accent);
  color: color-mix(in srgb, var(--chip-accent) 78%, black);
  background-color: color-mix(in srgb, var(--chip-accent) 9%, transparent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--chip-accent) 25%, transparent);
}

.chip-tinted:not(:disabled):hover {
  background-color: color-mix(in srgb, var(--chip-accent) 16%, transparent);
}

.dark .chip-tinted {
  color: color-mix(in srgb, var(--chip-accent) 72%, white);
  background-color: color-mix(in srgb, var(--chip-accent) 12%, transparent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--chip-accent) 30%, transparent);
}

.dark .chip-tinted:not(:disabled):hover {
  background-color: color-mix(in srgb, var(--chip-accent) 18%, transparent);
}

.chip-tinted-active {
  color: #fff;
  background-color: var(--chip-accent);
  background-color: color-mix(in srgb, var(--chip-accent) 85%, black);
  box-shadow: 0 1px 2px 0 color-mix(in srgb, var(--chip-accent) 35%, transparent);
}

.chip-tinted-active:not(:disabled):hover {
  background-color: color-mix(in srgb, var(--chip-accent) 75%, black);
}

.dark .chip-tinted-active {
  background-color: color-mix(in srgb, var(--chip-accent) 80%, transparent);
}

.dark .chip-tinted-active:not(:disabled):hover {
  background-color: var(--chip-accent);
}
</style>