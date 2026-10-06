<template>
  <div
    v-if="presentation"
    class="mt-1 space-y-0.5"
    data-testid="zhipu-sign-alert-detail"
    :data-kind="presentation.kind"
  >
    <!-- 类别药丸：带 #25 冻结的指标类型字面名，便于对回告警规则 -->
    <span
      class="inline-flex items-center rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-bold text-gray-700 dark:bg-dark-700 dark:text-gray-300"
      data-testid="zhipu-sign-alert-kind"
    >
      {{ t(`admin.ops.alertEvents.zhipuSign.kind.${presentation.kind}`) }}
    </span>

    <!-- 本地化标题：后端事件标题是单语字符串，L1/L2 归类后按界面语言渲染 -->
    <p
      class="text-xs font-semibold text-gray-900 dark:text-white"
      data-testid="zhipu-sign-alert-title"
    >
      {{ titleText }}
    </p>

    <!-- 当前值 / 目标 / 阈值（桌面与移动端同为等宽数值） -->
    <p
      class="font-mono text-xs text-gray-600 dark:text-gray-300"
      data-testid="zhipu-sign-alert-metric"
    >
      {{ metricText }}
    </p>

    <!-- 涉及账号：脱敏标识；完整 id 只在 title（不进入正文） -->
    <p
      v-if="presentation.accounts.length"
      class="text-[11px] text-gray-500 dark:text-gray-400"
      data-testid="zhipu-sign-alert-accounts"
    >
      {{ t('admin.ops.alertEvents.zhipuSign.accounts') }}
      <span
        v-for="account in presentation.accounts"
        :key="account.id"
        class="ml-1 font-mono"
        :title="String(account.id)"
        data-testid="zhipu-sign-alert-account"
      >
        #{{ account.masked }}
      </span>
    </p>

    <!-- 建议动作：沿用 #25 内置注册项文案（与告警邮件同源，只提配置键、不含凭据） -->
    <p
      v-if="presentation.suggestedAction"
      class="line-clamp-2 text-[11px] text-gray-500 dark:text-gray-400"
      data-testid="zhipu-sign-alert-action"
    >
      {{ t('admin.ops.alertEvents.zhipuSign.suggestedAction') }}
      {{ presentation.suggestedAction }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AlertEvent } from '@/api/admin/ops'
import { describeZhipuSignAlert } from '../utils/zhipuSignAlertPresentation'

/**
 * 智谱签名告警在既有 ops 告警行里的补充明细（ui-panels §6.2）。
 *
 * 复用现有告警中心的行/详情/静默能力，只补充本票要求的字段：
 * 类别（含 #25 指标类型字面名）、本地化标题、当前值/目标/阈值、涉及账号（脱敏标识）、
 * 建议动作。所有字段都来自事件自身（metric_value / threshold_value / dimensions /
 * description），字段缺失显示占位符而不臆造数值；本组件不提供任何操作入口
 * （静默/手动解决仍在既有详情弹窗）。
 */
const props = defineProps<{
  event: AlertEvent
}>()

const { t } = useI18n()

const presentation = computed(() => describeZhipuSignAlert(props.event))

const PLACEHOLDER = '-'

/** L1 计数取整；L2 系数固定两位小数；缺失为占位符（不用 0 冒充）。 */
function formatValue(value: number | null, kind: 'count' | 'rate'): string {
  if (value === null) return PLACEHOLDER
  if (kind === 'count') return Number.isInteger(value) ? String(value) : value.toFixed(2)
  return value.toFixed(2)
}

const titleText = computed(() => {
  const current = presentation.value
  if (!current) return ''
  if (current.kind === 'fail_window') {
    return t('admin.ops.alertEvents.zhipuSign.l1Title')
  }
  return t('admin.ops.alertEvents.zhipuSign.l2Title', {
    threshold: formatValue(current.threshold, 'rate'),
  })
})

const metricText = computed(() => {
  const current = presentation.value
  if (!current) return ''
  if (current.kind === 'fail_window') {
    return t('admin.ops.alertEvents.zhipuSign.windowCount', {
      value: formatValue(current.value, 'count'),
      threshold: formatValue(current.threshold, 'count'),
    })
  }
  return t('admin.ops.alertEvents.zhipuSign.effectiveRate', {
    value: formatValue(current.value, 'rate'),
    threshold: formatValue(current.threshold, 'rate'),
  })
})
</script>
