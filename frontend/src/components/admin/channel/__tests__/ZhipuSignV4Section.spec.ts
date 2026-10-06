/**
 * 票 29：渠道编辑/创建页「签名 V4」区块（票 28 契约的消费面）。
 *
 * 断言口径：
 * - 开关与降级策略的取值**只**来自接口（GET /admin/zhipu/sign/config），前端不硬编码默认值；
 * - 策略切换先确认再 PUT；保存失败（校验拒绝 / 无权限）不产生乐观更新；
 * - 状态区四态：加载中 / 接口错误 / 无数据（未握手）/ 有数据（含熔断中与未知）。
 *
 * vue-i18n 按仓库既有范式 mock 成 key 直出（附参数），因此文案断言落在 i18n key 上；
 * 文案本体与 zh/en 键完整性由 `src/i18n/__tests__/zhipuSignLocales.spec.ts` 覆盖。
 */
import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ZhipuSignV4Section from '../ZhipuSignV4Section.vue'
import type { ZhipuSignAccountStatus, ZhipuSignConfigView, ZhipuSignStatus } from '@/api/admin/zhipu'

const signApi = vi.hoisted(() => ({
  getSignConfig: vi.fn(),
  getSignStatus: vi.fn(),
  updateSignConfig: vi.fn(),
}))

vi.mock('@/api/admin/zhipu', () => ({
  getSignConfig: signApi.getSignConfig,
  getSignStatus: signApi.getSignStatus,
  updateSignConfig: signApi.updateSignConfig,
}))

vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}#${JSON.stringify(params)}` : key,
  }),
}))

const ERRORS = 'admin.channels.signV4.errors.'

/** 子组件按 name 查找（shallowMount 下与仓库既有用例一致）。 */
const TOGGLE = { name: 'Toggle' }
const CONFIRM_DIALOG = { name: 'ConfirmDialog' }

function configView(overrides: Partial<ZhipuSignConfigView> = {}): ZhipuSignConfigView {
  return {
    sign_v4_enabled: true,
    sign_client_version: '0.16.9',
    sign_pow_bits: 8,
    sign_key_ttl_minutes: 1440,
    sign_handshake_backoff_seconds: 30,
    sign_account_circuit_break_threshold: 10,
    sign_fail_policy: 'open',
    sign_alert_enabled: true,
    sign_reconcile_interval_hours: 6,
    sign_reconcile_deviation_threshold: 0.67,
    overridden_keys: [],
    ...overrides,
  }
}

function accountStatus(overrides: Partial<ZhipuSignAccountStatus> = {}): ZhipuSignAccountStatus {
  return {
    account_id: 7,
    account_name: 'zhipu-managed-a',
    key_cached: true,
    last_handshake_at: '2026-10-06T02:30:00Z',
    key_expires_at: '2026-10-07T02:30:00Z',
    consecutive_failures: 0,
    circuit_break_tripped: false,
    circuit_break_reason: '',
    runtime_state_available: true,
    ...overrides,
  }
}

function statusView(overrides: Partial<ZhipuSignStatus> = {}): ZhipuSignStatus {
  return {
    sign_v4_enabled: true,
    sign_fail_policy: 'open',
    accounts: [accountStatus()],
    ...overrides,
  }
}

async function mountSection(options: {
  config?: Partial<ZhipuSignConfigView> | null
  status?: Partial<ZhipuSignStatus> | null
} = {}) {
  if (options.config === null) {
    signApi.getSignConfig.mockReturnValue(new Promise(() => {}))
  } else {
    signApi.getSignConfig.mockResolvedValue(configView(options.config ?? {}))
  }
  if (options.status === null) {
    signApi.getSignStatus.mockReturnValue(new Promise(() => {}))
  } else {
    signApi.getSignStatus.mockResolvedValue(statusView(options.status ?? {}))
  }
  const wrapper = shallowMount(ZhipuSignV4Section)
  await flushPromises()
  return wrapper
}

function text(
  wrapper: { find: (selector: string) => { text: () => string } },
  testid: string,
): string {
  return wrapper.find(`[data-testid="${testid}"]`).text()
}

