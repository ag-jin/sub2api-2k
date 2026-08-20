<template>
  <article
    class="card card-hover group flex h-full min-w-0 flex-col overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm dark:border-dark-700 dark:bg-dark-800"
  >
    <div class="flex items-center gap-1 border-b border-gray-100 px-4 py-2 dark:border-dark-700" @click.stop>
      <input
        type="checkbox"
        :checked="selected"
        class="h-4 w-4 flex-shrink-0 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
        @click.stop="$emit('toggle-select', account.id)"
      />
      <div class="flex min-w-0 flex-1 cursor-pointer items-center gap-2.5" @click.stop="$emit('edit', account)">
        <PlatformIcon :platform="account.platform" size="sm" class="flex-shrink-0" />
        <div class="min-w-0 flex-1">
          <div class="flex min-w-0 items-baseline gap-1.5">
            <span class="truncate text-[15px] font-semibold text-gray-900 dark:text-white">{{ account.name }}</span>
            <span class="flex-shrink-0 font-mono text-[10px] text-gray-400 dark:text-dark-500">#{{ account.id }}</span>
          </div>
          <span v-if="accountDisplayEmail" class="block truncate text-[11px] text-gray-400 dark:text-dark-400">{{ accountDisplayEmail }}</span>
        </div>
      </div>
      <div class="flex max-w-[55%] flex-shrink-0 items-center gap-2 overflow-hidden">
        <PlatformTypeBadge
            :platform="account.platform"
            :type="account.type"
            :auth-mode="String(account.extra?.openai_auth_mode ?? '')"
            :plan-type="accountPlanType"
            :privacy-mode="(account.extra?.privacy_mode as string) || (account.parent_privacy_mode ?? undefined)"
            :subscription-expires-at="(account.credentials?.subscription_expires_at as string) || (account.parent_subscription_expires_at ?? undefined)"
          />
          <AccountStatusIndicator :account="account" @show-temp-unsched="$emit('edit', account)" />
          <button
            type="button"
            class="flex-shrink-0 rounded-md p-1 text-gray-400 hover:bg-gray-100 hover:text-gray-800 dark:hover:bg-dark-700 dark:hover:text-white"
            :title="t('common.more')"
            @click.stop="$emit('show-actions', account, $event)"
          >
            <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2"><path stroke-linecap="round" stroke-linejoin="round" d="M6.75 12a.75.75 0 11-1.5 0 .75.75 0 011.5 0zM12.75 12a.75.75 0 11-1.5 0 .75.75 0 011.5 0zM18.75 12a.75.75 0 11-1.5 0 .75 0 011.5 0z" /></svg>
          </button>
        </div>
    </div>

    <section class="grid grid-cols-2 gap-px border-b border-gray-100 bg-gray-100 dark:border-dark-700 dark:bg-dark-700">
      <div class="min-w-0 bg-white px-3 py-2.5 dark:bg-dark-800">
        <div class="text-[10px] font-medium uppercase tracking-wide text-gray-400 dark:text-dark-400">{{ t('admin.accounts.columns.capacity') }}</div>
        <AccountCapacityCell :account="account" />
      </div>
      <div class="min-w-0 bg-white px-3 py-2.5 dark:bg-dark-800">
        <div class="text-[10px] font-medium uppercase tracking-wide text-gray-400 dark:text-dark-400">{{ t('admin.accounts.columns.todayStats') }}</div>
        <div v-if="todayStatsLoading && !todayStats" class="text-xs text-gray-400">...</div>
        <div v-else-if="todayStats" class="space-y-0.5 text-xs leading-4">
          <div class="flex items-center justify-between gap-1"><span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.stats.requests') }}</span><strong class="text-gray-800 dark:text-gray-200">{{ formatNumber(todayStats.requests) }}</strong></div>
          <div class="flex items-center justify-between gap-1"><span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.stats.tokens') }}</span><strong class="text-gray-800 dark:text-gray-200">{{ formatTokens(todayStats.tokens) }}</strong></div>
        </div>
        <span v-else class="text-xs text-gray-400">-</span>
      </div>
    </section>

    <section v-if="account.groups?.length" class="flex min-w-0 items-center gap-1.5 border-b border-gray-100 px-4 py-2 dark:border-dark-700" @click.stop>
      <span class="text-[10px] font-medium uppercase tracking-wide text-gray-400 dark:text-dark-400">{{ t('admin.accounts.columns.groups') }}</span>
      <div class="flex min-w-0 flex-wrap gap-1">
        <span v-for="group in account.groups.slice(0, 4)" :key="group.id" class="max-w-full truncate rounded-md bg-gray-100 px-1.5 py-0.5 text-[11px] text-gray-700 dark:bg-dark-600 dark:text-gray-200" :title="group.name">{{ group.name }}</span>
        <span v-if="account.groups.length > 4" class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[11px] text-gray-400 dark:bg-dark-600 dark:text-gray-300">+{{ account.groups.length - 4 }}</span>
      </div>
    </section>

    <section class="min-h-[120px] border-b border-gray-100 px-4 py-3 dark:border-dark-700" @click.stop>
      <div class="mb-2 flex items-center justify-between gap-3">
        <span class="text-sm font-semibold text-gray-700 dark:text-gray-200">{{ t('admin.accounts.columns.usageWindows') }}</span>
        <UsageSummary :account="account" :batched-usage="batchedUsage ?? null" />
      </div>
        <!-- API Key accounts with upstream balance: 3-column grid -->
        <div v-if="batchedUsage?.upstream_balance" class="space-y-2">
          <div v-if="batchedUsageLoading" class="text-xs text-gray-400">...</div>
          <template v-else>
            <div class="grid grid-cols-3 gap-2">
              <div class="text-center">
                <div class="text-[10px] text-gray-400 dark:text-gray-500">余额</div>
                <div v-if="batchedUsage.upstream_balance.balance != null" class="text-sm font-semibold" :class="batchedUsage.upstream_balance.balance > 0 ? 'text-emerald-600 dark:text-emerald-400' : 'text-gray-400'">
                  ${{ batchedUsage.upstream_balance.balance.toFixed(2) }}
                </div>
                <div v-else class="text-sm text-gray-400">-</div>
              </div>
              <div class="text-center">
                <div class="text-[10px] text-gray-400 dark:text-gray-500">{{ t('admin.accounts.stats.requests') }}</div>
                <div v-if="batchedUsage.upstream_balance.today" class="text-sm font-semibold text-gray-800 dark:text-gray-200">{{ formatNumber(batchedUsage.upstream_balance.today.requests) }}</div>
                <div v-else class="text-sm text-gray-400">-</div>
              </div>
              <div class="text-center">
                <div class="text-[10px] text-gray-400 dark:text-gray-500">{{ t('admin.accounts.stats.tokens') }}</div>
                <div v-if="batchedUsage.upstream_balance.today" class="text-sm font-semibold text-gray-800 dark:text-gray-200">{{ formatTokens(batchedUsage.upstream_balance.today.tokens) }}</div>
                <div v-else class="text-sm text-gray-400">-</div>
              </div>
            </div>
            <div v-if="batchedUsage.upstream_balance.stale" class="text-[10px] text-amber-600 dark:text-amber-400">{{ t('admin.accounts.usageError') }}</div>
          </template>
        </div>
        <!-- Other accounts: full AccountUsageCell -->
        <div v-show="!batchedUsage?.upstream_balance">
          <AccountUsageCell
            :account="account"
            :card-mode="true"
            :today-stats="todayStats ?? null"
            :today-stats-loading="todayStatsLoading ?? false"
            :manual-refresh-token="manualRefreshToken ?? 0"
            :batched-usage="batchedUsage ?? null"
            :batched-usage-error="batchedUsageError ?? null"
            :batched-usage-loading="batchedUsageLoading ?? false"
            :request-batched-usage="requestBatchedUsage ?? null"
            @account-updated="$emit('account-updated', $event)"
            @usage-loaded="$emit('usage-loaded', $event)"
          />
        </div>
        <!-- Hidden: still mount AccountUsageCell for API Key accounts to trigger batched fetch -->
        <div v-if="batchedUsage?.upstream_balance" class="hidden">
          <AccountUsageCell
            :account="account"
            :card-mode="true"
            :today-stats="todayStats ?? null"
            :today-stats-loading="todayStatsLoading ?? false"
            :manual-refresh-token="manualRefreshToken ?? 0"
            :batched-usage="batchedUsage ?? null"
            :batched-usage-error="batchedUsageError ?? null"
            :batched-usage-loading="batchedUsageLoading ?? false"
            :request-batched-usage="requestBatchedUsage ?? null"
            @account-updated="$emit('account-updated', $event)"
            @usage-loaded="$emit('usage-loaded', $event)"
          />
        </div>
    </section>

    <section v-if="hasOptionalDetails" class="grid grid-cols-2 gap-x-4 gap-y-2 border-b border-gray-100 px-4 py-3 text-xs dark:border-dark-700" @click.stop>
      <div v-if="cardFields.has('proxy')" class="min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.proxy') }}</span><div class="truncate text-gray-700 dark:text-gray-300">{{ account.proxy?.name || '-' }}</div></div>
      <div v-if="cardFields.has('rate_multiplier')" class="min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.billingRateMultiplier') }}</span><div class="font-mono text-gray-700 dark:text-gray-300">{{ formatMultiplier(account.rate_multiplier ?? 1) }}x</div></div>
      <div v-if="cardFields.has('expires_at')" class="min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.expiresAt') }}</span><div class="truncate text-gray-700 dark:text-gray-300">{{ account.expires_at ? formatExpiresAt(account.expires_at) : '-' }}</div></div>
      <div v-if="cardFields.has('created_at')" class="min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.createdAt') }}</span><div class="truncate text-gray-700 dark:text-gray-300">{{ formatDateTime(account.created_at) }}</div></div>
      <div v-if="cardFields.has('scheduler_score') && getSchedulerScoreRows().length" class="min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.schedulerScore') }}</span><div class="truncate font-mono text-gray-700 dark:text-gray-300">{{ formatSchedulerScoreGroup(getSchedulerScoreRows()[0]) }}: {{ getSchedulerScoreRows()[0].base_score }}</div></div>
      <div v-if="cardFields.has('upstream_billing_rate')" class="min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.upstreamBillingRate') }}</span><UpstreamBillingRateCell :account="account" :global-probe-enabled="false" :now="Date.now()" :probing="false" /></div>
      <div v-if="cardFields.has('notes') && account.notes" class="col-span-2 min-w-0"><span class="text-gray-400">{{ t('admin.accounts.columns.notes') }}</span><div class="truncate text-gray-700 dark:text-gray-300" :title="account.notes">{{ account.notes }}</div></div>
    </section>

    <footer class="mt-auto flex items-center justify-between gap-2 bg-gray-50/70 px-4 py-2.5 dark:bg-dark-800/60" @click.stop>
      <div class="flex min-w-0 flex-1 items-center gap-2">
        <button
          class="relative inline-flex h-5 w-9 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none"
          :class="account.schedulable ? 'bg-primary-500 hover:bg-primary-600' : 'bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500'"
          :title="account.schedulable ? t('admin.accounts.schedulableEnabled') : t('admin.accounts.schedulableDisabled')"
          @click.stop="$emit('toggle-schedulable', account)"
        >
          <span class="pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out" :class="[account.schedulable ? 'translate-x-4' : 'translate-x-0']" />
        </button>
        <span class="truncate text-[11px] text-gray-500 dark:text-dark-400">{{ t('admin.accounts.columns.lastUsed') }}: {{ account.last_used_at ? formatRelativeTime(account.last_used_at) : '-' }}</span>
      </div>
      <button type="button" class="flex flex-shrink-0 items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-gray-500 hover:bg-white hover:text-primary-600 dark:hover:bg-dark-700 dark:hover:text-primary-400" :title="t('common.edit')" @click.stop="$emit('edit', account)">
        <svg class="h-3.5 w-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M16.862 4.487l1.687-1.688a1.875 1.875 0 112.652 2.652L10.582 16.07a4.5 4.5 0 01-1.897 1.13L6 18l.8-2.685a4.5 4.5 0 011.13-1.897l8.932-8.931zm0 0L19.5 7.125M18 14v4.75A2.25 2.25 0 0115.75 21H5.25A2.25 2.25 0 013 18.75V8.25A2.25 2.25 0 015.25 6H10" /></svg>
        {{ t('common.edit') }}
      </button>
    </footer>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account, AccountSchedulerGroupScore, AccountUsageInfo, WindowStats } from '@/types'
