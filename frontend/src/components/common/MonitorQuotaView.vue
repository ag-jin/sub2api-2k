<template>
  <div v-if="snapshot" class="space-y-1" data-testid="monitor-quota-view">
    <!-- 套餐等级徽章（如智谱 plan level / Claude 订阅档） -->
    <div v-if="snapshot.plan_level" class="flex flex-wrap items-center gap-1.5">
      <span class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-600 dark:text-gray-300">
        {{ snapshot.plan_level }}
      </span>
    </div>

    <!-- 用量窗口条形图（样式/阈值对齐账号页 CNProviderQuotaCell） -->
    <div v-if="snapshot.success && tierRows.length" class="space-y-1">
      <div v-for="row in tierRows" :key="row.key" class="flex items-center gap-1.5 text-[10px]">
        <span class="w-14 shrink-0 truncate text-gray-500 dark:text-gray-400" :title="row.title">
          {{ row.label }}
        </span>
        <div class="h-1.5 w-16 shrink-0 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600">
          <div
            class="h-full rounded-full transition-all"
            :class="utilizationColor(row.tier.used_percent)"
            :style="{ width: `${Math.min(100, Math.max(0, row.tier.used_percent))}%` }"
          />
        </div>
        <span :class="['shrink-0 font-medium', utilizationTextColor(row.tier.used_percent)]">
          {{ Math.round(row.tier.used_percent) }}%
        </span>
        <span v-if="row.tier.reset_at" class="truncate text-gray-400 dark:text-gray-500" :title="row.tier.reset_at">
          · {{ formatReset(row.tier.reset_at) }}
        </span>
      </div>
    </div>

    <!-- 余额（国产 payg；支持多币种） -->
    <div v-if="snapshot.success && balanceRows.length" class="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px]">
      <span
        v-for="b in balanceRows"
        :key="b.currency"
        :class="['font-medium', b.balance <= 0 ? 'text-red-600 dark:text-red-400' : 'text-gray-600 dark:text-gray-300']"
      >
        {{ b.balance.toFixed(2) }} {{ b.currency }}
      </span>
    </div>

    <div v-if="!snapshot.success" class="truncate text-[10px] text-red-600 dark:text-red-400" :title="snapshot.error" data-testid="monitor-quota-error">
      {{ truncatedError }}
    </div>

    <!-- 智谱登录托管账号：近 7 日逐模型积分明细（design M4 model_credits） -->
    <section
      v-if="creditsPanelVisible"
      class="rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900"
      data-testid="zhipu-model-credits-panel"
    >
      <div class="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-gray-100 px-4 py-3 dark:border-dark-700">
        <Icon name="chart" size="sm" class="h-4 w-4 shrink-0 text-gray-400 dark:text-dark-500" aria-hidden="true" />
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
          {{ t('monitorCommon.credits.title') }}
        </h4>
        <span class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-600 dark:text-gray-300">
          {{ t('monitorCommon.credits.range') }}
        </span>
        <span
          class="ml-auto text-xs text-gray-500 dark:text-gray-400"
          data-testid="zhipu-model-credits-updated"
          :title="snapshot.fetched_at"
        >
          {{ t('monitorCommon.credits.updatedAt', { time: fetchedAtLabel }) }}
        </span>
      </div>

      <DataTable
        v-if="creditRows.length && !creditsDetailMissing"
        :columns="creditColumns"
        :data="creditRows"
        row-key="key"
        :sticky-first-column="false"
        :sticky-actions-column="false"
        :expandable-actions="false"
        :estimate-row-height="40"
      >
        <template #cell-date="{ row }">
          <span
            class="truncate"
            data-testid="zhipu-model-credits-date"
            :title="creditDateTitle(row)"
          >
            {{ formatCreditDate(row.date) }}
          </span>
        </template>
        <template #cell-model="{ row }">
          <span
            class="block max-w-[280px] truncate"
            data-testid="zhipu-model-credits-model"
            :title="row.model || undefined"
          >
            {{ formatCreditModel(row.model) }}
          </span>
        </template>
        <template #cell-input_tokens="{ row }">
          <span
            class="font-mono"
            data-testid="zhipu-model-credits-input_tokens"
            :title="numericTitle(row.input_tokens)"
          >
            {{ formatCreditNumber(row.input_tokens) }}
          </span>
        </template>
        <template #cell-cached_tokens="{ row }">
          <span
            class="font-mono"
            data-testid="zhipu-model-credits-cached_tokens"
            :title="numericTitle(row.cached_tokens)"
          >
            {{ formatCreditNumber(row.cached_tokens) }}
          </span>
        </template>
        <template #cell-output_tokens="{ row }">
          <span
            class="font-mono"
            data-testid="zhipu-model-credits-output_tokens"
            :title="numericTitle(row.output_tokens)"
          >
            {{ formatCreditNumber(row.output_tokens) }}
          </span>
        </template>
        <template #cell-credits="{ row }">
          <span
            class="font-mono"
            :class="row.credits < 0 ? 'text-red-600 dark:text-red-400' : undefined"
            data-testid="zhipu-model-credits-credits"
            :title="numericTitle(row.credits)"
          >
            {{ formatCreditNumber(row.credits) }}
          </span>
        </template>
      </DataTable>

      <!-- 有行但四个明细字段全缺：字段不可用（不渲染只含 "-" 的空表） -->
      <div
        v-else-if="creditsDetailMissing"
        class="px-4 py-6"
        data-testid="zhipu-model-credits-unavailable"
      >
        <EmptyState :title="t('monitorCommon.credits.unavailable')">
          <template #icon>
            <Icon name="inbox" size="xl" class="h-10 w-10 text-gray-400 dark:text-dark-500" />
          </template>
        </EmptyState>
      </div>

      <!-- 空数组：无记录空态（无操作入口） -->
      <div v-else class="px-4 py-6" data-testid="zhipu-model-credits-empty">
        <EmptyState :title="t('monitorCommon.credits.empty')">
          <template #icon>
            <Icon name="inbox" size="xl" class="h-10 w-10 text-gray-400 dark:text-dark-500" />
          </template>
        </EmptyState>
      </div>

      <div
        v-if="creditsNotes.length"
        class="space-y-1 border-t border-gray-100 px-4 py-2 dark:border-dark-700"
      >
        <p
          v-for="note in creditsNotes"
          :key="note.testid"
          class="text-xs"
          :class="note.className"
          :data-testid="note.testid"
        >
          {{ note.text }}
        </p>
      </div>
    </section>

    <!--
      智谱登录托管账号：重置卡只读卡片（design M4 reset_cards / R0）。
      R0 硬性边界：本区块只有展示文本，不得出现使用/消耗/重置/续期等任何
      按钮、链接、菜单、开关或可点击样式（时钟图标仅为装饰）。
    -->
    <section
      v-if="resetPanelVisible"
      class="rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900"
      data-testid="zhipu-reset-cards-panel"
    >
      <div class="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-gray-100 px-4 py-3 dark:border-dark-700">
        <Icon name="clock" size="sm" class="h-4 w-4 shrink-0 text-gray-400 dark:text-dark-500" aria-hidden="true" />
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
          {{ t('monitorCommon.resetCards.title') }}
        </h4>
        <span
          class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-600 dark:text-gray-300"
          data-testid="zhipu-reset-cards-readonly"
        >
          {{ t('monitorCommon.resetCards.readOnly') }}
        </span>
      </div>

      <div class="p-4">
        <div
          v-if="resetCardsEmpty"
          data-testid="zhipu-reset-cards-empty"
        >
          <EmptyState :title="t('monitorCommon.resetCards.empty')">
            <template #icon>
              <Icon name="clock" size="xl" class="h-10 w-10 text-gray-400 dark:text-dark-500" />
            </template>
          </EmptyState>
        </div>

        <div v-else-if="resetCardsPresent" class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div
            v-for="card in resetCardViews"
            :key="card.type"
            class="min-w-0 rounded-lg bg-gray-50 p-3 dark:bg-dark-800"
            :data-testid="`zhipu-reset-card-${card.type}`"
            :data-state="card.state"
          >
            <span class="text-xs font-medium text-gray-500 dark:text-dark-400">
              {{ t(card.labelKey) }}
            </span>

            <!-- 该类型无卡：中性占位，不虚构 0 张 -->
            <p
              v-if="card.count === 0"
              class="mt-1 text-xs text-gray-500 dark:text-dark-400"
              data-testid="zhipu-reset-card-empty"
            >
              {{ t('monitorCommon.resetCards.typeEmpty') }}
            </p>

            <template v-else>
              <p
                class="mt-1 text-lg font-bold text-gray-900 dark:text-gray-100"
                data-testid="zhipu-reset-card-count"
              >
                {{ t('monitorCommon.resetCards.count', { count: card.count }) }}
              </p>

              <template v-if="card.state === 'unknown'">
                <p
                  class="mt-1 text-xs text-gray-500 dark:text-dark-400"
                  data-testid="zhipu-reset-card-expiry"
                >
                  {{ t('monitorCommon.resetCards.unknownExpiry') }}
                </p>
              </template>
              <template v-else>
                <p
                  class="mt-1 text-xs font-medium"
                  :class="card.stateTextClass"
                  data-testid="zhipu-reset-card-expiry"
                  :title="card.expireAt ?? undefined"
                >
                  {{ card.stateText }}
                </p>
                <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
                  {{ t('monitorCommon.resetCards.expiresAt', { time: card.absExpireAt }) }}
                </p>
              </template>
            </template>
          </div>
        </div>

        <!-- 面板级状态提示（按严重度排列；全部为文本，无任何操作入口） -->
        <div
          v-if="resetNotes.length"
          class="space-y-1"
          :class="
            resetCardsPresent || resetCardsEmpty
              ? 'mt-3 border-t border-gray-100 pt-2 dark:border-dark-700'
              : ''
          "
        >
          <p
            v-for="note in resetNotes"
            :key="note.testid"
            class="text-xs"
            :class="note.className"
            :data-testid="note.testid"
          >
            {{ note.text }}
          </p>
        </div>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import DataTable from '@/components/common/DataTable.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Column } from '@/components/common/types'