describe('ZhipuSignV4Section — 生效值只来自接口', () => {
  beforeEach(() => {
    signApi.getSignConfig.mockReset()
    signApi.getSignStatus.mockReset()
    signApi.updateSignConfig.mockReset()
  })

  it('renders the enabled state reported by the API (on)', async () => {
    const wrapper = await mountSection({ config: { sign_v4_enabled: true } })

    expect(wrapper.findComponent(TOGGLE).props('modelValue')).toBe(true)
    expect(text(wrapper, 'zhipu-sign-v4-effective')).toContain('admin.channels.signV4.effectiveOn')
  })

  it('renders the disabled state reported by the API (off)', async () => {
    const wrapper = await mountSection({ config: { sign_v4_enabled: false } })

    expect(wrapper.findComponent(TOGGLE).props('modelValue')).toBe(false)
    expect(text(wrapper, 'zhipu-sign-v4-effective')).toContain('admin.channels.signV4.effectiveOff')
  })

  it('renders no switch value at all before the API answers (no hardcoded default)', async () => {
    const wrapper = await mountSection({ config: null })

    expect(wrapper.find('[data-testid="zhipu-sign-v4-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="zhipu-sign-v4-effective"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="zhipu-sign-v4-config-loading"]').exists()).toBe(true)
  })

  it('states the scope as global flag AND account marker, and labels runtime overrides', async () => {
    const wrapper = await mountSection({
      config: { overridden_keys: ['gateway.zhipu.sign_v4_enabled'] },
    })

    const scope = text(wrapper, 'zhipu-sign-v4-scope')
    expect(scope).toContain('zcode_client_sign')
    expect(scope).toContain('sign_v4_enabled')
    expect(text(wrapper, 'zhipu-sign-v4-override')).toContain('admin.channels.signV4.overridden')
  })

  it('marks an inherited (non-overridden) value instead of claiming a runtime override', async () => {
    const wrapper = await mountSection({ config: { overridden_keys: [] } })

    expect(text(wrapper, 'zhipu-sign-v4-override')).toContain('admin.channels.signV4.inherited')
  })

  it('surfaces a config load error with a retry that re-queries the API', async () => {
    signApi.getSignConfig.mockRejectedValueOnce({ status: 500, message: 'boom' })
    signApi.getSignStatus.mockResolvedValue(statusView())
    const wrapper = shallowMount(ZhipuSignV4Section)
    await flushPromises()

    expect(wrapper.find('[data-testid="zhipu-sign-v4-config-error"]').exists()).toBe(true)
    expect(signApi.getSignConfig).toHaveBeenCalledTimes(1)

    signApi.getSignConfig.mockResolvedValueOnce(configView({ sign_v4_enabled: false }))
    await wrapper.find('[data-testid="zhipu-sign-v4-config-reload"]').trigger('click')
    await flushPromises()

    expect(signApi.getSignConfig).toHaveBeenCalledTimes(2)
    expect(wrapper.findComponent(TOGGLE).props('modelValue')).toBe(false)
  })
})

describe('ZhipuSignV4Section — 降级策略与费率影响文案', () => {
  beforeEach(() => {
    signApi.getSignConfig.mockReset()
    signApi.getSignStatus.mockReset()
    signApi.updateSignConfig.mockReset()
  })

  it('highlights fail-open with the 1.0-rate cost copy when the API reports open', async () => {
    const wrapper = await mountSection({ config: { sign_fail_policy: 'open' } })

    expect(wrapper.find('[data-testid="zhipu-sign-v4-policy-open"]').attributes('aria-pressed')).toBe('true')
    expect(wrapper.find('[data-testid="zhipu-sign-v4-policy-closed"]').attributes('aria-pressed')).toBe('false')
    expect(text(wrapper, 'zhipu-sign-v4-policy-impact')).toContain('admin.channels.signV4.policy.open.impact')
  })

  it('highlights fail-closed with its own cost copy when the API reports closed', async () => {
    const wrapper = await mountSection({ config: { sign_fail_policy: 'closed' } })

    expect(wrapper.find('[data-testid="zhipu-sign-v4-policy-closed"]').attributes('aria-pressed')).toBe('true')
    expect(text(wrapper, 'zhipu-sign-v4-policy-impact')).toContain('admin.channels.signV4.policy.closed.impact')
  })

  it('asks for confirmation before switching and sends nothing yet', async () => {
    const wrapper = await mountSection({ config: { sign_fail_policy: 'open' } })

    await wrapper.find('[data-testid="zhipu-sign-v4-policy-closed"]').trigger('click')

    expect(signApi.updateSignConfig).not.toHaveBeenCalled()
    const dialog = wrapper.findComponent(CONFIRM_DIALOG)
    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('title')).toBe('admin.channels.signV4.policy.confirmTitle')
    expect(String(dialog.props('message'))).toContain('admin.channels.signV4.policy.confirmMessage')
    expect(String(dialog.props('message'))).toContain('admin.channels.signV4.policy.closed.impact')
    expect(wrapper.find('[data-testid="zhipu-sign-v4-policy-open"]').attributes('aria-pressed')).toBe('true')
  })

  it('cancels the switch without writing and keeps the server value selected', async () => {
    const wrapper = await mountSection({ config: { sign_fail_policy: 'open' } })

    await wrapper.find('[data-testid="zhipu-sign-v4-policy-closed"]').trigger('click')
    wrapper.findComponent(CONFIRM_DIALOG).vm.$emit('cancel')
    await flushPromises()

    expect(signApi.updateSignConfig).not.toHaveBeenCalled()
    expect(wrapper.findComponent(CONFIRM_DIALOG).props('show')).toBe(false)
    expect(wrapper.find('[data-testid="zhipu-sign-v4-policy-open"]').attributes('aria-pressed')).toBe('true')
    expect(text(wrapper, 'zhipu-sign-v4-policy-impact')).toContain('admin.channels.signV4.policy.open.impact')
  })

  it('writes the confirmed policy and adopts the effective value returned by the server', async () => {
    const wrapper = await mountSection({ config: { sign_fail_policy: 'open' } })
    signApi.updateSignConfig.mockResolvedValue(configView({ sign_fail_policy: 'closed', overridden_keys: ['gateway.zhipu.sign_fail_policy'] }))

    await wrapper.find('[data-testid="zhipu-sign-v4-policy-closed"]').trigger('click')
    wrapper.findComponent(CONFIRM_DIALOG).vm.$emit('confirm')
    await flushPromises()

    expect(signApi.updateSignConfig).toHaveBeenCalledWith({ sign_fail_policy: 'closed' })
    expect(wrapper.find('[data-testid="zhipu-sign-v4-policy-closed"]').attributes('aria-pressed')).toBe('true')
    expect(text(wrapper, 'zhipu-sign-v4-policy-impact')).toContain('admin.channels.signV4.policy.closed.impact')
    expect(wrapper.find('[data-testid="zhipu-sign-v4-feedback-success"]').exists()).toBe(true)
  })
})

