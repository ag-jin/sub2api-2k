<template>
  <div
    v-if="visible"
    data-test="cn-provider-quota"
    class="min-w-[220px] space-y-1"
  >
    <!-- Tier rows: 5h + weekly utilization bars (snapshot renders on mount) -->
    <div v-if="data?.success && data.tiers?.length" class="space-y-1">
      <div
        v-for="tier in data.tiers"
        :key="tier.window"
        data-test="cn-provider-quota-tier"
        class="flex min-w-0 items-center gap-1.5 text-[10px] leading-4"
      >
        <span
          data-test="cn-provider-quota-label"
          class="w-14 shrink-0 whitespace-nowrap text-gray-500 dark:text-gray-400"
        >
          {{ windowLabel(tier.window) }}
        </span>
        <div class="h-1.5 w-16 shrink-0 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600">
          <div
            class="h-full rounded-full transition-all"
            :class="utilizationColor(tier.used_percent)"
            :style="{ width: `${Math.min(100, Math.max(0, tier.used_percent))}%` }"
          />
        </div>
        <span :class="['shrink-0 font-medium', utilizationTextColor(tier.used_percent)]">
          {{ Math.round(tier.used_percent) }}%
        </span>
        <span
          v-if="tier.reset_at"
          class="min-w-0 truncate text-gray-400 dark:text-gray-500"
          :title="tier.reset_at"
        >
          · {{ formatReset(tier.reset_at) }}
        </span>
      </div>
    </div>

    <!-- Explicit refresh action (aligned with the OpenAI "Query" / Grok "Probe"
         buttons): a verb label tells users this chip is clickable. The previous
         noun label ("5h/weekly") read as a passive caption and users could not
         discover the manual refresh. -->
    <div class="flex flex-wrap items-center gap-1.5">
      <!-- 登录失效徽标（design M6 / ui-panels §6.3）：只提示「需重新登录」，
           不暴露凭据细节，也不触发任何操作（重登入口在账号编辑弹窗）。 -->
      <span
        v-if="needsRelogin"
        data-test="cn-provider-quota-needs-relogin"
        class="inline-flex items-center gap-1 rounded bg-red-100 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-red-700 dark:bg-red-900/40 dark:text-red-300"
        :title="t('admin.accounts.cnProviders.zhipuLogin.needsReloginTooltip')"
      >
        <Icon name="exclamationTriangle" size="sm" :stroke-width="2" />
        {{ t('admin.accounts.cnProviders.zhipuLogin.needsRelogin') }}
      </span>
      <!-- 签名降级/熔断徽标（design M6 / ui-panels §6.3，票 24/30）：账号级熔断摘除
           签名生效位时出现；纯展示，不伪装成可点击（无对应告警详情链接）。 -->
      <span
        v-if="signDegraded"
        data-test="cn-provider-quota-sign-degraded"
        class="inline-flex items-center gap-1 rounded bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300"
        :title="signDegradedTooltip"
      >
        <Icon name="exclamationTriangle" size="sm" :stroke-width="2" />
        {{ t('admin.accounts.cnProviders.zhipuSign.degraded') }}
      </span>
      <button
        type="button"
        data-test="cn-provider-quota-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.cnProviders.probeTooltip')"
        @click="handleProbe()"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.cnProviders.probe') }}
      </button>
    </div>

    <div
      v-if="error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { ZhipuSignAccountStatus } from '@/api/admin/zhipu'
import type { CNProviderQuotaProbeResult } from '@/api/admin/cnProviders'
import type { Account } from '@/types'
import Icon from '@/components/icons/Icon.vue'
import { cnQuotaCellVisible } from './credentialsBuilder'
import { loadZhipuSignStatus, resolveZhipuSignDegraded } from '@/composables/useZhipuSignStatus'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const readMode = (): string => {
  const mode = props.account.credentials?.account_mode
  return typeof mode === 'string' ? mode : ''
}

const visible = computed(() => cnQuotaCellVisible(props.account.platform, readMode()))

// 运行态标记：keeper 探针 401/自愈失败写 extra["zhipu_needs_relogin"]（design M2/M3），
// 重登成功后由重登链路清除。
const needsRelogin = computed(
  () => (props.account.extra as Record<string, unknown> | undefined)?.['zhipu_needs_relogin'] === true
)

const loading = ref(false)
const error = ref<string | null>(null)
const data = ref<CNProviderQuotaProbeResult | null>(null)

/**
 * 签名降级/熔断徽标的运行态来源（票 24 的账号级熔断，经票 28 的状态接口读取）。
 *
 * 状态读取走 `useZhipuSignStatus` 的模块级缓存：账号列表每行一个单元格，
 * 缓存把一次列表渲染收敛成最多一次请求；读取失败静默降级为「无徽标」，
 * 不影响既有配额单元格渲染。
 */