import { formatCompactNumber } from '@/utils/format'
import { getLocale } from '@/i18n'
import type { MonitorQuotaSnapshot, MonitorQuotaTier } from '@/api/admin/channelMonitor'

/**
 * 配额快照渲染（管理端监控列表/运行结果 + 用户端监控卡片共用）。
 * 展示形态对齐账号管理侧的用量视图（CNProviderQuotaCell：同阈值配色、
 * 同倒计时格式）；tier 的 Window/Label 是后端约定的机器 token，
 * 已知 token 走 i18n，未知 token 原样展示（前向兼容）。
 */
const props = defineProps<{
  snapshot?: MonitorQuotaSnapshot | null
}>()

const { t, te } = useI18n()

interface QuotaTierRow {
  key: string
  label: string
  title: string
  tier: MonitorQuotaTier
}

// 已知的 window/label 机器 token → i18n key（monitorCommon.quota.*）。
const windowI18nKeys: Record<string, string> = {
  '5h': 'monitorCommon.quota.windows.5h',
  '7d': 'monitorCommon.quota.windows.7d',
  '7d-sonnet': 'monitorCommon.quota.windows.7dSonnet',
  '7d-fable': 'monitorCommon.quota.windows.7dFable',
  weekly: 'monitorCommon.quota.windows.weekly',
  daily: 'monitorCommon.quota.windows.daily',
  '30d': 'monitorCommon.quota.windows.30d',
  total: 'monitorCommon.quota.windows.total',
}