describe('ZhipuSignV4Section — 总开关保存与错误分支', () => {
  beforeEach(() => {
    signApi.getSignConfig.mockReset()
    signApi.getSignStatus.mockReset()
    signApi.updateSignConfig.mockReset()
  })

  it('sends the toggle patch and adopts the value returned by the server', async () => {
    const wrapper = await mountSection({ config: { sign_v4_enabled: true } })
    signApi.updateSignConfig.mockResolvedValue(configView({ sign_v4_enabled: false }))

    wrapper.findComponent(TOGGLE).vm.$emit('update:modelValue', false)
    await flushPromises()

    expect(signApi.updateSignConfig).toHaveBeenCalledWith({ sign_v4_enabled: false })
    expect(wrapper.findComponent(TOGGLE).props('modelValue')).toBe(false)
    expect(text(wrapper, 'zhipu-sign-v4-effective')).toContain('admin.channels.signV4.effectiveOff')
    expect(wrapper.find('[data-testid="zhipu-sign-v4-feedback-success"]').exists()).toBe(true)
  })

  it('keeps the server value when the backend rejects the update (no optimistic flip)', async () => {
    const wrapper = await mountSection({ config: { sign_v4_enabled: true } })
    signApi.updateSignConfig.mockRejectedValueOnce({ status: 400, reason: 'ZHIPU_SIGN_CONFIG_INVALID' })

    wrapper.findComponent(TOGGLE).vm.$emit('update:modelValue', false)
    await flushPromises()

    expect(wrapper.findComponent(TOGGLE).props('modelValue')).toBe(true)
    expect(text(wrapper, 'zhipu-sign-v4-effective')).toContain('admin.channels.signV4.effectiveOn')
    expect(text(wrapper, 'zhipu-sign-v4-feedback-error')).toContain(`${ERRORS}ZHIPU_SIGN_CONFIG_INVALID`)
    expect(wrapper.find('[data-testid="zhipu-sign-v4-feedback-success"]').exists()).toBe(false)
  })

  it('reports a permission failure (403) and keeps the server value', async () => {
    const wrapper = await mountSection({ config: { sign_v4_enabled: true } })
    signApi.updateSignConfig.mockRejectedValueOnce({ status: 403, message: 'forbidden' })

    wrapper.findComponent(TOGGLE).vm.$emit('update:modelValue', false)
    await flushPromises()

    expect(wrapper.findComponent(TOGGLE).props('modelValue')).toBe(true)
    expect(text(wrapper, 'zhipu-sign-v4-feedback-error')).toContain(`${ERRORS}forbidden`)
  })

  it('falls back to a generic message when the backend only reports an unavailable store', async () => {
    const wrapper = await mountSection({ config: { sign_v4_enabled: true } })
    signApi.updateSignConfig.mockRejectedValueOnce({ status: 500, reason: 'ZHIPU_SIGN_CONFIG_UNAVAILABLE' })

    wrapper.findComponent(TOGGLE).vm.$emit('update:modelValue', false)
    await flushPromises()

    expect(text(wrapper, 'zhipu-sign-v4-feedback-error')).toContain(`${ERRORS}ZHIPU_SIGN_CONFIG_UNAVAILABLE`)
  })

  it('never renders a submit-able button inside the channel form', async () => {
    const wrapper = await mountSection()

    const buttons = wrapper.findAll('button')
    expect(buttons.length).toBeGreaterThan(0)
    for (const button of buttons) {
      expect(button.attributes('type')).toBe('button')
    }
  })
})

