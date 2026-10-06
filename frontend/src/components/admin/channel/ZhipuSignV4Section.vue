<template>
  <div
    data-testid="zhipu-sign-v4-section"
    class="space-y-3 rounded-lg border border-gray-200 bg-gray-50/50 p-3 dark:border-dark-700 dark:bg-dark-900/30"
  >
    <div class="flex items-start justify-between gap-3">
      <div>
        <h4 class="text-xs font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.channels.signV4.title') }}
        </h4>
        <p class="mt-0.5 text-[11px] text-gray-500 dark:text-gray-400">
          {{ t('admin.channels.signV4.subtitle') }}
        </p>
      </div>
      <!-- 生效值徽标：取值一律来自接口（前端不硬编码默认值）。 -->
      <span
        v-if="configLoaded"
        data-testid="zhipu-sign-v4-effective"
        class="flex-none rounded px-1.5 py-0.5 text-[11px] font-medium"
        :class="signEnabled
          ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
          : 'bg-gray-200 text-gray-600 dark:bg-dark-600 dark:text-gray-300'"
      >
        {{ t('admin.channels.signV4.effective') }}:
        {{ signEnabled ? t('admin.channels.signV4.effectiveOn') : t('admin.channels.signV4.effectiveOff') }}
      </span>
    </div>

    <!-- 配置加载中 -->
    <p
      v-if="configLoading"
      data-testid="zhipu-sign-v4-config-loading"
      class="text-[11px] text-gray-500 dark:text-gray-400"
    >
      {{ t('admin.channels.signV4.loading') }}
    </p>

    <!-- 配置读取失败：不渲染任何开关，避免用本地默认值误导运营 -->
    <div v-else-if="configFailed" class="space-y-2">
      <p data-testid="zhipu-sign-v4-config-error" class="text-[11px] text-red-600 dark:text-red-400">
        {{ t('admin.channels.signV4.errors.configFailed') }}
        <span v-if="configErrorDetail" class="text-gray-500 dark:text-gray-400">{{ configErrorDetail }}</span>
      </p>
      <button
        type="button"
        data-testid="zhipu-sign-v4-config-reload"
        class="btn btn-secondary text-xs"
        :disabled="configLoading"
        @click="loadConfig"
      >
        {{ t('admin.channels.signV4.status.retry') }}
      </button>
    </div>

    <template v-else-if="configLoaded">
      <!-- 作用域：全局开关 ∧ 账号级标记（标识符保持语言中立，文案本地化） -->
      <p
        data-testid="zhipu-sign-v4-scope"
        class="flex flex-wrap items-center gap-1 text-[11px] text-gray-500 dark:text-gray-400"
      >
        <span>{{ t('admin.channels.signV4.scope') }}</span>
        <code class="rounded bg-gray-100 px-1 font-mono text-[10px] dark:bg-dark-700">gateway.zhipu.sign_v4_enabled</code>
        <span>+</span>
        <code class="rounded bg-gray-100 px-1 font-mono text-[10px] dark:bg-dark-700">zcode_client_sign=v4</code>
      </p>

      <!-- 总开关 -->
      <div class="flex items-start justify-between gap-3 border-t border-gray-200 pt-3 dark:border-dark-600">
        <div>
          <label class="text-xs font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.channels.signV4.toggleLabel') }}
          </label>
          <p class="mt-0.5 text-[11px] text-gray-500 dark:text-gray-400">
            {{ t('admin.channels.signV4.toggleHint') }}
          </p>
          <p
            data-testid="zhipu-sign-v4-override"
            class="mt-0.5 text-[11px] text-gray-400 dark:text-dark-400"
          >
            {{ signV4Overridden
              ? t('admin.channels.signV4.overridden')
              : t('admin.channels.signV4.inherited') }}
          </p>
          <p class="mt-0.5 text-[11px] text-gray-400 dark:text-dark-400">
            {{ t('admin.channels.signV4.auditHint') }}
          </p>
        </div>
        <Toggle
          :model-value="signEnabled"
          :disabled="saving"
          data-testid="zhipu-sign-v4-toggle"
          @update:model-value="onToggle"
        />
      </div>

      <!-- 失败策略：降级即 1.0 系数（成本约 ×1.5）；切换需二次确认 -->
      <div class="border-t border-gray-200 pt-3 dark:border-dark-600">
        <label class="text-xs font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.channels.signV4.policy.label') }}
        </label>
        <p class="mt-0.5 text-[11px] text-gray-500 dark:text-gray-400">
          {{ t('admin.channels.signV4.policy.hint') }}
        </p>
        <div class="mt-2 grid gap-2 sm:grid-cols-2">
          <button
            v-for="option in POLICY_OPTIONS"
            :key="option"
            type="button"
            :data-testid="`zhipu-sign-v4-policy-${option}`"
            :aria-pressed="failPolicy === option"
            :disabled="saving"
            class="rounded-lg border-2 px-3 py-2 text-left transition-colors disabled:cursor-not-allowed disabled:opacity-60"
            :class="failPolicy === option
              ? 'border-primary-500 bg-primary-50 dark:border-primary-700 dark:bg-primary-900/20'
              : 'border-gray-200 hover:border-gray-300 dark:border-dark-600 dark:hover:border-dark-500'"
            @click="requestPolicyChange(option)"
          >
            <span class="block text-xs font-semibold text-gray-800 dark:text-gray-200">
              {{ t(`admin.channels.signV4.policy.${option}.title`) }}
            </span>
            <span class="mt-0.5 block text-[11px] text-gray-500 dark:text-gray-400">
              {{ t(`admin.channels.signV4.policy.${option}.desc`) }}
            </span>
          </button>
        </div>
        <p
          data-testid="zhipu-sign-v4-policy-impact"
          class="mt-2 text-[11px] text-amber-600 dark:text-amber-400"
        >
          {{ t(`admin.channels.signV4.policy.${failPolicy}.impact`) }}
        </p>
        <p class="mt-1 text-[11px] text-gray-400 dark:text-dark-400">
          {{ policyOverridden
            ? t('admin.channels.signV4.overridden')
            : t('admin.channels.signV4.inherited') }}
        </p>
      </div>

      <!-- 握手私钥状态：加载中 / 接口错误 / 无数据（未握手）/ 有数据（含熔断中与未知） -->
      <div class="border-t border-gray-200 pt-3 dark:border-dark-600">
        <div class="flex items-center justify-between gap-2">
          <span class="text-xs font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.channels.signV4.status.title') }}
          </span>
          <button
            type="button"
            data-testid="zhipu-sign-v4-status-retry"
            class="text-[11px] text-primary-600 hover:text-primary-700 dark:text-primary-400"
            :disabled="statusLoading"
            @click="loadStatus"
          >
            {{ t('admin.channels.signV4.status.retry') }}
          </button>
        </div>
        <p class="mt-0.5 text-[11px] text-gray-500 dark:text-gray-400">
          {{ t('admin.channels.signV4.status.hint') }}
        </p>

        <p
          v-if="statusLoading"
          data-testid="zhipu-sign-v4-status-loading"
          class="mt-2 text-[11px] text-gray-500 dark:text-gray-400"
        >
          {{ t('admin.channels.signV4.status.loading') }}
        </p>

        <p
          v-else-if="statusFailed"
          data-testid="zhipu-sign-v4-status-error"
          class="mt-2 text-[11px] text-red-600 dark:text-red-400"
        >
          {{ t('admin.channels.signV4.status.error') }}
          <span v-if="statusErrorDetail" class="text-gray-500 dark:text-gray-400">{{ statusErrorDetail }}</span>
        </p>

        <p
          v-else-if="accounts.length === 0"
          data-testid="zhipu-sign-v4-status-empty"
          class="mt-2 text-[11px] text-gray-500 dark:text-gray-400"
        >
          {{ t('admin.channels.signV4.status.empty') }}
        </p>

        <ul v-else class="mt-2 space-y-2">
          <li
            v-for="account in accounts"
            :key="account.account_id"
            data-testid="zhipu-sign-v4-account"
            class="rounded border border-gray-200 bg-white p-2 dark:border-dark-600 dark:bg-dark-800"
          >
            <div class="flex items-center justify-between gap-2">
              <span class="truncate text-xs font-medium text-gray-800 dark:text-gray-200">
                {{ account.account_name }}
                <span class="text-gray-400">#{{ account.account_id }}</span>
              </span>
              <span
                data-testid="zhipu-sign-v4-circuit-break"
                class="flex-none rounded px-1.5 py-0.5 text-[10px] font-medium"
                :class="circuitBreakClass(account)"
              >
                {{ circuitBreakLabel(account) }}
              </span>
            </div>
            <div class="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-gray-500 dark:text-gray-400">
              <span data-testid="zhipu-sign-v4-key-cached">
                {{ t('admin.channels.signV4.status.keyCached') }}:
                {{ account.key_cached
                  ? t('admin.channels.signV4.status.keyCachedYes')
                  : t('admin.channels.signV4.status.keyCachedNo') }}
              </span>
              <span data-testid="zhipu-sign-v4-last-handshake">
                {{ t('admin.channels.signV4.status.lastHandshake') }}:
                {{ account.last_handshake_at
                  ? formatDateTimeToMinute(account.last_handshake_at)
                  : t('admin.channels.signV4.status.neverHandshake') }}
              </span>
              <span data-testid="zhipu-sign-v4-expires">
                {{ t('admin.channels.signV4.status.keyExpires') }}:
                {{ account.key_expires_at
                  ? formatDateTimeToMinute(account.key_expires_at)
                  : t('admin.channels.signV4.status.notAvailable') }}
              </span>
              <span data-testid="zhipu-sign-v4-failures">
                {{ t('admin.channels.signV4.status.consecutiveFailures') }}: {{ account.consecutive_failures }}
              </span>
            </div>
            <p
              v-if="account.circuit_break_tripped && account.circuit_break_reason"
              data-testid="zhipu-sign-v4-circuit-break-reason"
              class="mt-1 font-mono text-[10px] text-amber-600 dark:text-amber-400"
            >
              {{ t('admin.channels.signV4.status.reasonLabel') }}: {{ account.circuit_break_reason }}
            </p>
          </li>
        </ul>
      </div>

      <!-- 保存反馈（成功 / 校验拒绝 / 无权限 / 存储不可用） -->
      <p
        v-if="feedback && feedback.kind === 'success'"
        data-testid="zhipu-sign-v4-feedback-success"
        class="text-[11px] text-emerald-600 dark:text-emerald-400"
      >
        {{ feedback.message }}
      </p>
      <p
        v-else-if="feedback"
        data-testid="zhipu-sign-v4-feedback-error"
        class="text-[11px] text-red-600 dark:text-red-400"
      >
        {{ feedback.message }}
      </p>
      <p v-if="saving" data-testid="zhipu-sign-v4-saving" class="text-[11px] text-gray-500 dark:text-gray-400">
        {{ t('admin.channels.signV4.saving') }}
      </p>
    </template>

    <!-- 策略切换二次确认：不确认则不写库（避免误改全局费率语义） -->
    <ConfirmDialog
      :show="pendingPolicy !== null"
      :title="t('admin.channels.signV4.policy.confirmTitle')"
      :message="policyConfirmMessage"
      :confirm-text="t('admin.channels.signV4.policy.confirm')"
      :cancel-text="t('admin.channels.signV4.policy.cancel')"
      @confirm="confirmPolicyChange"
      @cancel="cancelPolicyChange"
    />
  </div>