const labelI18nKeys: Record<string, string> = {
  requests: 'monitorCommon.quota.labels.requests',
  tokens: 'monitorCommon.quota.labels.tokens',
  shared: 'monitorCommon.quota.labels.shared',
  pro: 'monitorCommon.quota.labels.pro',
  flash: 'monitorCommon.quota.labels.flash',
}

function windowLabel(window: string): string {
  const key = windowI18nKeys[window]
  return key && te(key) ? t(key) : window
}

function tierLabel(tier: MonitorQuotaTier): string {
  const window = windowLabel(tier.window)
  if (!tier.label) return window
  const labelKey = labelI18nKeys[tier.label]
  const label = labelKey && te(labelKey) ? t(labelKey) : tier.label
  return `${label}/${window}`
}

const tierRows = computed<QuotaTierRow[]>(() =>
  (props.snapshot?.tiers || []).map((tier, idx) => ({
    key: `${tier.window}-${tier.label || ''}-${idx}`,
    label: tierLabel(tier),
    title: tierLabel(tier),
    tier,
  })),
)

const balanceRows = computed(() => {
  const snapshot = props.snapshot
  if (!snapshot) return []
  if (snapshot.balances?.length) return snapshot.balances
  if (snapshot.balance != null) {
    return [{ currency: snapshot.currency || '?', balance: snapshot.balance }]
  }
  return []
})

