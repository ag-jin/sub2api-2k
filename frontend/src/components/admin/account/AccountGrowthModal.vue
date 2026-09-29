<template>
  <BaseDialog :show="show" :title="t('admin.accounts.codebuddy.growth.title')" width="wide" @close="handleClose">
    <div class="space-y-4">
      <p class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.codebuddy.growth.hint') }}
      </p>

      <!-- 批量动作：跑一轮自动通道 / 触发活跃上报 -->
      <div class="flex flex-wrap gap-2">
        <button
          :disabled="runAllRunning || activityRunning || loading"
          class="rounded-lg bg-primary-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-primary-700 disabled:cursor-not-allowed disabled:opacity-50"
          data-testid="growth-run-all"
          @click="handleRunAll"
        >
          {{ runAllRunning ? t('admin.accounts.codebuddy.growth.runAllRunning') : t('admin.accounts.codebuddy.growth.runAll') }}
        </button>
        <button
          :disabled="activityRunning || runAllRunning || loading"
          class="rounded-lg border border-gray-300 px-3 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50 dark:border-dark-500 dark:text-gray-200 dark:hover:bg-dark-700"
          data-testid="growth-activity-run"
          @click="handleActivityRun"
        >
          {{ activityRunning ? t('admin.accounts.codebuddy.growth.activityRunRunning') : t('admin.accounts.codebuddy.growth.activityRun') }}
        </button>
      </div>

      <!-- 通道列表 -->
      <div v-if="loading" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.codebuddy.growth.loading') }}
      </div>
      <div v-else-if="channels.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.codebuddy.growth.empty') }}
      </div>
      <div v-else class="space-y-2">
        <div
          v-for="channel in channels"
          :key="channel.key"
          class="rounded-xl border border-gray-200 p-3 dark:border-dark-500"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-mono text-sm font-semibold text-gray-900 dark:text-gray-100">
                  {{ channel.key }}
                </span>
                <!-- 性质徽标：如实呈现动作性质，不因授权而改变 -->
                <span
                  :class="[
                    'rounded-full px-2 py-0.5 text-[10px] font-medium',
                    tierBadgeClass(channel.tier)
                  ]"
                  data-testid="growth-tier"
                >
                  {{ t('admin.accounts.codebuddy.growth.tier.label') }}: {{ tierLabel(channel.tier) }}
                </span>
                <!-- 授权徽标：区分「性质上可自动」与「已授权放开」 -->
                <span
                  v-if="channel.auto_authorized"
                  class="rounded-full bg-amber-100 px-2 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-500/20 dark:text-amber-400"
                  data-testid="growth-auto-authorized"
                >
                  {{ t('admin.accounts.codebuddy.growth.autoAuthorized') }}
                </span>
                <span
                  v-else-if="channel.auto_runnable"
                  class="rounded-full bg-green-100 px-2 py-0.5 text-[10px] font-medium text-green-700 dark:bg-green-500/20 dark:text-green-400"
                  data-testid="growth-auto-runnable"
                >
                  {{ t('admin.accounts.codebuddy.growth.autoRunnable') }}
                </span>
                <span
                  v-else
                  class="rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-medium text-gray-600 dark:bg-dark-600 dark:text-gray-300"
                  data-testid="growth-manual-only"
                >
                  {{ t('admin.accounts.codebuddy.growth.manualOnly') }}
                </span>
              </div>
              <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
                {{ channel.rationale }}
              </p>
              <!-- 授权依据：让"为什么它能自动跑"可追溯，而不是靠记忆 -->
              <p
                v-if="channel.auto_authorization"
                class="mt-1 text-xs text-amber-600 dark:text-amber-400"
                data-testid="growth-authorization"
              >
                {{ t('admin.accounts.codebuddy.growth.authorizationHint') }}：{{ channel.auto_authorization }}
              </p>
            </div>
            <button
              :disabled="runningChannel !== '' || runAllRunning || activityRunning"
              class="shrink-0 rounded-lg border border-gray-300 px-3 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50 dark:border-dark-500 dark:text-gray-200 dark:hover:bg-dark-700"
              :data-testid="`growth-run-${channel.key}`"
              @click="handleRunChannel(channel.key)"
            >
              {{ runningChannel === channel.key ? t('admin.accounts.codebuddy.growth.runOneRunning') : t('admin.accounts.codebuddy.growth.runOne') }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import type { CodeBuddyGrowthChannel } from '@/api/admin/codebuddy'

const props = defineProps<{
  show: boolean
  accountId: number | null
  accountName?: string
}>()

const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const channels = ref<CodeBuddyGrowthChannel[]>([])
const runningChannel = ref('')
const runAllRunning = ref(false)
const activityRunning = ref(false)

/** 性质 → 徽标配色。full 用琥珀（提醒"需人担责"），不做成功色。 */
function tierBadgeClass(tier: CodeBuddyGrowthChannel['tier']): string {
  switch (tier) {
    case 'preview':
      return 'bg-sky-100 text-sky-700 dark:bg-sky-500/20 dark:text-sky-400'
    case 'claim':
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-400'
    default:
      return 'bg-amber-100 text-amber-700 dark:bg-amber-500/20 dark:text-amber-400'
  }
}

function tierLabel(tier: CodeBuddyGrowthChannel['tier']): string {
  return t(`admin.accounts.codebuddy.growth.tier.${tier}`)
}

async function loadChannels(): Promise<void> {
  loading.value = true
  try {
    const data = await adminAPI.codebuddy.growthChannels()
    channels.value = data.channels ?? []
  } catch (error) {
    console.error('Failed to load codebuddy growth channels:', error)
    appStore.showError(t('admin.accounts.codebuddy.growth.loadFailed'))
    channels.value = []
  } finally {
    loading.value = false
  }
}

async function handleRunChannel(channel: string): Promise<void> {
  if (!props.accountId || runningChannel.value) return
  runningChannel.value = channel
  try {
    const result = await adminAPI.codebuddy.growthRunChannel(props.accountId, channel)
    if (result.error) {
      appStore.showError(
        t('admin.accounts.codebuddy.growth.runOneFailed', { channel, error: result.error })
      )
    } else {
      appStore.showSuccess(t('admin.accounts.codebuddy.growth.runOneSuccess', { channel }))
    }
  } catch (error) {
    console.error('Failed to run codebuddy growth channel:', error)
    appStore.showError(
      t('admin.accounts.codebuddy.growth.runOneFailed', { channel, error: String(error) })
    )
  } finally {
    runningChannel.value = ''
  }
}

async function handleRunAll(): Promise<void> {
  if (runAllRunning.value) return
  runAllRunning.value = true
  try {
    const result = await adminAPI.codebuddy.growthRunAll()
    if (result.error) {
      appStore.showError(t('admin.accounts.codebuddy.growth.runAllFailed', { error: result.error }))
    } else {
      appStore.showSuccess(
        t('admin.accounts.codebuddy.growth.runAllSuccess', {
          succeeded: result.succeeded,
          skipped: result.skipped,
          failed: result.failed
        })
      )
    }
  } catch (error) {
    console.error('Failed to run codebuddy growth channels:', error)
    appStore.showError(t('admin.accounts.codebuddy.growth.runAllFailed', { error: String(error) }))
  } finally {
    runAllRunning.value = false
  }
}

async function handleActivityRun(): Promise<void> {
  if (activityRunning.value) return
  activityRunning.value = true
  try {
    const result = await adminAPI.codebuddy.growthActivityRun()
    if (result.error) {
      appStore.showError(
        t('admin.accounts.codebuddy.growth.activityRunFailed', { error: result.error })
      )
    } else {
      appStore.showSuccess(
        t('admin.accounts.codebuddy.growth.activityRunSuccess', {
          reported: result.reported,
          skipped: result.skipped,
          failed: result.failed
        })
      )
    }
  } catch (error) {
    console.error('Failed to trigger codebuddy activity report:', error)
    appStore.showError(
      t('admin.accounts.codebuddy.growth.activityRunFailed', { error: String(error) })
    )
  } finally {
    activityRunning.value = false
  }
}

function handleClose(): void {
  emit('close')
}

// 每次打开都重新拉通道列表：分级与授权是政策声明，可能随时被改动，
// 缓存住会让界面显示过期的"仅手动/已授权"结论。
//
// `immediate: true` 不是可选优化：组件被 `v-if`/父级条件渲染挂载时，
// `show` 在挂载那一刻已是 true，watch 的首次回调**不会**触发——
// 若只写普通 watch，面板打开后是空列表且不发请求（实测：用例里
// `growthChannels` 一次都没被调用）。带 immediate 才能在"打开即挂载"
// 与"已挂载后切换 show"两种父级写法下都正确加载。
watch(
  () => props.show,
  (visible) => {
    if (visible) {
      void loadChannels()
    }
  },
  { immediate: true }
)
</script>