describe('ZhipuSignV4Section — 握手私钥状态四态', () => {
  beforeEach(() => {
    signApi.getSignConfig.mockReset()
    signApi.getSignStatus.mockReset()
    signApi.updateSignConfig.mockReset()
  })

  it('shows the loading state while the status request is in flight', async () => {
    const wrapper = await mountSection({ status: null })

    expect(wrapper.find('[data-testid="zhipu-sign-v4-status-loading"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="zhipu-sign-v4-status-empty"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="zhipu-sign-v4-account"]')).toHaveLength(0)
  })

  it('shows the error state and re-queries the status on retry', async () => {
    signApi.getSignConfig.mockResolvedValue(configView())
    signApi.getSignStatus.mockRejectedValueOnce({ status: 500, message: 'boom' })
    const wrapper = shallowMount(ZhipuSignV4Section)
    await flushPromises()

    expect(wrapper.find('[data-testid="zhipu-sign-v4-status-error"]').exists()).toBe(true)
    expect(text(wrapper, 'zhipu-sign-v4-status-error')).toContain('admin.channels.signV4.status.error')

    signApi.getSignStatus.mockResolvedValueOnce(statusView())
    await wrapper.find('[data-testid="zhipu-sign-v4-status-retry"]').trigger('click')
    await flushPromises()

    expect(signApi.getSignStatus).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="zhipu-sign-v4-status-error"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="zhipu-sign-v4-account"]')).toHaveLength(1)
  })

  it('shows the empty state when no account has signing enabled', async () => {
    const wrapper = await mountSection({ status: { accounts: [] } })

    expect(wrapper.find('[data-testid="zhipu-sign-v4-status-empty"]').exists()).toBe(true)
    expect(text(wrapper, 'zhipu-sign-v4-status-empty')).toContain('admin.channels.signV4.status.empty')
    expect(wrapper.findAll('[data-testid="zhipu-sign-v4-account"]')).toHaveLength(0)
  })

  it('renders cache hit, last handshake and consecutive failures per account', async () => {
    const wrapper = await mountSection({
      status: {
        accounts: [
          accountStatus(),
          accountStatus({
            account_id: 9,
            account_name: 'zhipu-managed-b',
            key_cached: false,
            last_handshake_at: null,
            key_expires_at: null,
            consecutive_failures: 3,
          }),
        ],
      },
    })

    const rows = wrapper.findAll('[data-testid="zhipu-sign-v4-account"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-testid="zhipu-sign-v4-key-cached"]').text()).toContain('signV4.status.keyCachedYes')
    expect(rows[0].find('[data-testid="zhipu-sign-v4-last-handshake"]').text()).not.toContain('signV4.status.neverHandshake')
    expect(rows[1].find('[data-testid="zhipu-sign-v4-key-cached"]').text()).toContain('signV4.status.keyCachedNo')
    expect(rows[1].find('[data-testid="zhipu-sign-v4-last-handshake"]').text()).toContain('signV4.status.neverHandshake')
    expect(rows[1].find('[data-testid="zhipu-sign-v4-expires"]').text()).toContain('signV4.status.notAvailable')
    expect(rows[1].find('[data-testid="zhipu-sign-v4-failures"]').text()).toContain('3')
  })

  it('marks a circuit-broken account as removed from signing and shows the stable reason', async () => {
    const wrapper = await mountSection({
      status: {
        accounts: [
          accountStatus({
            circuit_break_tripped: true,
            circuit_break_reason: 'verify_invalid_over_threshold',
          }),
        ],
      },
    })

    const row = wrapper.find('[data-testid="zhipu-sign-v4-account"]')
    expect(row.find('[data-testid="zhipu-sign-v4-circuit-break"]').text()).toContain('signV4.status.circuitBreakTripped')
    expect(row.find('[data-testid="zhipu-sign-v4-circuit-break-reason"]').text()).toContain('verify_invalid_over_threshold')
  })

  it('reports "unknown" (not "normal") when the runtime state source is not wired', async () => {
    const wrapper = await mountSection({
      status: { accounts: [accountStatus({ runtime_state_available: false })] },
    })

    const circuitBreak = wrapper.find('[data-testid="zhipu-sign-v4-circuit-break"]').text()
    expect(circuitBreak).toContain('signV4.status.circuitBreakUnknown')
    expect(circuitBreak).not.toContain('signV4.status.circuitBreakNone')
  })
})