const truncatedError = computed(() => {
  const error = props.snapshot?.error || t('monitorCommon.quota.unavailable')
  return error.length > 48 ? `${error.slice(0, 48)}…` : error
})

/** 缺失/非法字段的统一占位（不使用 0 冒充缺失值）。 */
const PLACEHOLDER = '-'

/**
 * 快照新鲜度窗口：与账号侧同类配额快照单元格（CNProviderQuotaCell.SNAPSHOT_STALE_MS）
 * 对齐——账号配额快照缓存 TTL 5 分钟、监控可配间隔上限 3600s，
 * 超过 15 分钟未刷新即视为陈旧，保留旧值并加 amber 提示。
 */
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

const snapshotStale = computed(() => {
  const raw = props.snapshot?.fetched_at
  if (!raw) return false
  const fetchedAt = Date.parse(raw)
  if (Number.isNaN(fetchedAt)) return false
  return Date.now() - fetchedAt > SNAPSHOT_STALE_MS
})

const fetchedAtLabel = computed(() => {
  const raw = props.snapshot?.fetched_at
  if (!raw) return PLACEHOLDER
  const date = new Date(raw)
  if (Number.isNaN(date.getTime())) return raw
  try {
    return new Intl.DateTimeFormat(getLocale(), {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    }).format(date)
  } catch {
    return raw
  }
})

function toFiniteNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

/** Tokens/积分：项目统一的 K/M 缩写；缺失或非法为 `-`（不用 0 冒充）。 */
function formatCreditNumber(value: unknown): string {
  const num = toFiniteNumber(value)
  return num === null ? PLACEHOLDER : formatCompactNumber(num)
}

/** 精确值放 `title`，缩写值只做展示。 */
function numericTitle(value: unknown): string | undefined {
  const num = toFiniteNumber(value)
  return num === null ? undefined : String(num)
}

function formatCreditModel(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : PLACEHOLDER
}

