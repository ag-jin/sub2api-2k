<template>
  <div class="space-y-5">
    <!-- 页头(独立形态下展示标题;后台形态 AppHeader 已有页面标题) -->
    <div v-if="!embedded">
      <h1 class="text-2xl font-bold tracking-tight text-gray-900 dark:text-white sm:text-3xl">{{ t('modelPlaza.title') }}</h1>
      <p class="mt-1.5 text-sm text-gray-500 dark:text-dark-400">{{ t('modelPlaza.description') }}</p>
    </div>

    <!-- 全局价格说明(管理员配置,Markdown) -->
    <div
      v-if="descriptionHtml"
      class="plaza-description rounded-2xl border border-gray-100 bg-white px-5 py-4 text-sm shadow-card dark:border-dark-700/50 dark:bg-dark-800/50"
      v-html="descriptionHtml"
    ></div>

    <!-- 未登录提示 -->
    <p
      v-if="!isAuthenticated"
      class="flex items-center gap-1.5 text-xs text-gray-400 dark:text-dark-500"
    >
      <Icon name="infoCircle" size="xs" class="h-3.5 w-3.5" />
      {{ t('modelPlaza.anonymousHint') }}
    </p>

    <!-- 加载/错误/空 -->
    <div v-if="loading" class="flex min-h-[240px] items-center justify-center">
      <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-600/25 border-t-primary-600 dark:border-primary-400/25 dark:border-t-primary-400"></div>
    </div>
    <div
      v-else-if="error"
      class="rounded-2xl border border-red-200 bg-red-50 px-5 py-8 text-center text-sm text-red-600 dark:border-red-500/30 dark:bg-red-500/10 dark:text-red-300"
    >
      {{ t('modelPlaza.loadFailed') }}
    </div>
    <template v-else>
      <!-- 筛选区:套餐 → 协议 → 模型搜索 -->
      <PlazaFilterBar
        :plans="respPlans"
        :protocols="protocols"
        :plan="selectedPlan"
        :protocol="selectedProtocol"
        :search="searchQuery"
        @update:plan="selectedPlan = $event"
        @update:protocol="selectedProtocol = $event"
        @update:search="searchQuery = $event"
      />

      <!-- 套餐分节的目录表(仅渲染有命中的套餐) -->
      <div v-if="filteredPlans.length > 0" class="space-y-5">
        <PlazaPlanSection v-for="plan in filteredPlans" :key="plan.code" :plan="plan" />
      </div>
      <div
        v-else
        class="rounded-2xl border border-dashed border-gray-300 px-5 py-12 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-dark-400"
      >
        {{ searchActive ? t('modelPlaza.noSearchResult') : t('modelPlaza.empty') }}
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Icon from '@/components/icons/Icon.vue'
import PlazaFilterBar from './PlazaFilterBar.vue'
import PlazaPlanSection from './PlazaPlanSection.vue'
import type { CatalogModel, PricingCatalog } from '@/api/modelPlaza'
import { useAuthStore } from '@/stores/auth'

const props = defineProps<{
  response: PricingCatalog | null
  loading: boolean
  error?: boolean
  /** 后台内嵌形态(AppLayout 内):隐藏页头。 */
  embedded?: boolean
}>()

const { t } = useI18n()
const authStore = useAuthStore()
const isAuthenticated = computed(() => authStore.isAuthenticated)

const selectedPlan = ref<string>('all')
const selectedProtocol = ref<string>('all')
const searchQuery = ref('')

const searchActive = computed(() => searchQuery.value.trim() !== '')

const descriptionHtml = computed(() => {
  const md = props.response?.description?.trim()
  if (!md) return ''
  return DOMPurify.sanitize(marked.parse(md) as string)
})

const respPlans = computed(() => props.response?.plans ?? [])

/** 数据中出现的协议(全目录去重排序)。 */
const protocols = computed(() =>
  [...new Set(respPlans.value.flatMap((p) => p.models.flatMap((m) => m.protocols.map((row) => row.protocol))))].sort()
)

/** 数据刷新后选中的套餐/协议可能不复存在,重置为全部。 */
watch([protocols, respPlans], ([protos, plans]) => {
  if (selectedProtocol.value !== 'all' && !protos.includes(selectedProtocol.value)) {
    selectedProtocol.value = 'all'
  }
  if (selectedPlan.value !== 'all' && !plans.some((p) => p.code === selectedPlan.value)) {
    selectedPlan.value = 'all'
  }
})

const filteredPlans = computed(() => {
  let plans = respPlans.value
  if (selectedPlan.value !== 'all') {
    plans = plans.filter((p) => p.code === selectedPlan.value)
  }
  // 模型名/标识搜索 + 协议过滤:套餐内只留命中的模型与协议行,整套餐无命中则隐藏。
  const q = searchQuery.value.trim().toLowerCase()
  return plans
    .map((p) => {
      const models = p.models
        .map((m) => filterModel(m, q))
        .filter((m) => m.protocols.length > 0)
      return { ...p, models }
    })
    .filter((p) => p.models.length > 0)
})

/** 模型按搜索词裁剪展示名/标识,按所选协议裁剪协议行。 */
function filterModel(m: CatalogModel, q: string): CatalogModel {
  const hit = !q || m.display_name.toLowerCase().includes(q) || m.id.toLowerCase().includes(q)
  if (!hit) return { ...m, protocols: [] }
  if (selectedProtocol.value === 'all') return m
  return { ...m, protocols: m.protocols.filter((p) => p.protocol === selectedProtocol.value) }
}
</script>

<style scoped>
.plaza-description {
  line-height: 1.7;
  overflow-wrap: anywhere;
}

.plaza-description :deep(h1),
.plaza-description :deep(h2),
.plaza-description :deep(h3) {
  @apply mb-2 mt-3 font-semibold text-gray-900 first:mt-0 dark:text-white;
}

.plaza-description :deep(p) {
  @apply mb-2 text-gray-700 last:mb-0 dark:text-dark-200;
}

.plaza-description :deep(a) {
  @apply text-primary-600 underline underline-offset-4 hover:text-primary-700 dark:text-primary-300;
}

.plaza-description :deep(ul) {
  @apply mb-2 list-disc pl-5;
}

.plaza-description :deep(ol) {
  @apply mb-2 list-decimal pl-5;
}

.plaza-description :deep(li) {
  @apply mb-0.5 text-gray-700 dark:text-dark-200;
}

.plaza-description :deep(code) {
  @apply rounded bg-gray-100 px-1.5 py-0.5 font-mono text-xs dark:bg-dark-800;
}

.plaza-description :deep(blockquote) {
  @apply my-2 border-l-4 border-gray-300 pl-3 text-gray-600 dark:border-dark-600 dark:text-dark-300;
}
</style>