import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const {
  qrStartMock,
  qrPollMock,
  createAccountMock,
  showSuccessMock,
  showErrorMock,
  toCanvasMock,
} = vi.hoisted(() => ({
  qrStartMock: vi.fn(),
  qrPollMock: vi.fn(),
  createAccountMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn(),
  toCanvasMock: vi.fn(),
}))

vi.mock('qrcode', () => ({
  default: {
    toCanvas: toCanvasMock,
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
    showWarning: vi.fn(),
    showInfo: vi.fn(),
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isSimpleMode: true,
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      probeUpstreamBilling: vi.fn(),
      syncUpstreamModels: vi.fn(),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: vi.fn(),
      createOpenAICodexPAT: vi.fn(),
    },
    codebuddy: {
      qrStart: qrStartMock,
      qrPoll: qrPollMock,
      checkin: vi.fn(),
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

function mountModal() {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: true,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: true,
        ModelWhitelistSelector: true,
        QuotaLimitCard: true,
        Toggle: true,
      },
    },
  })
}

async function openCodebuddyCreate() {
  const wrapper = mountModal()
  await wrapper.get('[data-testid="codebuddy-platform-button"]').trigger('click')
  return wrapper
}

const AUTH_JSON = JSON.stringify({
  auth: { accessToken: 'tok', refreshToken: 'rt' },
  account: { uid: 'u1' },
})

describe('CreateAccountModal CodeBuddy 扫码双态（A1）', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    qrStartMock.mockReset().mockResolvedValue({ state: 'st-1', authUrl: 'https://auth.example.com/qr' })
    qrPollMock.mockReset().mockResolvedValue({ status: 'waiting' })
    createAccountMock.mockReset().mockResolvedValue({ id: 77, platform: 'codebuddy', type: 'apikey' })
    showSuccessMock.mockReset()
    showErrorMock.mockReset()
    toCanvasMock.mockReset().mockResolvedValue(undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('默认落在粘贴态：粘贴输入与 enterpriseId 可达，扫码面板不渲染', async () => {
    const wrapper = await openCodebuddyCreate()

    expect(wrapper.find('[data-testid="codebuddy-auth-json"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="codebuddy-enterprise-id"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="codebuddy-qr-panel"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="codebuddy-cred-tab-qr"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('切到扫码态：隐藏粘贴输入与 enterpriseId，点开始后拉取 qr/start 并用 qrcode 渲染 canvas', async () => {
    const wrapper = await openCodebuddyCreate()

    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-auth-json"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="codebuddy-enterprise-id"]').exists()).toBe(false)

    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    expect(qrStartMock).toHaveBeenCalledTimes(1)
    expect(toCanvasMock).toHaveBeenCalledTimes(1)
    expect(toCanvasMock.mock.calls[0]?.[1]).toBe('https://auth.example.com/qr')
    expect(wrapper.find('[data-testid="codebuddy-qr-canvas"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.codebuddy.qr.waiting')
    wrapper.unmount()
  })

  it('qr/start 失败时进入错误态并可通过重试重新 start', async () => {
    qrStartMock.mockRejectedValueOnce(new Error('boom'))
    const wrapper = await openCodebuddyCreate()

    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-qr-error"]').exists()).toBe(true)

    await wrapper.get('[data-testid="codebuddy-qr-retry"]').trigger('click')
    await flushPromises()

    // 重试必须重新 qr/start（新 state）
    expect(qrStartMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="codebuddy-qr-canvas"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('轮询状态机 waiting→ok：关弹窗并触发父列表刷新，不回表单二次提交', async () => {
    qrPollMock
      .mockResolvedValueOnce({ status: 'waiting' })
      .mockResolvedValueOnce({ status: 'ok', account: { id: 9, uid: 'u-9', created: true } })

    const wrapper = await openCodebuddyCreate()
    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    expect(qrPollMock).toHaveBeenCalledTimes(1)
    expect(qrPollMock.mock.calls[0]?.[0]).toBe('st-1')
    expect(wrapper.emitted('created')).toBeUndefined()

    await vi.advanceTimersByTimeAsync(4000)
    await flushPromises()

    expect(qrPollMock).toHaveBeenCalledTimes(2)
    expect(wrapper.emitted('created')).toHaveLength(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
    expect(showSuccessMock).toHaveBeenCalledWith('admin.accounts.codebuddy.qr.success')
    // 不回表单二次提交
    expect(createAccountMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('轮询 2s→4s 退避', async () => {
    const wrapper = await openCodebuddyCreate()
    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(1999)
    expect(qrPollMock).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(1)
    expect(qrPollMock).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(3000)
    expect(qrPollMock).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1000)
    expect(qrPollMock).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('expired 后重试会重新 qr/start', async () => {
    qrPollMock.mockResolvedValueOnce({ status: 'expired' })
    const wrapper = await openCodebuddyCreate()
    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-qr-expired"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="codebuddy-qr-canvas"]').exists()).toBe(false)

    await wrapper.get('[data-testid="codebuddy-qr-retry"]').trigger('click')
    await flushPromises()

    expect(qrStartMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="codebuddy-qr-canvas"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('poll 返回 error 态时显示轮询失败文案', async () => {
    qrPollMock.mockResolvedValueOnce({ status: 'error' })
    const wrapper = await openCodebuddyCreate()
    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-qr-error"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="codebuddy-qr-retry"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('unmount 即停：不再发出后续轮询', async () => {
    const wrapper = await openCodebuddyCreate()
    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    expect(qrPollMock).toHaveBeenCalledTimes(1)

    wrapper.unmount()

    await vi.advanceTimersByTimeAsync(300_000)
    expect(qrPollMock).toHaveBeenCalledTimes(1)
  })

  it('切回粘贴态即停轮询并恢复粘贴输入', async () => {
    const wrapper = await openCodebuddyCreate()
    await wrapper.get('[data-testid="codebuddy-cred-tab-qr"]').trigger('click')
    await wrapper.get('[data-testid="codebuddy-qr-start"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    expect(qrPollMock).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-testid="codebuddy-cred-tab-paste"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(300_000)
    expect(qrPollMock).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-testid="codebuddy-auth-json"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('回归：粘贴路径 testid 与校验文案零改动（空提交仍走 authJsonRequired 拦截）', async () => {
    const wrapper = await openCodebuddyCreate()

    await wrapper.get('form#create-account-form input[type="text"]').setValue('CB account')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.codebuddy.authJsonRequired')
    expect(createAccountMock).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="codebuddy-auth-json"]').setValue(AUTH_JSON)
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      base_url: 'https://copilot.tencent.com',
      auth: { accessToken: 'tok' },
    })
    wrapper.unmount()
  })
})