/** 解析失败时显示原始字段（不伪造日期），可解析时按用户本地化短日期渲染。 */
function parseCreditDate(value: unknown): { raw: string; date: Date | null } {
  const raw = typeof value === 'string' ? value.trim() : ''
  if (!raw) return { raw: '', date: null }
  const calendarDay = /^(\d{4})-(\d{2})-(\d{2})$/.exec(raw)
  if (calendarDay) {
    const year = Number(calendarDay[1])
    const month = Number(calendarDay[2])
    const day = Number(calendarDay[3])
    // 上游 date 是 Asia/Shanghai 日历日：按本地日历日构造，避免时区回退一天。
    // 回读校验：非法日历日（如 2026-13-45）会被 Date 静默进位，不能算解析成功。
    const candidate = new Date(year, month - 1, day)
    if (
      candidate.getFullYear() === year &&
      candidate.getMonth() === month - 1 &&
      candidate.getDate() === day
    ) {
      return { raw, date: candidate }
    }
  }
  const parsed = new Date(raw)
  return { raw, date: Number.isNaN(parsed.getTime()) ? null : parsed }
}

function formatCreditDate(value: unknown): string {
  const { raw, date } = parseCreditDate(value)
  if (!raw) return PLACEHOLDER
  if (!date) return raw
  try {
    return new Intl.DateTimeFormat(getLocale(), {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
    }).format(date)
  } catch {
    return raw
  }
}

function creditDateTitle(row: unknown): string | undefined {
  const raw = (row as { date?: unknown } | null)?.date
  if (typeof raw !== 'string') return undefined
  const trimmed = raw.trim()
  return trimmed || undefined
}

/** 排序键：积分缺失/非法时排最后（不被当成 0）。 */
function creditSortValue(value: unknown): number {
  const num = toFiniteNumber(value)
  return num === null ? Number.NEGATIVE_INFINITY : num
}

interface CreditRow {
  key: string
  date: string
  model: string
  input_tokens?: number
  cached_tokens?: number
  output_tokens?: number
  credits?: number
}

const creditsPanelVisible = computed(() => Array.isArray(props.snapshot?.model_credits))

const creditRows = computed<CreditRow[]>(() => {
  const list = props.snapshot?.model_credits
  if (!Array.isArray(list)) return []
  return list
    .map((item, index) => ({ item, index }))
    .sort((a, b) => {
      const dateA = parseCreditDate(a.item?.date).date?.getTime() ?? Number.NEGATIVE_INFINITY
      const dateB = parseCreditDate(b.item?.date).date?.getTime() ?? Number.NEGATIVE_INFINITY
      if (dateA !== dateB) return dateB - dateA
      const creditsA = creditSortValue(a.item?.credits)
      const creditsB = creditSortValue(b.item?.credits)
      if (creditsA !== creditsB) return creditsB - creditsA
      return a.index - b.index
    })
    .map(({ item, index }) => ({
      key: `${item?.date ?? ''}|${item?.model ?? ''}|${index}`,
      date: item?.date ?? '',
      model: item?.model ?? '',
      input_tokens: item?.input_tokens,
      cached_tokens: item?.cached_tokens,
      output_tokens: item?.output_tokens,
      credits: item?.credits,
    }))
})

const creditColumns = computed<Column[]>(() => [
  { key: 'date', label: t('monitorCommon.credits.columns.date'), class: 'text-xs' },
  { key: 'model', label: t('monitorCommon.credits.columns.model'), class: 'max-w-[280px]' },
  { key: 'input_tokens', label: t('monitorCommon.credits.columns.inputTokens'), class: 'text-right' },
  { key: 'cached_tokens', label: t('monitorCommon.credits.columns.cachedTokens'), class: 'text-right' },
  { key: 'output_tokens', label: t('monitorCommon.credits.columns.outputTokens'), class: 'text-right' },
  { key: 'credits', label: t('monitorCommon.credits.columns.credits'), class: 'text-right' },
])

/** 有明细行但四个数值字段全缺 → 「字段不可用」，而不是满屏 "-"。 */
const creditsDetailMissing = computed(
  () =>
    creditRows.value.length > 0 &&
    creditRows.value.every(
      (row) =>
        toFiniteNumber(row.input_tokens) === null &&
        toFiniteNumber(row.cached_tokens) === null &&
        toFiniteNumber(row.output_tokens) === null &&
        toFiniteNumber(row.credits) === null
    )
)

