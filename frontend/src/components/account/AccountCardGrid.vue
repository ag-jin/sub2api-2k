<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-y-auto">
    <div
      v-if="loading && accounts.length === 0"
      class="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4"
    >
      <div
        v-for="i in 8"
        :key="i"
        class="card animate-pulse p-4"
      >
        <div class="flex items-center gap-2">
          <div class="h-4 w-4 rounded bg-gray-200 dark:bg-dark-700"></div>
          <div class="h-5 w-5 rounded bg-gray-200 dark:bg-dark-700"></div>
          <div class="h-4 flex-1 rounded bg-gray-200 dark:bg-dark-700"></div>
          <div class="h-5 w-5 rounded bg-gray-200 dark:bg-dark-700"></div>
        </div>
        <div class="mt-3 h-6 w-3/4 rounded bg-gray-200 dark:bg-dark-700"></div>
        <div class="mt-3 space-y-2">
          <div class="h-3 w-full rounded bg-gray-200 dark:bg-dark-700"></div>
          <div class="h-3 w-2/3 rounded bg-gray-200 dark:bg-dark-700"></div>
          <div class="h-3 w-5/6 rounded bg-gray-200 dark:bg-dark-700"></div>
        </div>
      </div>
    </div>
    <div
      v-else-if="accounts.length === 0"
      class="flex flex-1 flex-col items-center justify-center gap-2 py-20 text-gray-400 dark:text-dark-500"
    >
      <svg class="h-12 w-12" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="1">
        <path stroke-linecap="round" stroke-linejoin="round" d="M20 7l-8-4-8 4m16 0l-8 4m8-4v10l-8 4m0-10L4 7m8 4v10M4 7v10l8 4" />
      </svg>
      <span class="text-sm">{{ t('admin.accounts.noAccountsYet') }}</span>
    </div>
    <div
      v-else
      class="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4"
    >
      <AccountCard
        v-for="account in accounts"
        :key="account.id"
        :account="account"
        :selected="selectedIds.has(account.id)"
        :card-fields="cardFields"
        :today-stats="todayStatsByAccountId[String(account.id)] ?? null"
        :today-stats-loading="todayStatsLoading"
        :manual-refresh-token="manualRefreshToken"
        :batched-usage="usageBatchByAccountId[String(account.id)] ?? null"
        :batched-usage-error="usageBatchErrorByAccountId[String(account.id)] ?? null"
        :batched-usage-loading="usageBatchLoadingByAccountId[String(account.id)] === true"
        :request-batched-usage="requestBatchedUsage"
        @toggle-select="$emit('toggle-select', $event)"
        @edit="$emit('edit', $event)"
        @toggle-schedulable="$emit('toggle-schedulable', $event)"
        @show-actions="(acc, ev) => $emit('show-actions', acc, ev)"
        @account-updated="$emit('account-updated', $event)"
        @usage-loaded="$emit('usage-loaded', account.id, $event)"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { Account, AccountUsageInfo, WindowStats } from '@/types'
import AccountCard from './AccountCard.vue'

const { t } = useI18n()

defineProps<{
  accounts: Account[]
  selectedIds: Set<number>
  cardFields: Set<string>
  loading: boolean
  todayStatsByAccountId: Record<string, WindowStats>
  todayStatsLoading: boolean
  manualRefreshToken: number
  usageBatchByAccountId: Record<string, AccountUsageInfo | null>
  usageBatchErrorByAccountId: Record<string, string | null>
  usageBatchLoadingByAccountId: Record<string, boolean>
  requestBatchedUsage: ((account: Account, options?: { force?: boolean }) => void) | null
}>()

defineEmits<{
  'toggle-select': [id: number]
  'edit': [account: Account]
  'toggle-schedulable': [account: Account]
  'show-actions': [account: Account, event: MouseEvent]
  'account-updated': [account: Account]
  'usage-loaded': [accountId: number, usage: AccountUsageInfo]
}>()
</script>