</template>

<script setup lang="ts">
/**
 * 渠道编辑/创建页的「签名 V4」区块（票 29；后端契约见票 28）。
 *
 * 三条不变量：
 *  1. 开关与降级策略的取值**只**来自 `GET /admin/zhipu/sign/config`，前端不硬编码默认值
 *     （M3 与 M6 的默认口径冲突由后端生效值裁决）；
 *  2. 写操作一律「先请求后落地」：只有后端返回的生效视图才更新本地状态，保存失败不产生
 *     乐观更新（校验拒绝 / 无权限 / 存储不可用时界面保持服务端值）；
 *  3. 状态区如实区分「未知」与「正常」：`runtime_state_available=false` 不得渲染成未熔断。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import { extractApiErrorMessage, extractI18nErrorMessage } from '@/utils/apiError'
import { formatDateTimeToMinute } from '@/utils/format'
import {
  getSignConfig,
  getSignStatus,
  updateSignConfig,
  type ZhipuSignAccountStatus,
  type ZhipuSignConfigUpdate,
  type ZhipuSignConfigView,
  type ZhipuSignFailPolicy,
  type ZhipuSignStatus,
} from '@/api/admin/zhipu'

const { t } = useI18n()

const POLICY_OPTIONS: ZhipuSignFailPolicy[] = ['open', 'closed']
const ERRORS = 'admin.channels.signV4.errors'

const config = ref<ZhipuSignConfigView | null>(null)
const configLoading = ref(true)
const configFailed = ref(false)
const configErrorDetail = ref('')
const status = ref<ZhipuSignStatus | null>(null)
const statusLoading = ref(true)
const statusFailed = ref(false)
const statusErrorDetail = ref('')
const saving = ref(false)
const feedback = ref<{ kind: 'success' | 'error'; message: string } | null>(null)
const pendingPolicy = ref<ZhipuSignFailPolicy | null>(null)

const configLoaded = computed(() => config.value !== null)
const signEnabled = computed(() => config.value?.sign_v4_enabled === true)
const failPolicy = computed<ZhipuSignFailPolicy>(() =>
  config.value?.sign_fail_policy === 'closed' ? 'closed' : 'open'
)
const signV4Overridden = computed(() =>
  (config.value?.overridden_keys ?? []).includes('gateway.zhipu.sign_v4_enabled')
)
const policyOverridden = computed(() =>
  (config.value?.overridden_keys ?? []).includes('gateway.zhipu.sign_fail_policy')
)
const accounts = computed<ZhipuSignAccountStatus[]>(() => status.value?.accounts ?? [])

const policyConfirmMessage = computed(() =>
  t('admin.channels.signV4.policy.confirmMessage', {
    policy: pendingPolicy.value
      ? t(`admin.channels.signV4.policy.${pendingPolicy.value}.title`)
      : '',
    impact: pendingPolicy.value
      ? t(`admin.channels.signV4.policy.${pendingPolicy.value}.impact`)
      : '',
  })
)

/** 403/401 单独给出「无权限」文案；其余按 backend reason 映射到 errors 命名空间。 */
function describeError(err: unknown, fallbackKey: string): string {
  const httpStatus = (err as { status?: number } | null | undefined)?.status
  if (httpStatus === 401 || httpStatus === 403) return t(`${ERRORS}.forbidden`)
  return extractI18nErrorMessage(err, t, ERRORS, t(fallbackKey))
}