// --- 重置卡（只读展示，R0：永不提供使用入口） ---

/** 重置卡类型（后端契约：five_hour / week，design M4）。 */
const RESET_CARD_TYPES = ['five_hour', 'week'] as const
type ResetCardType = (typeof RESET_CARD_TYPES)[number]

/** 7 天内到期 = 注意档（design §5.3）；到期提醒细化属票 17。 */
const RESET_CARD_SOON_MS = 7 * 24 * 60 * 60 * 1000

type ResetCardState = 'ok' | 'expiring' | 'expired' | 'unknown'

interface ResetCardView {
  type: ResetCardType
  labelKey: string
  count: number
  state: ResetCardState
  expireAt: string | null
  absExpireAt: string
  stateText: string
  stateTextClass: string
}

const resetCards = computed(() => props.snapshot?.reset_cards ?? [])

const resetPanelVisible = computed(
  () => Array.isArray(props.snapshot?.reset_cards) || props.snapshot?.needs_relogin === true
)

const resetCardsEmpty = computed(
  () => Array.isArray(props.snapshot?.reset_cards) && resetCards.value.length === 0
)

/** 有 reset_cards 字段且有内容 → 渲染两张类型微卡；字段缺失时不虚构 0 张卡片。 */
const resetCardsPresent = computed(
  () => Array.isArray(props.snapshot?.reset_cards) && resetCards.value.length > 0
)

/** 绝对到期时间：本地化到分钟；解析失败原样展示（不伪造日期）。 */
function formatExpireAt(raw: string): string {
  const parsed = new Date(raw)
  if (Number.isNaN(parsed.getTime())) return raw
  try {
    return new Intl.DateTimeFormat(getLocale(), {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    }).format(parsed)
  } catch {
    return raw
  }
}

/** 相对到期：复用 common.time.countdown.* 文案（天/时/分三档）。 */
function formatExpireCountdown(diffMs: number): string {
  const totalMinutes = Math.max(1, Math.floor(diffMs / 60_000))
  const days = Math.floor(totalMinutes / 1440)
  const hours = Math.floor((totalMinutes % 1440) / 60)
  const minutes = totalMinutes % 60
  if (days > 0) return t('common.time.countdown.daysHours', { d: days, h: hours })
  if (hours > 0) return t('common.time.countdown.hoursMinutes', { h: hours, m: minutes })
  return t('common.time.countdown.minutes', { m: minutes })
}

function buildResetCardView(type: ResetCardType): ResetCardView {
  const items = resetCards.value.filter((card) => card?.type === type)
  // 「最近到期」取最早的一个（含已过期者），保证最严重状态优先暴露
  const withExpiry = items
    .map((card) => {
      const raw = typeof card?.expire_at === 'string' ? card.expire_at.trim() : ''
      return { raw, ts: raw ? Date.parse(raw) : Number.NaN }
    })
    .filter((entry) => entry.raw && !Number.isNaN(entry.ts))
    .sort((a, b) => a.ts - b.ts)

  const base = {
    type,
    labelKey: `monitorCommon.resetCards.types.${type}`,
    count: items.length,
  }

  const nearest = withExpiry[0]
  if (!nearest) {
    return {
      ...base,
      state: 'unknown',
      expireAt: null,
      absExpireAt: PLACEHOLDER,
      stateText: t('monitorCommon.resetCards.unknownExpiry'),
      stateTextClass: 'text-gray-500 dark:text-dark-400',
    }
  }

  const absExpireAt = formatExpireAt(nearest.raw)
  const remainingMs = nearest.ts - Date.now()
  if (remainingMs <= 0) {
    return {
      ...base,
      state: 'expired',
      expireAt: nearest.raw,
      absExpireAt,
      stateText: t('monitorCommon.resetCards.expired', { time: absExpireAt }),
      stateTextClass: 'text-red-600 dark:text-red-400',
    }
  }

  if (remainingMs <= RESET_CARD_SOON_MS) {
    return {
      ...base,
      state: 'expiring',
      expireAt: nearest.raw,
      absExpireAt,
      stateText: t('monitorCommon.resetCards.expiring', { time: absExpireAt }),
      stateTextClass: 'text-amber-600 dark:text-amber-400',
    }
  }

  return {
    ...base,
    state: 'ok',
    expireAt: nearest.raw,
    absExpireAt,
    stateText: t('monitorCommon.resetCards.expiresIn', {
      time: formatExpireCountdown(remainingMs),
    }),
    stateTextClass: 'text-emerald-600 dark:text-emerald-400',
  }
}