const signDegraded = ref<ZhipuSignAccountStatus | null>(null)

/** 签名只作用于智谱登录托管的 coding 账号（与单元格可见性同口径）。 */
const isZhipuSigningAccount = computed(
  () => props.account.platform === 'zhipu' && readMode() === 'coding'
)

const signDegradedTooltip = computed(() => {
  const base = t('admin.accounts.cnProviders.zhipuSign.degradedTooltip')
  const reason = signDegraded.value?.circuit_break_reason?.trim()
  return reason ? `${base} · ${reason}` : base
})

/** 读取一次该账号的熔断状态：未接线 / 查不到 / 未熔断都保持无徽标（不虚构状态）。 */
const refreshSignDegraded = async () => {
  signDegraded.value = null
  if (!isZhipuSigningAccount.value) return
  const accountId = props.account.id
  const status = await loadZhipuSignStatus()
  // 账号在等待期间被切换（列表复用行）时丢弃过期结果
  if (accountId !== props.account.id) return
  signDegraded.value = resolveZhipuSignDegraded(status, accountId)
}

// 后端周期任务/手动探测写入的 extra 快照键（<provider>_ 前缀，与后端
// cnQuotaExtraUpdates 对齐）。页面加载即有数据，无需等待探测。
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

// 自动探测去抖窗口与最近一次自动探测时间（模块级，跨实例共享）。
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const lastAutoProbeAt = new Map<number, number>()

const readExtraNumber = (key: string): number | null => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

const readExtraString = (key: string): string => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'string' ? v : ''
}

// 从持久化快照构造展示数据（缺少 5h/weekly 两档键时返回 null）。
const snapshotData = computed<CNProviderQuotaProbeResult | null>(() => {
  const platform = props.account.platform
  const used5h = readExtraNumber(`${platform}_5h_used_percent`)
  const usedWeekly = readExtraNumber(`${platform}_weekly_used_percent`)
  if (used5h == null && usedWeekly == null) return null
  const tiers: CNProviderQuotaProbeResult['tiers'] = []
  if (used5h != null) {
    tiers.push({ window: '5h', used_percent: used5h, reset_at: readExtraString(`${platform}_5h_reset_at`) || undefined })
  }
  if (usedWeekly != null) {
    tiers.push({ window: 'weekly', used_percent: usedWeekly, reset_at: readExtraString(`${platform}_weekly_reset_at`) || undefined })
  }
  return { success: true, tiers } as CNProviderQuotaProbeResult
})

// 快照是否过期（无更新时间或超过 staleness 窗口）→ 挂载时需要自动探测。
const snapshotIsStale = computed(() => {
  const updatedAt = readExtraString(`${props.account.platform}_usage_updated_at`)
  if (!updatedAt) return true
  const ts = new Date(updatedAt).getTime()
  return Number.isNaN(ts) || Date.now() - ts > SNAPSHOT_STALE_MS
})

// 挂载时：先用持久化快照渲染；快照缺失或过期再自动探测一次（失败显示错误，
// 避免静默失败导致单元格空白无提示）。
onMounted(() => {
  if (!visible.value) return
  data.value = snapshotData.value
  void refreshSignDegraded()
  if (!snapshotIsStale.value) return
  // 模块级去抖：列表页每行一个实例，翻页/筛选/刷新会重复挂载；同一账号
  // 短时间内已自动探测过则跳过，避免对上游形成探测风暴。
  const last = lastAutoProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastAutoProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

const windowLabel = (window: string) =>
  window === 'weekly'
    ? t('admin.accounts.cnProviders.windowWeekly')
    : t('admin.accounts.cnProviders.window5h')

const utilizationColor = (pct: number) => {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 75) return 'bg-amber-500'
  return 'bg-emerald-500'
}

const utilizationTextColor = (pct: number) => {
  if (pct >= 90) return 'text-red-600 dark:text-red-400'
  if (pct >= 75) return 'text-amber-600 dark:text-amber-400'
  return 'text-emerald-600 dark:text-emerald-400'
}

// 重置时间相对/绝对简短显示。
const formatReset = (iso: string) => {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  const now = Date.now()
  const diffMs = d.getTime() - now
  if (diffMs <= 0) return t('admin.accounts.cnProviders.resetSoon')
  if (diffMs < 3_600_000) return `${Math.max(1, Math.round(diffMs / 60_000))}m`
  const hours = Math.round(diffMs / 3_600_000)
  if (hours < 48) return `${hours}h`
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${mm}-${dd}`
}

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await adminAPI.cnProviders.queryQuota(props.account.id)
    // 失败时保留已渲染的快照条形图（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
    void refreshSignDegraded()
  }
)
</script>