async function loadConfig(): Promise<void> {
  configLoading.value = true
  configFailed.value = false
  configErrorDetail.value = ''
  try {
    config.value = await getSignConfig()
  } catch (err) {
    // 读取失败时不保留上一次的视图：宁可显示「读取失败」也不要让运营看到过期生效值。
    config.value = null
    configFailed.value = true
    configErrorDetail.value = extractApiErrorMessage(err, '')
  } finally {
    configLoading.value = false
  }
}

async function loadStatus(): Promise<void> {
  statusLoading.value = true
  statusFailed.value = false
  statusErrorDetail.value = ''
  try {
    status.value = await getSignStatus()
  } catch (err) {
    status.value = null
    statusFailed.value = true
    statusErrorDetail.value = extractApiErrorMessage(err, '')
  } finally {
    statusLoading.value = false
  }
}

async function applyUpdate(patch: ZhipuSignConfigUpdate, successKey: string): Promise<void> {
  if (saving.value) return
  saving.value = true
  feedback.value = null
  try {
    // 只在服务端确认后落地：返回值是本次写入后的生效视图。
    config.value = await updateSignConfig(patch)
    feedback.value = { kind: 'success', message: t(successKey) }
  } catch (err) {
    feedback.value = { kind: 'error', message: describeError(err, `${ERRORS}.saveFailed`) }
  } finally {
    saving.value = false
  }
}

