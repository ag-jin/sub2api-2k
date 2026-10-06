<template>
  <section class="overflow-hidden rounded-2xl border border-gray-100 bg-white shadow-card dark:border-dark-700/50 dark:bg-dark-800/50">
    <!-- 套餐头部:代号徽章 + 名称 + 描述 -->
    <header class="border-b border-gray-100 px-5 py-4 dark:border-dark-700/60">
      <div class="flex flex-wrap items-center gap-2">
        <span
          class="inline-flex items-center rounded-md bg-primary-50 px-2 py-0.5 font-mono text-xs font-medium text-primary-700 dark:bg-primary-900/20 dark:text-primary-300"
        >
          {{ plan.code }}
        </span>
        <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ plan.name }}</h2>
      </div>
      <p v-if="plan.description" class="mt-2 text-sm text-gray-500 dark:text-dark-400">
        {{ plan.description }}
      </p>
    </header>

    <!-- 模型价格表:整行(含 hover 底色)顶到卡片边缘,左右留白由表格首列/末列的 padding 提供 -->
    <div>
      <PlazaModelPricingTable
        v-if="plan.models.length > 0"
        :models="plan.models"
      />
      <p v-else class="px-5 py-4 text-center text-sm text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.detail.noModels') }}
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import PlazaModelPricingTable from './PlazaModelPricingTable.vue'
import type { PricingPlan } from '@/api/modelPlaza'

defineProps<{
  plan: PricingPlan
}>()

const { t } = useI18n()
</script>