const resetCardViews = computed<ResetCardView[]>(() =>
  RESET_CARD_TYPES.map((type) => buildResetCardView(type))
)

interface PanelNote {
  testid: string
  text: string
  className: string
}

const fetchFailed = computed(() => props.snapshot?.success === false)

/** 积分面板状态提示：失败优先，其次陈旧；有旧数据时保留旧值只加提示。 */
const creditsNotes = computed<PanelNote[]>(() => {
  const notes: PanelNote[] = []
  if (fetchFailed.value) {
    notes.push({
      testid: 'zhipu-model-credits-failed',
      text: t('monitorCommon.credits.failed'),
      className: 'text-red-600 dark:text-red-400',
    })
  }
  if (snapshotStale.value) {
    notes.push({
      testid: 'zhipu-model-credits-stale',
      text: t('monitorCommon.credits.stale'),
      className: 'text-amber-600 dark:text-amber-400',
    })
  }
  return notes
})

/** 重置卡面板状态提示：按严重度排列（登录失效 > 已过期 > 即将到期 > 陈旧 > 读取失败）。 */
const resetNotes = computed<PanelNote[]>(() => {
  const notes: PanelNote[] = []
  if (props.snapshot?.needs_relogin === true) {
    notes.push({
      testid: 'zhipu-reset-cards-relogin',
      text: t('monitorCommon.resetCards.needsRelogin'),
      className: 'text-red-600 dark:text-red-400',
    })
  }
  const cards = resetCardViews.value.filter((card) => card.count > 0)
  const expired = cards.find((card) => card.state === 'expired')
  const expiring = cards.find((card) => card.state === 'expiring')
  if (expired) {
    notes.push({
      testid: 'zhipu-reset-cards-expired',
      text: expired.stateText,
      className: 'text-red-600 dark:text-red-400',
    })
  } else if (expiring) {
    notes.push({
      testid: 'zhipu-reset-cards-expiring',
      text: expiring.stateText,
      className: 'text-amber-600 dark:text-amber-400',
    })
  }
  if (snapshotStale.value) {
    notes.push({
      testid: 'zhipu-reset-cards-stale',
      text: t('monitorCommon.resetCards.stale'),
      className: 'text-amber-600 dark:text-amber-400',
    })
  }
  if (fetchFailed.value && Array.isArray(props.snapshot?.reset_cards)) {
    notes.push({
      testid: 'zhipu-reset-cards-failed',
      text: t('monitorCommon.resetCards.failed'),
      className: 'text-red-600 dark:text-red-400',
    })
  }
  return notes
})

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

// 重置时间相对/绝对简短显示（与账号页一致）。
const formatReset = (iso: string) => {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  const now = Date.now()
  const diffMs = d.getTime() - now
  if (diffMs <= 0) return t('monitorCommon.quota.resetSoon')
  if (diffMs < 3_600_000) return `${Math.max(1, Math.round(diffMs / 60_000))}m`
  const hours = Math.round(diffMs / 3_600_000)
  if (hours < 48) return `${hours}h`
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${mm}-${dd}`
}
</script>