function onToggle(next: boolean): void {
  if (saving.value || next === signEnabled.value) return
  void applyUpdate({ sign_v4_enabled: next }, 'admin.channels.signV4.save.success')
}

/** 单选只在确认后提交：未确认的点击不改本地选中态，也不发请求。 */
function requestPolicyChange(policy: ZhipuSignFailPolicy): void {
  if (saving.value || policy === failPolicy.value) return
  pendingPolicy.value = policy
}

function cancelPolicyChange(): void {
  pendingPolicy.value = null
}

async function confirmPolicyChange(): Promise<void> {
  const policy = pendingPolicy.value
  pendingPolicy.value = null
  if (!policy) return
  await applyUpdate({ sign_fail_policy: policy }, 'admin.channels.signV4.save.policySuccess')
}

function circuitBreakLabel(account: ZhipuSignAccountStatus): string {
  if (!account.runtime_state_available) return t('admin.channels.signV4.status.circuitBreakUnknown')
  if (account.circuit_break_tripped) return t('admin.channels.signV4.status.circuitBreakTripped')
  return t('admin.channels.signV4.status.circuitBreakNone')
}

function circuitBreakClass(account: ZhipuSignAccountStatus): string {
  if (!account.runtime_state_available) {
    return 'bg-gray-200 text-gray-600 dark:bg-dark-600 dark:text-gray-300'
  }
  if (account.circuit_break_tripped) {
    return 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
  }
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
}

onMounted(() => {
  void loadConfig()
  void loadStatus()
})
</script>
