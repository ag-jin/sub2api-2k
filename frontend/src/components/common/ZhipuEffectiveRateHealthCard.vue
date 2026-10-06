<template>
  <section
    v-if="visible"
    class="rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900"
    data-testid="zhipu-sign-health-card"
    :data-state="state"
  >
    <div class="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-gray-100 px-4 py-3 dark:border-dark-700">
      <Icon name="shield" size="sm" class="h-4 w-4 shrink-0 text-gray-400 dark:text-dark-500" aria-hidden="true" />
      <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
        {{ t('monitorCommon.signHealth.title') }}
      </h4>
      <span
        class="rounded px-1.5 py-0.5 text-[10px] font-medium"
        :class="stateBadgeClass"
        data-testid="zhipu-sign-health-state"
      >
        {{ stateText }}
      </span>
      <!-- 陈旧标记与状态色并存：陈旧时保留原状态色，只追加 amber 提示，不把旧值重新涂成健康 -->
      <span
        v-if="stale"
        class="rounded bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
        data-testid="zhipu-sign-health-stale"
      >
        {{ t('monitorCommon.signHealth.stale') }}
      </span>
    </div>

    <div class="space-y-2 p-4">
      <p
        class="font-mono text-xl font-bold"
        :class="stateTextClass"
        :title="t('monitorCommon.signHealth.effectiveRateTooltip')"
        :aria-label="t('monitorCommon.signHealth.current')"
        data-testid="zhipu-sign-health-value"
      >
        {{ valueText }}
      </p>

      <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-gray-100 pt-2 text-xs dark:border-dark-700">
        <span class="text-gray-500 dark:text-dark-400">
          {{ t('monitorCommon.signHealth.target') }}
          <span
            class="font-mono font-medium text-gray-900 dark:text-gray-100"
            data-testid="zhipu-sign-health-target"
          >
            {{ targetText }}
          </span>
        </span>
        <span class="text-gray-500 dark:text-dark-400">
          {{ t('monitorCommon.signHealth.variance') }}
          <span
            class="font-mono font-medium text-gray-900 dark:text-gray-100"
            data-testid="zhipu-sign-health-variance"
          >
            {{ varianceText }}
          </span>
        </span>
      </div>

      <div class="space-y-1">
        <p
          class="text-xs text-gray-500 dark:text-dark-400"
          :title="t('monitorCommon.signHealth.peakFactorTooltip')"
          data-testid="zhipu-sign-health-peak-factor"
        >
          {{ peakFactorText }}
        </p>
        <p
          class="text-xs text-gray-500 dark:text-dark-400"
          :title="t('monitorCommon.signHealth.reconciledAtTooltip')"
          data-testid="zhipu-sign-health-reconciled-at"
        >
          {{ reconciledAtText }}
        </p>
        <!-- 告警原因摘要（纯文本，不带任何操作入口） -->
        <p
          v-if="alertReason"
          class="text-xs text-red-600 dark:text-red-400"
          data-testid="zhipu-sign-health-reason"
        >
          {{ alertReason }}
        </p>
      </div>

      <p
        v-if="failed"
        class="text-xs text-red-600 dark:text-red-400"
        data-testid="zhipu-sign-health-failed"
      >
        {{ t('monitorCommon.signHealth.failed') }}
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { getLocale } from '@/i18n'
import type { MonitorQuotaSnapshot } from '@/api/admin/channelMonitor'

/**
 * 智谱签名「有效系数」健康卡（design M5 / 票 27 的对账结果，ui-panels §6.1）。
 *
 * 数据来自配额快照的 sign_* 字段（后端 `MonitorQuotaSnapshot`，全部 omitempty）：
 * 字段缺失 = 未启用签名 / 未接线 / 从未成功对账 → 渲染「暂无对账数据」，
 * 绝不用 0 或 NaN 冒充缺失值（票 30 验收项 1）。
 *
 * 状态阈值（ui-panels §3.2/§6.1）：
 *   - `<= 0.67`        达到目标（emerald）
 *   - `0.68–0.70`      高于目标（amber，注意档，不产生 L2 告警）
 *   - `> 0.70`         系数告警（red，与 M5 内置规则一致）
 *   - 无有效样本       未知（gray）
 * `sign_reconcile_deviation` 是后端按生效阈值算出的判定，存在时优先于前端默认阈值：
 * 自定义阈值下不得让前端把「后端未判定偏离」的值渲染成告警。
 * `sign_reconcile_stale` 表示最近一轮对账失败保留了旧值：追加 amber 陈旧标记，
 * 但保留原状态色与最后成功值。
 *
 * 面板只在快照确实携带签名字段时渲染（老快照 / 非智谱账号不出现空壳），
 * 与 #16 的积分/重置卡面板同一可见性口径；无可展示值时失败态也不虚构指标。
 */
const props = defineProps<{
  snapshot?: MonitorQuotaSnapshot | null
}>()

const { t } = useI18n()

