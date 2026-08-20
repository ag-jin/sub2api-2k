<template>
  <span v-if="summaryText" class="text-[11px] font-mono" :class="summaryClass">{{ summaryText }}</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account, AccountUsageInfo } from '@/types'

const props = defineProps<{
  account: Account
  batchedUsage: AccountUsageInfo | null
}>()

const { t } = useI18n()

const summary = computed(() => {
  const u = props.batchedUsage
  if (!u) return null

  if (u.error) return { text: t('admin.accounts.usageError'), class: 'text-red-500' }

  if (u.opencode) {
    const pct = u.opencode.rolling?.percent ?? u.opencode.weekly?.percent ?? u.opencode.monthly?.percent
    if (pct != null) {
      const cls = pct >= 80 ? 'text-red-500' : pct >= 50 ? 'text-amber-500' : 'text-emerald-500'
      return { text: `${pct.toFixed(0)}%`, class: cls }
    }
    const statusText = u.opencode.status === 'stale'
      ? t('admin.accounts.usageError')
      : u.opencode.status === 'error' || u.opencode.status === 'unavailable'
        ? t('admin.accounts.usageError')
        : u.opencode.status === 'rate-limited'
          ? t('admin.accounts.status.rateLimited')
          : u.opencode.status ?? '—'
    return { text: statusText, class: 'text-gray-400' }
  }

  if (u.five_hour?.utilization != null) {
    const pct = u.five_hour.utilization
    const cls = pct >= 80 ? 'text-red-500' : pct >= 50 ? 'text-amber-500' : 'text-emerald-500'
    return { text: `5h ${pct.toFixed(0)}%`, class: cls }
  }
  if (u.seven_day?.utilization != null) {
    const pct = u.seven_day.utilization
    const cls = pct >= 80 ? 'text-red-500' : pct >= 50 ? 'text-amber-500' : 'text-emerald-500'
    return { text: `7d ${pct.toFixed(0)}%`, class: cls }
  }

  if (u.grok_request_quota?.limit != null && u.grok_request_quota.limit > 0) {
    const remaining = u.grok_request_quota.remaining ?? 0
    const pct = ((u.grok_request_quota.limit - remaining) / u.grok_request_quota.limit) * 100
    const cls = pct >= 80 ? 'text-red-500' : pct >= 50 ? 'text-amber-500' : 'text-emerald-500'
    return { text: `${pct.toFixed(0)}%`, class: cls }
  }

  const gemini = u.gemini_shared_daily ?? u.gemini_pro_daily ?? u.gemini_flash_daily
  if (gemini?.utilization != null) {
    const pct = gemini.utilization
    const cls = pct >= 80 ? 'text-red-500' : pct >= 50 ? 'text-amber-500' : 'text-emerald-500'
    return { text: `${pct.toFixed(0)}%`, class: cls }
  }

  return null
})

const summaryText = computed(() => summary.value?.text ?? '')
const summaryClass = computed(() => summary.value?.class ?? '')
</script>