import { formatDateTime, formatNumber, formatRelativeTime } from '@/utils/format'
import { formatMultiplier } from '@/utils/formatters'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
import AccountStatusIndicator from '@/components/account/AccountStatusIndicator.vue'
import AccountCapacityCell from '@/components/account/AccountCapacityCell.vue'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import UpstreamBillingRateCell from '@/components/account/UpstreamBillingRateCell.vue'
import UsageSummary from './UsageSummary.vue'

const { t } = useI18n()
const props = defineProps<{
  account: Account
  selected: boolean
  cardFields: Set<string>
  todayStats?: WindowStats | null
  todayStatsLoading?: boolean
  manualRefreshToken?: number
  batchedUsage?: AccountUsageInfo | null
  batchedUsageError?: string | null
  batchedUsageLoading?: boolean
  requestBatchedUsage?: ((account: Account, options?: { force?: boolean }) => void) | null
}>()
defineEmits<{
  'toggle-select': [id: number]
  'edit': [account: Account]
  'toggle-schedulable': [account: Account]
  'show-actions': [account: Account, event: MouseEvent]
  'account-updated': [account: Account]
  'usage-loaded': [usage: AccountUsageInfo]
}>()
const accountDisplayEmail = computed(() => props.account.parent_email || (props.account.credentials?.email as string) || '')
const accountPlanType = computed(() => (props.account.extra?.plan_type as string) || props.account.parent_plan_type || undefined)
const hasOptionalDetails = computed(() => [...props.cardFields].some((field) => {
  if (field === 'notes') return Boolean(props.account.notes)
  if (field === 'scheduler_score') return getSchedulerScoreRows().length > 0
  return true
}))
const getSchedulerScoreRows = (): AccountSchedulerGroupScore[] => {
  if (props.account.scheduler_scores?.length) return props.account.scheduler_scores
  if (props.account.scheduler_score) return [{ group_id: null, ...props.account.scheduler_score }]
  return []
}
const formatSchedulerScoreGroup = (score: AccountSchedulerGroupScore) => score.group_name || (score.group_id != null ? `#${score.group_id}` : t('admin.accounts.schedulerScore.ungrouped'))
const formatTokens = (tokens: number) => tokens >= 1_000_000 ? `${(tokens / 1_000_000).toFixed(1)}M` : tokens >= 1_000 ? `${(tokens / 1_000).toFixed(1)}K` : String(tokens)
const formatExpiresAt = (ts: number) => formatDateTime(new Date(ts * 1000).toISOString())
</script>