/** 目标系数（design M5，硬性口径 0.67）。 */
const TARGET_RATE = 0.67
/** 默认告警阈值（design M5 `> 0.70`；运行期可由 sign_reconcile_deviation_threshold 覆盖）。 */
const ALERT_THRESHOLD = 0.7

/** 缺失/非法字段的统一占位（不使用 0 冒充缺失值）。 */
const PLACEHOLDER = '-'

function toFiniteNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

type HealthState = 'on_target' | 'above_target' | 'alert' | 'unknown'

const effectiveRate = computed(() => toFiniteNumber(props.snapshot?.sign_effective_rate))

/**
 * 是否有任何签名字段：老快照与未启用签名的账号字段全缺，此时不渲染空壳面板。
 * （`false` 的布尔位在 omitempty 下不会出现，这里一并按「无数据」处理。）
 */
const visible = computed(() => {
  const snapshot = props.snapshot
  if (!snapshot) return false
  return (
    toFiniteNumber(snapshot.sign_effective_rate) !== null ||
    typeof snapshot.sign_reconciled_at === 'string' ||
    toFiniteNumber(snapshot.sign_peak_factor) !== null ||
    snapshot.sign_reconcile_stale === true ||
    snapshot.sign_reconcile_deviation === true
  )
})

const state = computed<HealthState>(() => {
  const rate = effectiveRate.value
  if (rate === null) return 'unknown'
  const deviation = props.snapshot?.sign_reconcile_deviation
  if (deviation === true) return 'alert'
  if (deviation === false) return rate <= TARGET_RATE ? 'on_target' : 'above_target'
  if (rate <= TARGET_RATE) return 'on_target'
  return rate <= ALERT_THRESHOLD ? 'above_target' : 'alert'
})

const stateKey = computed(() => {
  switch (state.value) {
    case 'on_target':
      return 'onTarget'
    case 'above_target':
      return 'aboveTarget'
    case 'alert':
      return 'alert'
    default:
      return 'unknown'
  }
})

const stateText = computed(() => t(`monitorCommon.signHealth.states.${stateKey.value}`))

const stateTextClass = computed(() => {
  switch (state.value) {
    case 'on_target':
      return 'text-emerald-600 dark:text-emerald-400'
    case 'above_target':
      return 'text-amber-600 dark:text-amber-400'
    case 'alert':
      return 'text-red-600 dark:text-red-400'
    default:
      return 'text-gray-500 dark:text-gray-400'
  }
})

const stateBadgeClass = computed(() => {
  switch (state.value) {
    case 'on_target':
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
    case 'above_target':
      return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
    case 'alert':
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    default:
      return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
  }
})

const valueText = computed(() =>
  effectiveRate.value === null ? PLACEHOLDER : effectiveRate.value.toFixed(2)
)

/** 无数据时偏差为占位符，不显示 -0.67 或 0.00 这类伪值。 */
const varianceText = computed(() => {
  const rate = effectiveRate.value
  if (rate === null) return PLACEHOLDER
  const diff = Math.round((rate - TARGET_RATE) * 100) / 100
  if (diff === 0) return '0.00'
  return `${diff > 0 ? '+' : '-'}${Math.abs(diff).toFixed(2)}`
})

const targetText = TARGET_RATE.toFixed(2)

const peakFactorText = computed(() => {
  const factor = toFiniteNumber(props.snapshot?.sign_peak_factor)
  return t('monitorCommon.signHealth.peakFactor', {
    factor: factor === null ? PLACEHOLDER : factor.toFixed(2),
  })
})

/** 对账时间：可解析按用户本地化渲染，非法原样透出（不伪造日期），缺失为占位符。 */
function formatReconciledAt(raw: unknown): string {
  if (typeof raw !== 'string' || !raw.trim()) return PLACEHOLDER
  const trimmed = raw.trim()
  const parsed = new Date(trimmed)
  if (Number.isNaN(parsed.getTime())) return trimmed
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
    return trimmed
  }
}

const reconciledAtText = computed(() =>
  t('monitorCommon.signHealth.reconciledAt', {
    time: formatReconciledAt(props.snapshot?.sign_reconciled_at),
  })
)

/** 只有「陈旧且确有旧值」才是数据过期；无值可陈旧时只说未知。 */
const stale = computed(
  () => props.snapshot?.sign_reconcile_stale === true && effectiveRate.value !== null
)

const failed = computed(() => props.snapshot?.success === false)

/** 告警原因摘要：标志位触发点名 sign_reconcile_deviation，否则说明默认阈值 0.70。 */
const alertReason = computed(() => {
  if (state.value !== 'alert') return ''
  if (props.snapshot?.sign_reconcile_deviation === true) {
    return t('monitorCommon.signHealth.reasonDeviationFlag')
  }
  return t('monitorCommon.signHealth.reasonAboveAlertThreshold', {
    threshold: ALERT_THRESHOLD.toFixed(2),
  })
})
</script>
